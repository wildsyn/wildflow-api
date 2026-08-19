package inferenceclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validRequest() JobCreateRequest {
	return JobCreateRequest{
		OperationID:          "op-1",
		RequestDigest:        "digest-1",
		RequestID:            "request-1",
		TenantRef:            "tenant-a",
		ProductModelRef:      "tts-standard",
		ModelVersionRef:      "openbmb/VoxCPM2",
		InputArtifactRefs:    []string{"input://script-1"},
		Parameters:           map[string]any{"speed": 1.0},
		DeadlineAt:           time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC),
		CallbackCapabilities: []string{},
	}
}

func TestSubmitJobSendsInternalContractAndParsesAcceptedJob(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/jobs" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer internal-token" {
			t.Fatalf("unexpected authorization header: %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("unexpected content type: %q", got)
		}

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		for _, field := range []string{
			"operation_id", "request_digest", "request_id", "tenant_ref",
			"product_model_ref", "model_version_ref", "input_artifact_refs",
			"parameters", "deadline_at", "callback_capabilities",
		} {
			if _, ok := body[field]; !ok {
				t.Errorf("request is missing %q", field)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job":{"id":"job-1","state":"queued"}}`))
	}))
	defer server.Close()

	client, err := New(Config{
		BaseURL: server.URL,
		Token:   "internal-token",
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	job, err := client.SubmitJob(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}
	if job.ID != "job-1" || job.State != "queued" {
		t.Fatalf("unexpected job: %#v", job)
	}
}

func TestSubmitJobReturnsTypedConflictWithoutRetry(t *testing.T) {
	t.Parallel()

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(w, `{"detail":"operation_id was already used with a different request"}`, http.StatusConflict)
	}))
	defer server.Close()

	client, err := New(Config{BaseURL: server.URL, Token: "token", Timeout: time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = client.SubmitJob(context.Background(), validRequest())
	var conflict *OperationConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected OperationConflictError, got %T: %v", err, err)
	}
	if requests != 1 {
		t.Fatalf("client must not retry an ambiguous operation: got %d requests", requests)
	}
}

func TestSubmitJobPreservesRetryAfterForRetryableStatus(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "17")
		http.Error(w, `{"detail":"temporarily unavailable"}`, http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client, err := New(Config{BaseURL: server.URL, Token: "token", Timeout: time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = client.SubmitJob(context.Background(), validRequest())
	var retryable *RetryableError
	if !errors.As(err, &retryable) {
		t.Fatalf("expected RetryableError, got %T: %v", err, err)
	}
	if retryable.StatusCode != http.StatusServiceUnavailable || retryable.RetryAfter != "17" {
		t.Fatalf("unexpected retryable error: %#v", retryable)
	}
}

func TestSubmitJobDoesNotForwardTokenAcrossRedirect(t *testing.T) {
	t.Parallel()

	receiverRequests := 0
	receiver := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		receiverRequests++
	}))
	defer receiver.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", receiver.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	client, err := New(Config{BaseURL: server.URL, Token: "sensitive-token", Timeout: time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = client.SubmitJob(context.Background(), validRequest())
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("expected redirect APIError, got %T: %v", err, err)
	}
	if receiverRequests != 0 {
		t.Fatalf("redirect receiver got %d requests", receiverRequests)
	}
}

func TestNewFailsClosedForUnsafeConfiguration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		config Config
	}{
		{name: "missing base URL", config: Config{Token: "token"}},
		{name: "missing token", config: Config{BaseURL: "https://inference.internal"}},
		{name: "insecure non-loopback URL", config: Config{BaseURL: "http://inference.internal", Token: "token"}},
		{name: "non-absolute URL", config: Config{BaseURL: "/internal", Token: "token"}},
		{name: "missing timeout", config: Config{BaseURL: "https://inference.internal", Token: "token"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.config); err == nil {
				t.Fatal("expected configuration error")
			}
		})
	}
}

