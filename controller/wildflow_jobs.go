package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/internal/inferenceclient"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
)

const wildFlowJobRequestLimit = 256 * 1024

func CreateWildFlowJob(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, wildFlowJobRequestLimit)
	request, err := decodeWildFlowJobRequest(c.Request.Body)
	if err != nil {
		wildFlowJobError(c, http.StatusBadRequest, "invalid_request", "invalid job request")
		return
	}
	createWildFlowJob(c, request)
}

func createWildFlowJob(c *gin.Context, request service.WildFlowJobRequest) {
	var err error
	request, err = service.NormalizeWildFlowJobRequest(request)
	if err != nil {
		writeWildFlowOperationError(c, err)
		return
	}
	if !wildFlowTokenAllowsModel(c, request.Model) {
		wildFlowJobError(c, http.StatusForbidden, "model_forbidden", "token is not allowed to use this model")
		return
	}

	userID := c.GetInt("id")
	operation, created, err := service.PrepareWildFlowOperation(
		userID,
		c.GetInt("token_id"),
		wildFlowIdempotencyKey(c),
		c.GetString(common.RequestIdKey),
		request,
	)
	if err != nil {
		writeWildFlowOperationError(c, err)
		return
	}
	if operation.State == "recovery_required" {
		writeWildFlowOperationHeaders(c, operation)
		c.JSON(http.StatusOK, wildFlowOperationResponse(operation, nil))
		return
	}
	if operation.JobID != "" {
		client, clientErr := newWildFlowInferenceClient()
		if clientErr != nil {
			wildFlowJobError(c, http.StatusServiceUnavailable, "inference_unavailable", "inference service is unavailable")
			return
		}
		job, getErr := client.GetJob(c.Request.Context(), operation.JobID, wildFlowTenantRef(userID))
		if getErr != nil {
			writeWildFlowInferenceError(c, getErr)
			return
		}
		errorCode := wildFlowExecutionErrorCode(job.State, job.LastError)
		if updateErr := model.UpdateWildFlowOperationExecution(operation.OperationID, job.ID, job.State, errorCode); updateErr != nil {
			wildFlowInternalError(c, updateErr)
			return
		}
		operation.State = job.State
		operation.LastErrorCode = errorCode
		if !finalizeWildFlowOperationBilling(c, operation, job.Artifacts) {
			return
		}
		writeWildFlowOperationHeaders(c, operation)
		c.JSON(http.StatusOK, wildFlowOperationResponse(operation, job.Artifacts))
		return
	}
	client, err := newWildFlowInferenceClient()
	if err != nil {
		wildFlowJobError(c, http.StatusServiceUnavailable, "inference_unavailable", "inference service is unavailable")
		return
	}
	operation, err = service.ReserveWildFlowOperationBilling(operation, request)
	if err != nil {
		writeWildFlowBillingError(c, err)
		return
	}
	job, err := client.SubmitJob(c.Request.Context(), inferenceclient.JobCreateRequest{
		OperationID:          operation.OperationID,
		RequestDigest:        operation.RequestDigest,
		RequestID:            operation.RequestID,
		TenantRef:            wildFlowTenantRef(userID),
		ProductModelRef:      operation.ProductModelRef,
		ModelVersionRef:      operation.ModelVersionRef,
		InputArtifactRefs:    []string{},
		Parameters:           request.Parameters,
		DeadlineAt:           time.Now().UTC().Add(30 * time.Minute),
		CallbackCapabilities: []string{},
	})
	if err != nil {
		var retryable *inferenceclient.RetryableError
		if errors.As(err, &retryable) {
			if updateErr := model.UpdateWildFlowOperationExecution(
				operation.OperationID,
				operation.JobID,
				"submitting",
				"inference_unavailable",
			); updateErr != nil {
				wildFlowInternalError(c, updateErr)
				return
			}
			writeWildFlowInferenceError(c, err)
			return
		}
		markWildFlowRecoveryRequired(c, operation, "submission_unknown", err)
		return
	}
	if err := model.UpdateWildFlowOperationExecution(operation.OperationID, job.ID, job.State, ""); err != nil {
		markWildFlowRecoveryRequired(c, operation, "operation_persistence_failed", err)
		return
	}
	operation.JobID = job.ID
	operation.State = job.State
	operation.LastErrorCode = ""
	if !finalizeWildFlowOperationBilling(c, operation, job.Artifacts) {
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusAccepted
	}
	writeWildFlowOperationHeaders(c, operation)
	c.JSON(status, operation)
}

