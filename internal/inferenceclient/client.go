// Package inferenceclient owns the private control-plane-to-inference HTTP
// boundary. It deliberately does not retry job submissions: callers must use
// the durable Operation state to decide whether a retry is safe.
package inferenceclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxResponseBodyBytes = 1 << 20

type Config struct {
	BaseURL string
	Token   string
	Timeout time.Duration
}

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

type JobCreateRequest struct {
	OperationID          string         `json:"operation_id"`
	RequestDigest        string         `json:"request_digest"`
	RequestID            string         `json:"request_id"`
	TenantRef            string         `json:"tenant_ref"`
	ProductModelRef      string         `json:"product_model_ref"`
	ModelVersionRef      string         `json:"model_version_ref"`
	InputArtifactRefs    []string       `json:"input_artifact_refs"`
	Parameters           map[string]any `json:"parameters"`
	DeadlineAt           time.Time      `json:"deadline_at"`
	CallbackCapabilities []string       `json:"callback_capabilities"`
}

type Job struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

type jobResponse struct {
	Job Job `json:"job"`
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
	baseURL, err := validateBaseURL(config.BaseURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(config.Token) == "" {
		return nil, errors.New("inference client token is required")
	}
	if config.Timeout <= 0 {
		return nil, errors.New("inference client timeout must be positive")
	}

	return &Client{
		baseURL: strings.TrimRight(baseURL.String(), "/"),
		token:   config.Token,
		http: &http.Client{
			Timeout: config.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func validateBaseURL(raw string) (*url.URL, error) {
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
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())) {
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

func (client *Client) SubmitJob(ctx context.Context, request JobCreateRequest) (Job, error) {
	if err := request.validate(); err != nil {
		return Job{}, err
	}

	body, err := json.Marshal(request)
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
	if err := json.Unmarshal(responseBody, &accepted); err != nil {
		return Job{}, fmt.Errorf("decode inference job response: %w", err)
	}
	if accepted.Job.ID == "" || accepted.Job.State == "" {
		return Job{}, errors.New("inference job response is missing id or state")
	}
	return accepted.Job, nil
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
	if request.InputArtifactRefs == nil {
		return errors.New("inference job input_artifact_refs is required")
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
	if json.Unmarshal(body, &apiError) == nil && strings.TrimSpace(apiError.Detail) != "" {
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