func TestNewAllowsExplicitInternalHTTPDeployment(t *testing.T) {
	t.Parallel()

	client, err := New(Config{
		BaseURL:           "http://inference.internal:8120",
		Token:             "token",
		Timeout:           time.Second,
		AllowInternalHTTP: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if client == nil {
		t.Fatal("expected client")
	}
}

func TestNewStillRejectsPublicHTTPWhenInternalExceptionIsEnabled(t *testing.T) {
	t.Parallel()

	_, err := New(Config{
		BaseURL:           "http://example.com:8120",
		Token:             "token",
		Timeout:           time.Second,
		AllowInternalHTTP: true,
	})
	if err == nil {
		t.Fatal("expected public HTTP URL to remain rejected")
	}
}

func TestSubmitJobValidatesIdentityFieldsBeforeNetwork(t *testing.T) {
	t.Parallel()

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	defer server.Close()

	client, err := New(Config{BaseURL: server.URL, Token: "token", Timeout: time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := validRequest()
	request.TenantRef = ""

	if _, err := client.SubmitJob(context.Background(), request); err == nil {
		t.Fatal("expected validation error")
	}
	if requests != 0 {
		t.Fatalf("invalid request reached inference service: %d requests", requests)
	}
}

func TestJobAndArtifactReadsStayTenantScoped(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer internal-token" {
			t.Fatalf("unexpected authorization: %q", got)
		}
		if got := r.Header.Get("X-WildFlow-Tenant-Ref"); got != "user:42" {
			t.Fatalf("unexpected tenant ref: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/internal/v1/jobs/job-1":
			_, _ = w.Write([]byte(`{"job":{"id":"job-1","state":"succeeded","artifacts":[{"id":"artifact-1","job_id":"job-1","media_type":"audio/wav","size_bytes":12,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/internal/v1/jobs/job-1:cancel":
			_, _ = w.Write([]byte(`{"job":{"id":"job-1","state":"cancelled","artifacts":[]}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/internal/v1/artifacts/artifact-1":
			_, _ = w.Write([]byte(`{"artifact":{"id":"artifact-1","job_id":"job-1","media_type":"audio/wav","size_bytes":12,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := New(Config{BaseURL: server.URL, Token: "internal-token", Timeout: time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	job, err := client.GetJob(context.Background(), "job-1", "user:42")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.State != "succeeded" || len(job.Artifacts) != 1 || job.Artifacts[0].ID != "artifact-1" {
		t.Fatalf("unexpected job: %#v", job)
	}
	cancelled, err := client.CancelJob(context.Background(), "job-1", "user:42")
	if err != nil {
		t.Fatalf("CancelJob: %v", err)
	}
	if cancelled.State != "cancelled" {
		t.Fatalf("unexpected cancelled job: %#v", cancelled)
	}
	artifact, err := client.GetArtifact(context.Background(), "artifact-1", "user:42")
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	if artifact.JobID != "job-1" || artifact.MediaType != "audio/wav" {
		t.Fatalf("unexpected artifact: %#v", artifact)
	}
}

func TestOpenArtifactContentReturnsVerifiedInternalStream(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/artifacts/artifact-1/content" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("X-WildFlow-Tenant-Ref"); got != "user:42" {
			t.Fatalf("unexpected tenant ref: %q", got)
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Content-Disposition", `attachment; filename="artifact-1.wav"`)
		_, _ = w.Write([]byte("audio-result"))
	}))
	defer server.Close()

	client, err := New(Config{BaseURL: server.URL, Token: "internal-token", Timeout: time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	content, err := client.OpenArtifactContent(context.Background(), "artifact-1", "user:42")
	if err != nil {
		t.Fatalf("OpenArtifactContent: %v", err)
	}
	defer content.Body.Close()
	payload, err := io.ReadAll(content.Body)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if string(payload) != "audio-result" || content.MediaType != "audio/wav" {
		t.Fatalf("unexpected content: %#v payload=%q", content, payload)
	}
}

func TestOpenArtifactContentUsesThe320MiBFinalArtifactContract(t *testing.T) {
	tests := []struct {
		name          string
		contentLength int64
		wantError     bool
	}{
		{name: "accepts an MP3 above the obsolete 128 MiB cap", contentLength: 200 << 20},
		{name: "rejects an artifact above 320 MiB", contentLength: (320 << 20) + 1, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "audio/mpeg")
				w.Header().Set("Content-Length", fmt.Sprintf("%d", test.contentLength))
			}))
			defer server.Close()
			client, err := New(Config{BaseURL: server.URL, Token: "internal-token", Timeout: time.Second})
			require.NoError(t, err)

			content, err := client.OpenArtifactContent(context.Background(), "artifact-1", "user:42")
			if test.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			defer content.Body.Close()
			assert.Equal(t, test.contentLength, content.ContentLength)
		})
	}
}

func TestOpenArtifactContentRejectsLengthlessStreamsBeforePublicDownloadStarts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		flusher, ok := w.(http.Flusher)
		require.True(t, ok)
		flusher.Flush()
		_, _ = w.Write([]byte("partial-audio"))
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Token: "internal-token", Timeout: time.Second})
	require.NoError(t, err)

	content, err := client.OpenArtifactContent(context.Background(), "artifact-1", "user:42")

	require.Error(t, err)
	assert.Nil(t, content)
}