func wildFlowTokenAllowsModel(c *gin.Context, modelName string) bool {
	if !common.GetContextKeyBool(c, constant.ContextKeyTokenModelLimitEnabled) {
		return true
	}
	rawLimits, ok := common.GetContextKey(c, constant.ContextKeyTokenModelLimit)
	if !ok {
		return false
	}
	limits, ok := rawLimits.(map[string]bool)
	if !ok {
		return false
	}
	matchingName := ratio_setting.FormatMatchingModelName(modelName)
	return limits[modelName] || limits[matchingName]
}

func wildFlowIdempotencyKey(c *gin.Context) string {
	if key := strings.TrimSpace(c.GetHeader("Idempotency-Key")); key != "" {
		return key
	}
	return strings.TrimSpace(c.GetHeader("X-Idempotency-Key"))
}

func writeWildFlowOperationHeaders(c *gin.Context, operation *model.WildFlowOperation) {
	c.Header("Location", "/v1/jobs/"+operation.OperationID)
	if operation.JobID != "" {
		c.Header("X-Job-ID", operation.JobID)
	}
}

func GetWildFlowJob(c *gin.Context) {
	operation, ok := loadWildFlowOperation(c)
	if !ok {
		return
	}
	if operation.State == "recovery_required" {
		c.JSON(http.StatusOK, wildFlowOperationResponse(operation, nil))
		return
	}
	if operation.JobID == "" {
		c.JSON(http.StatusOK, operation)
		return
	}
	client, err := newWildFlowInferenceClient()
	if err != nil {
		wildFlowJobError(c, http.StatusServiceUnavailable, "inference_unavailable", "inference service is unavailable")
		return
	}
	job, err := client.GetJob(c.Request.Context(), operation.JobID, wildFlowTenantRef(operation.UserID))
	if err != nil {
		writeWildFlowInferenceError(c, err)
		return
	}
	errorCode := wildFlowExecutionErrorCode(job.State, job.LastError)
	if err := model.UpdateWildFlowOperationExecution(operation.OperationID, job.ID, job.State, errorCode); err != nil {
		wildFlowInternalError(c, err)
		return
	}
	operation.State = job.State
	operation.LastErrorCode = errorCode
	if !finalizeWildFlowOperationBilling(c, operation, job.Artifacts) {
		return
	}
	c.JSON(http.StatusOK, wildFlowOperationResponse(operation, job.Artifacts))
}

func CancelWildFlowJob(c *gin.Context) {
	operation, ok := loadWildFlowOperation(c)
	if !ok {
		return
	}
	if operation.JobID == "" {
		wildFlowJobError(c, http.StatusConflict, "job_not_submitted", "job has not been submitted")
		return
	}
	client, err := newWildFlowInferenceClient()
	if err != nil {
		wildFlowJobError(c, http.StatusServiceUnavailable, "inference_unavailable", "inference service is unavailable")
		return
	}
	job, err := client.CancelJob(c.Request.Context(), operation.JobID, wildFlowTenantRef(operation.UserID))
	if err != nil {
		writeWildFlowInferenceError(c, err)
		return
	}
	errorCode := wildFlowExecutionErrorCode(job.State, job.LastError)
	if err := model.UpdateWildFlowOperationExecution(operation.OperationID, job.ID, job.State, errorCode); err != nil {
		wildFlowInternalError(c, err)
		return
	}
	operation.State = job.State
	operation.LastErrorCode = errorCode
	if !finalizeWildFlowOperationBilling(c, operation, job.Artifacts) {
		return
	}
	c.JSON(http.StatusOK, operation)
}

func GetWildFlowArtifact(c *gin.Context) {
	artifact, _, ok := loadOwnedWildFlowArtifact(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, publicWildFlowArtifact(artifact))
}

