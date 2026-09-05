// Package inferenceclient owns the private control-plane-to-inference HTTP
// boundary. It deliberately does not retry job submissions: callers must use
// the durable Operation state to decide whether a retry is safe.
package inferenceclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
)

const maxResponseBodyBytes = 1 << 20
const maxArtifactBodyBytes = 320 << 20
const maxInputArtifactBodyBytes = int64(256 << 20)

var resourceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)
var sha256Pattern = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)
var validJobStates = map[string]struct{}{
	"pending": {}, "queued": {}, "running": {}, "succeeded": {},
	"failed": {}, "cancelled": {}, "recovery_required": {},
}

type Config struct {
	BaseURL           string
	Token             string
	Timeout           time.Duration
	AllowInternalHTTP bool
}

type Client struct {
	baseURL    string
	token      string
	http       *http.Client
	streamHTTP *http.Client
}

type JobCreateRequest struct {
	OperationID          string         `json:"operation_id"`
	RequestDigest        string         `json:"request_digest"`
	RequestID            string         `json:"request_id"`
	TenantRef            string         `json:"tenant_ref"`
	ProductModelRef      string         `json:"product_model_ref"`
	ModelVersionRef      string         `json:"model_version_ref"`
	InputArtifactIDs     []string       `json:"input_artifact_ids"`
	Parameters           map[string]any `json:"parameters"`
	DeadlineAt           time.Time      `json:"deadline_at"`
	CallbackCapabilities []string       `json:"callback_capabilities"`
}

type Job struct {
	ID        string     `json:"id"`
	State     string     `json:"state"`
	LastError string     `json:"last_error,omitempty"`
	Artifacts []Artifact `json:"artifacts,omitempty"`
}

