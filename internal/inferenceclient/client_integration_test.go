package inferenceclient

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"
)

// Run with a local wildflow-inference process. The test is opt-in so the API
// repository never reaches an undeclared service during the normal test suite.
func TestLocalInferenceIntegration(t *testing.T) {
	baseURL := os.Getenv("WILDFLOW_INFERENCE_INTEGRATION_URL")
	token := os.Getenv("WILDFLOW_INFERENCE_INTEGRATION_TOKEN")
	if baseURL == "" || token == "" {
		t.Skip("local inference integration environment is not configured")
	}

	client, err := New(Config{BaseURL: baseURL, Token: token, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Now().UTC()
	identity := strconv.FormatInt(now.UnixNano(), 10)
	request := validRequest()
	request.OperationID = "local-integration-" + identity
	request.RequestDigest = "digest-" + identity
	request.RequestID = "request-" + identity
	request.DeadlineAt = now.Add(time.Minute)

	job, err := client.SubmitJob(context.Background(), request)
	if err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}
	if job.ID == "" || job.State != "queued" {
		t.Fatalf("unexpected accepted job: %#v", job)
	}
}