func DownloadWildFlowArtifact(c *gin.Context) {
	artifact, operation, ok := loadOwnedWildFlowArtifact(c)
	if !ok {
		return
	}
	client, err := newWildFlowInferenceClient()
	if err != nil {
		wildFlowJobError(c, http.StatusServiceUnavailable, "inference_unavailable", "inference service is unavailable")
		return
	}
	content, err := client.OpenArtifactContent(
		c.Request.Context(),
		artifact.ID,
		wildFlowTenantRef(c.GetInt("id")),
	)
	if err != nil {
		writeWildFlowInferenceError(c, err)
		return
	}
	defer content.Body.Close()
	if content.MediaType != artifact.MediaType ||
		(content.ContentLength >= 0 && content.ContentLength != artifact.SizeBytes) {
		if updateErr := model.UpdateWildFlowOperationExecution(
			operation.OperationID,
			operation.JobID,
			"recovery_required",
			"artifact_integrity_error",
		); updateErr != nil {
			wildFlowInternalError(c, updateErr)
			return
		}
		wildFlowJobError(c, http.StatusServiceUnavailable, "artifact_integrity_error", "artifact content requires recovery")
		return
	}
	filename := content.Filename
	if artifact.MediaType == "audio/mpeg" {
		filename = artifact.ID + ".mp3"
	}
	c.Header("Content-Type", content.MediaType)
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", safeWildFlowFilename(filename)))
	if content.ContentLength >= 0 {
		c.Header("Content-Length", strconv.FormatInt(content.ContentLength, 10))
	}
	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, content.Body); err != nil {
		logger.LogError(c.Request.Context(), "stream WildFlow artifact: "+err.Error())
	}
}

func loadWildFlowOperation(c *gin.Context) (*model.WildFlowOperation, bool) {
	operation, err := model.GetWildFlowOperationForUser(c.GetInt("id"), c.Param("operation_id"))
	if err != nil {
		wildFlowInternalError(c, err)
		return nil, false
	}
	if operation == nil {
		wildFlowJobError(c, http.StatusNotFound, "operation_not_found", "operation not found")
		return nil, false
	}
	return operation, true
}

func loadOwnedWildFlowArtifact(c *gin.Context) (inferenceclient.Artifact, *model.WildFlowOperation, bool) {
	client, err := newWildFlowInferenceClient()
	if err != nil {
		wildFlowJobError(c, http.StatusServiceUnavailable, "inference_unavailable", "inference service is unavailable")
		return inferenceclient.Artifact{}, nil, false
	}
	userID := c.GetInt("id")
	artifact, err := client.GetArtifact(c.Request.Context(), c.Param("artifact_id"), wildFlowTenantRef(userID))
	if err != nil {
		writeWildFlowInferenceError(c, err)
		return inferenceclient.Artifact{}, nil, false
	}
	operation, err := model.GetWildFlowOperationForUserAndJob(userID, artifact.JobID)
	if err != nil {
		wildFlowInternalError(c, err)
		return inferenceclient.Artifact{}, nil, false
	}
	if operation == nil {
		wildFlowJobError(c, http.StatusNotFound, "artifact_not_found", "artifact not found")
		return inferenceclient.Artifact{}, nil, false
	}
	if operation.State != "succeeded" || operation.BillingState != model.WildFlowBillingStateSettled {
		wildFlowJobError(c, http.StatusConflict, "artifact_not_ready", "artifact is not ready")
		return inferenceclient.Artifact{}, nil, false
	}
	if err := service.ValidateWildFlowCompletedArtifacts(operation, []inferenceclient.Artifact{artifact}); err != nil {
		if updateErr := model.UpdateWildFlowOperationExecution(
			operation.OperationID,
			operation.JobID,
			"recovery_required",
			"invalid_artifact",
		); updateErr != nil {
			wildFlowInternalError(c, updateErr)
			return inferenceclient.Artifact{}, nil, false
		}
		wildFlowJobError(c, http.StatusServiceUnavailable, "recovery_required", "job result requires recovery")
		return inferenceclient.Artifact{}, nil, false
	}
	return artifact, operation, true
}

func newWildFlowInferenceClient() (*inferenceclient.Client, error) {
	return inferenceclient.New(inferenceclient.Config{
		BaseURL:           strings.TrimSpace(os.Getenv("WILDFLOW_INFERENCE_URL")),
		Token:             strings.TrimSpace(os.Getenv("WILDFLOW_INTERNAL_TOKEN")),
		Timeout:           30 * time.Second,
		AllowInternalHTTP: common.GetEnvOrDefaultBool("WILDFLOW_INFERENCE_ALLOW_INTERNAL_HTTP", false),
	})
}

