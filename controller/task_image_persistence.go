package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/model"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/service"
)

type taskImageProvider interface {
	relaychannel.TaskArtifactProvider
	relaychannel.TaskContentRequestProvider
}

// PersistTaskImages stores the completed provider result before the polling
// owner commits SUCCESS. It never submits work or changes the task status.
func PersistTaskImages(ctx context.Context, task *model.Task) error {
	store := service.GetTaskArtifactStore()
	if !store.Enabled() {
		return service.ErrTaskArtifactStoreDisabled
	}
	adaptor, err := initTaskArtifactAdaptor(task)
	if err != nil {
		return err
	}
	provider, ok := adaptor.(taskImageProvider)
	if !ok {
		return errors.New("image artifact provider unavailable")
	}
	ch, err := model.CacheGetChannel(task.ChannelId)
	if err != nil {
		return err
	}
	return persistTaskImages(ctx, task, provider, store, strings.TrimSpace(ch.GetSetting().Proxy))
}

func persistTaskImages(ctx context.Context, task *model.Task, provider taskImageProvider, store service.TaskArtifactStore, proxy string) error {
	if !store.Enabled() {
		return service.ErrTaskArtifactStoreDisabled
	}
	// Project the provider success without exposing a public terminal state.
	completed := *task
	completed.Status = model.TaskStatusSuccess
	artifacts, err := provider.ListArtifacts(&completed)
	if err != nil {
		return err
	}
	artifacts, err = validateProjectedTaskArtifacts(artifacts)
	if err != nil {
		return err
	}
	if len(artifacts) == 0 {
		return errors.New("completed image task has no artifacts")
	}
	for _, artifact := range artifacts {
		if artifact.Type != "image" {
			return errors.New("image task contains a non-image artifact")
		}
	}
	client := service.GetSSRFProtectedHTTPClient()
	if proxy != "" {
		client, err = service.GetHttpClientWithProxy(proxy)
		if err != nil {
			return err
		}
	}
	if client == nil {
		return errors.New("artifact HTTP client unavailable")
	}
	client = taskMediaRedirectClient(client, proxy, nil, nil, true)
	client.Timeout = 90 * time.Second
	for _, artifact := range artifacts {
		existing, err := store.Resolve(task, artifact.Key)
		if err != nil {
			return err
		}
		if existing != nil {
			continue
		}
		descriptor, err := provider.BuildContentRequest(&completed, artifact.Key, relaychannel.TaskArtifactClientRequest{Method: http.MethodGet})
		if err != nil {
			return err
		}
		// APIMart returns signed image URLs. Never copy channel credentials to
		// a CDN, including across redirects.
		if descriptor == nil || !descriptor.Credentialless || descriptor.Method != http.MethodGet || len(descriptor.Headers) != 0 || descriptor.Body != nil {
			return errors.New("image download requires a credentialless GET")
		}
		rawURL := strings.TrimSpace(descriptor.URL)
		parsed, err := url.Parse(rawURL)
		if err != nil || len(rawURL) > 64<<10 || parsed == nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || isTaskMediaProxyPath(parsed.Path) {
			return errors.New("invalid image download URL")
		}
		if err := validateTaskMediaURL(rawURL, proxy); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return errors.New("image download failed")
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return fmt.Errorf("image download returned HTTP %d", resp.StatusCode)
		}
		_, persistErr := store.Persist(ctx, task, artifact, resp.Body)
		resp.Body.Close()
		if persistErr != nil {
			return persistErr
		}
	}
	return nil
}
