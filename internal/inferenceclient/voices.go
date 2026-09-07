package inferenceclient

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/QuantumNous/new-api/common"
)

type Voice struct {
	Kind           string `json:"kind,omitempty"`
	ID             string `json:"voice_id"`
	Name           string `json:"name"`
	SHA256         string `json:"sha256"`
	MediaType      string `json:"media_type"`
	SizeBytes      int64  `json:"size_bytes"`
	DurationMS     int64  `json:"duration_ms"`
	SampleRate     int64  `json:"sample_rate"`
	Source         string `json:"source"`
	PreviewURL     string `json:"preview_url,omitempty"`
	RetentionState string `json:"retention_state"`
}

type VoiceList struct {
	Data       []Voice `json:"data"`
	NextCursor string  `json:"next_cursor"`
}

func (client *Client) UploadVoice(ctx context.Context, source io.Reader, size int64, digest, tenant, name string) (Voice, error) {
	if source == nil || size < 44 || size > 12<<20 || !sha256Pattern.MatchString(digest) || strings.TrimSpace(name) == "" || len(name) > 200 || strings.ContainsAny(name, "\r\n") {
		return Voice{}, errors.New("invalid voice upload")
	}
	request, err := client.scopedRequest(ctx, http.MethodPost, "/internal/v1/voices", tenant)
	if err != nil {
		return Voice{}, err
	}
	request.Body = io.NopCloser(io.LimitReader(source, size+1))
	request.ContentLength = size
	request.Header.Set("Content-Type", "audio/wav")
	request.Header.Set("X-WildFlow-Content-SHA256", digest)
	request.Header.Set("X-WildFlow-Voice-Name", name)
	response, err := client.streamHTTP.Do(request)
	if err != nil {
		return Voice{}, errors.New("voice upload failed")
	}
	defer response.Body.Close()
	body, err := readBounded(response.Body)
	if err != nil {
		return Voice{}, err
	}
	if response.StatusCode != 201 {
		return Voice{}, responseError(response, body)
	}
	var voice Voice
	if err := common.Unmarshal(body, &voice); err != nil {
		return Voice{}, err
	}
	if !resourceIDPattern.MatchString(voice.ID) || voice.SHA256 != digest || voice.SizeBytes != size || voice.MediaType != "audio/wav" || voice.RetentionState != "active" {
		return Voice{}, errors.New("invalid voice upload response")
	}
	return voice, nil
}

func (client *Client) ListVoices(ctx context.Context, tenant, after string) (VoiceList, error) {
	if after != "" && !resourceIDPattern.MatchString(after) {
		return VoiceList{}, errors.New("invalid cursor")
	}
	request, err := client.scopedRequest(ctx, http.MethodGet, "/internal/v1/voices?after="+url.QueryEscape(after), tenant)
	if err != nil {
		return VoiceList{}, err
	}
	response, err := client.http.Do(request)
	if err != nil {
		return VoiceList{}, err
	}
	defer response.Body.Close()
	body, err := readBounded(response.Body)
	if err != nil {
		return VoiceList{}, err
	}
	if response.StatusCode != 200 {
		return VoiceList{}, responseError(response, body)
	}
	var voices VoiceList
	err = common.Unmarshal(body, &voices)
	return voices, err
}

func (client *Client) GetVoice(ctx context.Context, id, tenant string) (Voice, error) {
	if err := validateScopedResource(id, tenant); err != nil {
		return Voice{}, err
	}
	request, err := client.scopedRequest(ctx, http.MethodGet, "/internal/v1/voices/"+url.PathEscape(id), tenant)
	if err != nil {
		return Voice{}, err
	}
	response, err := client.http.Do(request)
	if err != nil {
		return Voice{}, err
	}
	defer response.Body.Close()
	body, err := readBounded(response.Body)
	if err != nil {
		return Voice{}, err
	}
	if response.StatusCode != 200 {
		return Voice{}, responseError(response, body)
	}
	var voice Voice
	err = common.Unmarshal(body, &voice)
	if err != nil {
		return Voice{}, err
	}
	if voice.ID != id {
		return Voice{}, errors.New("voice identity mismatch")
	}
	return voice, nil
}

func (client *Client) VoiceContent(ctx context.Context, id, tenant string) ([]byte, error) {
	voice, err := client.GetVoice(ctx, id, tenant)
	if err != nil {
		return nil, err
	}
	if voice.SizeBytes < 44 || voice.SizeBytes > 12<<20 || !sha256Pattern.MatchString(voice.SHA256) {
		return nil, errors.New("invalid voice content metadata")
	}
	request, err := client.scopedRequest(ctx, http.MethodGet, "/internal/v1/voices/"+url.PathEscape(id)+"/content", tenant)
	if err != nil {
		return nil, err
	}
	response, err := client.streamHTTP.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		body, _ := readBounded(response.Body)
		return nil, responseError(response, body)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, voice.SizeBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != voice.SizeBytes || fmt.Sprintf("%x", sha256.Sum256(data)) != voice.SHA256 {
		return nil, errors.New("voice content integrity failure")
	}
	return data, nil
}