type Artifact struct {
	ID        string         `json:"id"`
	JobID     string         `json:"job_id"`
	MediaType string         `json:"media_type"`
	SizeBytes int64          `json:"size_bytes"`
	SHA256    string         `json:"sha256"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

type InputArtifact struct {
	ID             string `json:"id"`
	MediaType      string `json:"media_type"`
	SizeBytes      int64  `json:"size_bytes"`
	SHA256         string `json:"sha256"`
	RetentionState string `json:"retention_state"`
}

type ArtifactContent struct {
	Body          io.ReadCloser
	MediaType     string
	ContentLength int64
	Filename      string
}

type jobResponse struct {
	Job Job `json:"job"`
}

type artifactResponse struct {
	Artifact Artifact `json:"artifact"`
}

type inputArtifactResponse struct {
	Artifact InputArtifact `json:"artifact"`
}

type errorResponse struct {
	Detail string `json:"detail"`
}

type OperationConflictError struct {
	Message string
}

func (err *OperationConflictError) Error() string {
	return fmt.Sprintf("inference operation conflict: %s", err.Message)
}

type RetryableError struct {
	StatusCode int
	RetryAfter string
	Message    string
}

func (err *RetryableError) Error() string {
	return fmt.Sprintf("inference service returned retryable status %d: %s", err.StatusCode, err.Message)
}

type APIError struct {
	StatusCode int
	Message    string
}

func (err *APIError) Error() string {
	return fmt.Sprintf("inference service returned status %d: %s", err.StatusCode, err.Message)
}

func New(config Config) (*Client, error) {
	baseURL, err := validateBaseURL(config.BaseURL, config.AllowInternalHTTP)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(config.Token) == "" {
		return nil, errors.New("inference client token is required")
	}
	if config.Timeout <= 0 {
		return nil, errors.New("inference client timeout must be positive")
	}

	checkRedirect := func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("default HTTP transport does not support streaming safeguards")
	}
	streamTransport := defaultTransport.Clone()
	streamTransport.ResponseHeaderTimeout = config.Timeout
	return &Client{
		baseURL: strings.TrimRight(baseURL.String(), "/"),
		token:   config.Token,
		http: &http.Client{
			Timeout:       config.Timeout,
			CheckRedirect: checkRedirect,
		},
		streamHTTP: &http.Client{
			Transport:     streamTransport,
			CheckRedirect: checkRedirect,
		},
	}, nil
}

func validateBaseURL(raw string, allowInternalHTTP bool) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("parse inference client base URL: %w", err)
	}
	if !parsed.IsAbs() || parsed.Host == "" {
		return nil, errors.New("inference client base URL must be absolute")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("inference client base URL must not contain credentials, query, or fragment")
	}
	httpAllowed := parsed.Scheme == "http" && (isLoopbackHost(parsed.Hostname()) ||
		(allowInternalHTTP && isInternalHost(parsed.Hostname())))
	if parsed.Scheme != "https" && !httpAllowed {
		return nil, errors.New("inference client base URL requires HTTPS outside loopback development")
	}
	return parsed, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isInternalHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "host.docker.internal" || strings.HasSuffix(host, ".internal") || !strings.Contains(host, ".") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}

func (client *Client) SubmitJob(ctx context.Context, request JobCreateRequest) (Job, error) {
	if err := request.validate(); err != nil {
		return Job{}, err
	}

	body, err := common.Marshal(request)
	if err != nil {
		return Job{}, fmt.Errorf("encode inference job request: %w", err)
	}
	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		client.baseURL+"/internal/v1/jobs",
		bytes.NewReader(body),
	)
	if err != nil {
		return Job{}, fmt.Errorf("create inference job request: %w", err)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+client.token)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("X-WildFlow-Tenant-Ref", request.TenantRef)

	response, err := client.http.Do(httpRequest)
	if err != nil {
		return Job{}, fmt.Errorf("submit inference job: %w", err)
	}
	defer response.Body.Close()

	responseBody, err := readBounded(response.Body)
	if err != nil {
		return Job{}, fmt.Errorf("read inference job response: %w", err)
	}
	if response.StatusCode != http.StatusAccepted {
		return Job{}, responseError(response, responseBody)
	}

	var accepted jobResponse
	if err := common.Unmarshal(responseBody, &accepted); err != nil {
		return Job{}, fmt.Errorf("decode inference job response: %w", err)
	}
	if err := validateJob(accepted.Job); err != nil {
		return Job{}, err
	}
	return accepted.Job, nil
}

func (client *Client) UploadInputArtifact(
	ctx context.Context,
	source io.Reader,
	sizeBytes int64,
	digest string,
	tenantRef string,
) (InputArtifact, error) {
	if source == nil || sizeBytes < 4 || sizeBytes > maxInputArtifactBodyBytes ||
		!sha256Pattern.MatchString(digest) || strings.TrimSpace(tenantRef) == "" || len(tenantRef) > 200 {
		return InputArtifact{}, errors.New("invalid inference input artifact upload")
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		client.baseURL+"/internal/v1/input-artifacts",
		io.LimitReader(source, sizeBytes+1),
	)
	if err != nil {
		return InputArtifact{}, fmt.Errorf("create inference input artifact request: %w", err)
	}
	request.ContentLength = sizeBytes
	request.Header.Set("Authorization", "Bearer "+client.token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "audio/flac")
	request.Header.Set("X-WildFlow-Content-SHA256", strings.ToLower(digest))
	request.Header.Set("X-WildFlow-Tenant-Ref", tenantRef)
	response, err := client.streamHTTP.Do(request)
	if err != nil {
		return InputArtifact{}, fmt.Errorf("upload inference input artifact: %w", err)
	}
	defer response.Body.Close()
	body, err := readBounded(response.Body)
	if err != nil {
		return InputArtifact{}, fmt.Errorf("read inference input artifact response: %w", err)
	}
	if response.StatusCode != http.StatusCreated {
		return InputArtifact{}, responseError(response, body)
	}
	var payload inputArtifactResponse
	if err := common.Unmarshal(body, &payload); err != nil {
		return InputArtifact{}, fmt.Errorf("decode inference input artifact response: %w", err)
	}
	artifact := payload.Artifact
	if !resourceIDPattern.MatchString(artifact.ID) || artifact.MediaType != "audio/flac" ||
		artifact.SizeBytes != sizeBytes || artifact.SizeBytes > maxInputArtifactBodyBytes ||
		!strings.EqualFold(artifact.SHA256, digest) || artifact.RetentionState != "active" {
		return InputArtifact{}, errors.New("inference input artifact response is invalid")
	}
	return artifact, nil
}

func (client *Client) GetJob(ctx context.Context, jobID, tenantRef string) (Job, error) {
	if err := validateScopedResource(jobID, tenantRef); err != nil {
		return Job{}, err
	}
	request, err := client.scopedRequest(
		ctx,
		http.MethodGet,
		"/internal/v1/jobs/"+url.PathEscape(jobID),
		tenantRef,
	)
	if err != nil {
		return Job{}, err
	}
	response, err := client.http.Do(request)
	if err != nil {
		return Job{}, fmt.Errorf("get inference job: %w", err)
	}
	defer response.Body.Close()
	body, err := readBounded(response.Body)
	if err != nil {
		return Job{}, fmt.Errorf("read inference job response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return Job{}, responseError(response, body)
	}
	var payload jobResponse
	if err := common.Unmarshal(body, &payload); err != nil {
		return Job{}, fmt.Errorf("decode inference job response: %w", err)
	}
	if err := validateJob(payload.Job); err != nil {
		return Job{}, err
	}
	if payload.Job.ID != jobID {
		return Job{}, errors.New("inference job response identity mismatch")
	}
	return payload.Job, nil
}

func (client *Client) CancelJob(ctx context.Context, jobID, tenantRef string) (Job, error) {
	if err := validateScopedResource(jobID, tenantRef); err != nil {
		return Job{}, err
	}
	request, err := client.scopedRequest(
		ctx,
		http.MethodPost,
		"/internal/v1/jobs/"+url.PathEscape(jobID)+":cancel",
		tenantRef,
	)
	if err != nil {
		return Job{}, err
	}
	response, err := client.http.Do(request)
	if err != nil {
		return Job{}, fmt.Errorf("cancel inference job: %w", err)
	}
	defer response.Body.Close()
	body, err := readBounded(response.Body)
	if err != nil {
		return Job{}, fmt.Errorf("read inference cancel response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return Job{}, responseError(response, body)
	}
	var payload jobResponse
	if err := common.Unmarshal(body, &payload); err != nil {
		return Job{}, fmt.Errorf("decode inference cancel response: %w", err)
	}
	if err := validateJob(payload.Job); err != nil {
		return Job{}, err
	}
	if payload.Job.ID != jobID {
		return Job{}, errors.New("inference cancel response identity mismatch")
	}
	return payload.Job, nil
}

func (client *Client) GetArtifact(ctx context.Context, artifactID, tenantRef string) (Artifact, error) {
	if err := validateScopedResource(artifactID, tenantRef); err != nil {
		return Artifact{}, err
	}
	request, err := client.scopedRequest(
		ctx,
		http.MethodGet,
		"/internal/v1/artifacts/"+url.PathEscape(artifactID),
		tenantRef,
	)
	if err != nil {
		return Artifact{}, err
	}
	response, err := client.http.Do(request)
	if err != nil {
		return Artifact{}, fmt.Errorf("get inference artifact: %w", err)
	}
	defer response.Body.Close()
	body, err := readBounded(response.Body)
	if err != nil {
		return Artifact{}, fmt.Errorf("read inference artifact response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return Artifact{}, responseError(response, body)
	}
	var payload artifactResponse
	if err := common.Unmarshal(body, &payload); err != nil {
		return Artifact{}, fmt.Errorf("decode inference artifact response: %w", err)
	}
	if err := validateArtifact(payload.Artifact); err != nil {
		return Artifact{}, err
	}
	return payload.Artifact, nil
}

func (client *Client) OpenArtifactContent(
	ctx context.Context,
	artifactID string,
	tenantRef string,
) (*ArtifactContent, error) {
	if err := validateScopedResource(artifactID, tenantRef); err != nil {
		return nil, err
	}
	request, err := client.scopedRequest(
		ctx,
		http.MethodGet,
		"/internal/v1/artifacts/"+url.PathEscape(artifactID)+"/content",
		tenantRef,
	)
	if err != nil {
		return nil, err
	}
	response, err := client.streamHTTP.Do(request)
	if err != nil {
		return nil, fmt.Errorf("open inference artifact content: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		body, readErr := readBounded(response.Body)
		if readErr != nil {
			return nil, readErr
		}
		return nil, responseError(response, body)
	}
	if response.ContentLength < 0 {
		response.Body.Close()
		return nil, errors.New("inference artifact content requires a bounded content length")
	}
	if response.ContentLength > maxArtifactBodyBytes {
		response.Body.Close()
		return nil, errors.New("inference artifact content exceeds 320 MiB limit")
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || (!strings.HasPrefix(mediaType, "audio/") && !strings.HasPrefix(mediaType, "image/") && mediaType != "application/json") {
		response.Body.Close()
		return nil, errors.New("inference artifact content has invalid media type")
	}
	filename := "artifact.bin"
	if _, parameters, dispositionErr := mime.ParseMediaType(response.Header.Get("Content-Disposition")); dispositionErr == nil {
		if candidate := strings.TrimSpace(parameters["filename"]); candidate != "" {
			filename = candidate
		}
	}
	return &ArtifactContent{
		Body:          http.MaxBytesReader(nil, response.Body, maxArtifactBodyBytes),
		MediaType:     mediaType,
		ContentLength: response.ContentLength,
		Filename:      filename,
	}, nil
}

func (client *Client) scopedRequest(
	ctx context.Context,
	method string,
	path string,
	tenantRef string,
) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, client.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+client.token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-WildFlow-Tenant-Ref", tenantRef)
	return request, nil
}

func validateScopedResource(resourceID, tenantRef string) error {
	if !resourceIDPattern.MatchString(resourceID) {
		return errors.New("invalid inference resource id")
	}
	if strings.TrimSpace(tenantRef) == "" || len(tenantRef) > 200 {
		return errors.New("invalid inference tenant ref")
	}
	return nil
}

func validateArtifact(artifact Artifact) error {
	if !resourceIDPattern.MatchString(artifact.ID) || !resourceIDPattern.MatchString(artifact.JobID) {
		return errors.New("inference artifact response has invalid identity")
	}
	mediaType, _, err := mime.ParseMediaType(artifact.MediaType)
	if err != nil || (!strings.HasPrefix(mediaType, "audio/") && !strings.HasPrefix(mediaType, "image/") && mediaType != "application/json") {
		return errors.New("inference artifact response has invalid media type")
	}
	if artifact.SizeBytes < 0 || artifact.SizeBytes > maxArtifactBodyBytes || !sha256Pattern.MatchString(artifact.SHA256) {
		return errors.New("inference artifact response has invalid integrity metadata")
	}
	return nil
}

func validateJob(job Job) error {
	if !resourceIDPattern.MatchString(job.ID) {
		return errors.New("inference job response has invalid identity")
	}
	if _, ok := validJobStates[job.State]; !ok {
		return errors.New("inference job response has invalid state")
	}
	for _, artifact := range job.Artifacts {
		if err := validateArtifact(artifact); err != nil {
			return err
		}
		if artifact.JobID != job.ID {
			return errors.New("inference artifact response job identity mismatch")
		}
	}
	return nil
}

func (request JobCreateRequest) validate() error {
	required := map[string]string{
		"operation_id":      request.OperationID,
		"request_digest":    request.RequestDigest,
		"request_id":        request.RequestID,
		"tenant_ref":        request.TenantRef,
		"product_model_ref": request.ProductModelRef,
		"model_version_ref": request.ModelVersionRef,
	}
	for field, value := range required {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("inference job %s is required", field)
		}
	}
	if request.InputArtifactIDs == nil || len(request.InputArtifactIDs) > 64 {
		return errors.New("inference job input_artifact_ids is required")
	}
	for _, artifactID := range request.InputArtifactIDs {
		if !resourceIDPattern.MatchString(artifactID) {
			return errors.New("inference job input_artifact_ids is invalid")
		}
	}
	if request.Parameters == nil {
		return errors.New("inference job parameters is required")
	}
	if request.CallbackCapabilities == nil {
		return errors.New("inference job callback_capabilities is required")
	}
	if request.DeadlineAt.IsZero() {
		return errors.New("inference job deadline_at is required")
	}
	return nil
}

func readBounded(reader io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maxResponseBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxResponseBodyBytes {
		return nil, errors.New("response exceeds 1 MiB limit")
	}
	return body, nil
}

func responseError(response *http.Response, body []byte) error {
	message := strings.TrimSpace(http.StatusText(response.StatusCode))
	var apiError errorResponse
	if common.Unmarshal(body, &apiError) == nil && strings.TrimSpace(apiError.Detail) != "" {
		message = apiError.Detail
	}

	switch response.StatusCode {
	case http.StatusConflict:
		return &OperationConflictError{Message: message}
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return &RetryableError{
			StatusCode: response.StatusCode,
			RetryAfter: response.Header.Get("Retry-After"),
			Message:    message,
		}
	default:
		return &APIError{StatusCode: response.StatusCode, Message: message}
	}
}