func decodeWildFlowJobRequest(reader io.Reader) (service.WildFlowJobRequest, error) {
	body, err := io.ReadAll(reader)
	if err != nil {
		return service.WildFlowJobRequest{}, err
	}
	var fields map[string]json.RawMessage
	if err := common.Unmarshal(body, &fields); err != nil || len(fields) != 2 {
		return service.WildFlowJobRequest{}, errors.New("invalid job request")
	}
	modelField, hasModel := fields["model"]
	parametersField, hasParameters := fields["parameters"]
	if !hasModel || !hasParameters {
		return service.WildFlowJobRequest{}, errors.New("invalid job request")
	}
	var request service.WildFlowJobRequest
	if err := common.Unmarshal(modelField, &request.Model); err != nil {
		return service.WildFlowJobRequest{}, err
	}
	if err := common.Unmarshal(parametersField, &request.Parameters); err != nil {
		return service.WildFlowJobRequest{}, err
	}
	return request, nil
}

func wildFlowTenantRef(userID int) string {
	return "user:" + strconv.Itoa(userID)
}

func publicWildFlowArtifact(artifact inferenceclient.Artifact) gin.H {
	return gin.H{
		"id":         artifact.ID,
		"job_id":     artifact.JobID,
		"media_type": artifact.MediaType,
		"size_bytes": artifact.SizeBytes,
		"sha256":     artifact.SHA256,
		"metadata":   publicWildFlowArtifactMetadata(artifact.Metadata),
		"download":   "/v1/artifacts/" + artifact.ID + "/content",
	}
}

func publicWildFlowArtifactMetadata(metadata map[string]any) map[string]any {
	public := make(map[string]any)
	for _, key := range []string{
		"codec", "bitrate", "sample_rate", "channels", "duration_ms",
		"input_characters", "completed_characters", "segment_count", "completed_segment_count",
		"size_bytes", "sha256", "voice", "width", "height", "prompt_length",
	} {
		if value, ok := metadata[key]; ok {
			public[key] = value
		}
	}
	return public
}

func wildFlowOperationResponse(operation *model.WildFlowOperation, artifacts []inferenceclient.Artifact) gin.H {
	response := gin.H{
		"id":                operation.OperationID,
		"request_id":        operation.RequestID,
		"model":             operation.ProductModelRef,
		"model_version_ref": operation.ModelVersionRef,
		"job_id":            operation.JobID,
		"state":             operation.State,
		"created_at":        operation.CreatedTime,
		"updated_at":        operation.UpdatedTime,
	}
	if operation.LastErrorCode != "" {
		response["error"] = operation.LastErrorCode
	}
	if len(artifacts) > 0 {
		publicArtifacts := make([]gin.H, 0, len(artifacts))
		for _, artifact := range artifacts {
			publicArtifacts = append(publicArtifacts, publicWildFlowArtifact(artifact))
		}
		response["artifacts"] = publicArtifacts
	}
	return response
}

func safeWildFlowFilename(filename string) string {
	filename = strings.TrimSpace(filename)
	if filename == "" || strings.ContainsAny(filename, "\r\n/\\") {
		return "artifact.bin"
	}
	return filename
}

func wildFlowExecutionErrorCode(state string, lastError string) string {
	if state == "recovery_required" {
		return "recovery_required"
	}
	if state == "failed" || strings.TrimSpace(lastError) != "" {
		return "execution_failed"
	}
	return ""
}

func markWildFlowRecoveryRequired(c *gin.Context, operation *model.WildFlowOperation, code string, cause error) {
	if err := model.UpdateWildFlowOperationExecution(operation.OperationID, operation.JobID, "recovery_required", code); err != nil {
		logger.LogError(c.Request.Context(), "persist WildFlow recovery state: "+err.Error())
	}
	logger.LogError(c.Request.Context(), "WildFlow job requires recovery: "+cause.Error())
	wildFlowJobError(c, http.StatusServiceUnavailable, code, "job submission is temporarily unavailable")
}

func finalizeWildFlowOperationBilling(c *gin.Context, operation *model.WildFlowOperation, artifacts []inferenceclient.Artifact) bool {
	err := service.FinalizeWildFlowOperationBilling(c.Request.Context(), operation, artifacts)
	if err == nil {
		return true
	}
	if errors.Is(err, service.ErrWildFlowMissingArtifact) {
		if updateErr := model.UpdateWildFlowOperationExecution(
			operation.OperationID,
			operation.JobID,
			"recovery_required",
			"missing_artifact",
		); updateErr != nil {
			wildFlowInternalError(c, updateErr)
			return false
		}
		operation.State = "recovery_required"
		operation.LastErrorCode = "missing_artifact"
		wildFlowJobError(c, http.StatusServiceUnavailable, "recovery_required", "job result requires recovery")
		return false
	}
	if errors.Is(err, service.ErrWildFlowInvalidArtifact) {
		if updateErr := model.UpdateWildFlowOperationExecution(
			operation.OperationID,
			operation.JobID,
			"recovery_required",
			"invalid_artifact",
		); updateErr != nil {
			wildFlowInternalError(c, updateErr)
			return false
		}
		operation.State = "recovery_required"
		operation.LastErrorCode = "invalid_artifact"
		wildFlowJobError(c, http.StatusServiceUnavailable, "recovery_required", "job result requires recovery")
		return false
	}
	if errors.Is(err, model.ErrWildFlowBillingStateConflict) {
		if updateErr := model.UpdateWildFlowOperationExecution(
			operation.OperationID,
			operation.JobID,
			"recovery_required",
			"billing_state_conflict",
		); updateErr != nil {
			wildFlowInternalError(c, updateErr)
			return false
		}
		operation.State = "recovery_required"
		operation.LastErrorCode = "billing_state_conflict"
		wildFlowJobError(c, http.StatusServiceUnavailable, "recovery_required", "job billing requires recovery")
		return false
	}
	wildFlowInternalError(c, err)
	return false
}

func writeWildFlowOperationError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrWildFlowIdempotencyRequired), errors.Is(err, service.ErrWildFlowInvalidParameters):
		wildFlowJobError(c, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, service.ErrWildFlowUnsupportedModel):
		wildFlowJobError(c, http.StatusBadRequest, "unsupported_model", err.Error())
	case errors.Is(err, service.ErrWildFlowIdempotencyConflict):
		wildFlowJobError(c, http.StatusConflict, "idempotency_conflict", err.Error())
	default:
		wildFlowInternalError(c, err)
	}
}

func writeWildFlowBillingError(c *gin.Context, err error) {
	if errors.Is(err, service.ErrWildFlowBillingInsufficientQuota) {
		wildFlowJobError(c, http.StatusForbidden, "insufficient_quota", "insufficient quota for this job")
		return
	}
	if errors.Is(err, model.ErrWildFlowBillingStateConflict) {
		wildFlowJobError(c, http.StatusConflict, "billing_state_conflict", "billing state conflicts with this request")
		return
	}
	wildFlowInternalError(c, err)
}

func writeWildFlowInferenceError(c *gin.Context, err error) {
	var retryable *inferenceclient.RetryableError
	if errors.As(err, &retryable) {
		retryAfter := strings.TrimSpace(retryable.RetryAfter)
		if retryAfter == "" {
			retryAfter = "5"
		}
		c.Header("Retry-After", retryAfter)
		wildFlowJobError(c, retryable.StatusCode, "inference_unavailable", "inference service is temporarily unavailable")
		return
	}
	var apiError *inferenceclient.APIError
	if errors.As(err, &apiError) && apiError.StatusCode == http.StatusNotFound {
		wildFlowJobError(c, http.StatusNotFound, "resource_not_found", "resource not found")
		return
	}
	logger.LogError(c.Request.Context(), "WildFlow inference request failed: "+err.Error())
	wildFlowJobError(c, http.StatusBadGateway, "inference_error", "inference request failed")
}

func wildFlowInternalError(c *gin.Context, err error) {
	logger.LogError(c.Request.Context(), "WildFlow operation failed: "+err.Error())
	wildFlowJobError(c, http.StatusInternalServerError, "server_error", "internal server error")
}

func wildFlowJobError(c *gin.Context, status int, code string, message string) {
	if status == http.StatusServiceUnavailable && c.Writer.Header().Get("Retry-After") == "" {
		c.Header("Retry-After", "5")
	}
	c.JSON(status, gin.H{
		"error": gin.H{
			"message": message,
			"type":    "wildflow_job_error",
			"code":    code,
		},
	})
}
