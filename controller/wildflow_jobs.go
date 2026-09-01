package controller

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
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
	"github.com/google/uuid"
)

const wildFlowJobRequestLimit = 256 * 1024
const wildFlowInputArtifactLimit = int64(2 << 30)
const wildFlowArtifactVerificationConcurrency = 2

var wildFlowArtifactVerificationSlots = make(chan struct{}, wildFlowArtifactVerificationConcurrency)

func CreateWildFlowInputArtifact(c *gin.Context) {
	if !wildFlowTokenAllowsModel(c, service.WildFlowModelExamDualASR) {
		wildFlowJobError(c, http.StatusForbidden, "model_forbidden", "token is not allowed to use this internal workflow")
		return
	}
	mediaType, parameters, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "audio/flac" || len(parameters) != 0 {
		wildFlowJobError(c, http.StatusUnsupportedMediaType, "unsupported_media_type", "input artifact must be audio/flac")
		return
	}
	if c.Request.ContentLength > wildFlowInputArtifactLimit {
		wildFlowJobError(c, http.StatusRequestEntityTooLarge, "input_too_large", "input artifact exceeds 2 GiB")
		return
	}
	if c.Request.ContentLength < 4 {
		wildFlowJobError(c, http.StatusLengthRequired, "content_length_required", "a bounded Content-Length is required")
		return
	}
	digest := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(c.GetHeader("X-WildFlow-Content-SHA256"))), "sha256:")
	decodedDigest, err := hex.DecodeString(digest)
	if err != nil || len(decodedDigest) != sha256.Size {
		wildFlowJobError(c, http.StatusBadRequest, "invalid_digest", "a valid SHA-256 digest is required")
		return
	}
	client, err := newWildFlowInferenceClient()
	if err != nil {
		wildFlowJobError(c, http.StatusServiceUnavailable, "inference_unavailable", "inference service is unavailable")
		return
	}
	artifact, err := client.UploadInputArtifact(
		c.Request.Context(), c.Request.Body, c.Request.ContentLength, digest, wildFlowTenantRef(c.GetInt("id")),
	)
	if err != nil {
		writeWildFlowInputArtifactError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"artifact": gin.H{
		"id": artifact.ID, "media_type": artifact.MediaType, "size_bytes": artifact.SizeBytes,
		"sha256": artifact.SHA256, "retention_state": artifact.RetentionState,
	}})
}

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
	if operation.JobID == "" && wildFlowSubmissionNeedsReconciliation(operation) {
		if _, reconcileErr := service.ReconcileWildFlowSubmissionLease(operation.OperationID, time.Now().Unix()); reconcileErr != nil {
			logger.LogError(c.Request.Context(), "reconcile WildFlow submission lease: "+reconcileErr.Error())
		}
		operation, err = model.GetWildFlowOperationForUser(userID, operation.OperationID)
		if err != nil || operation == nil {
			if err == nil {
				err = errors.New("WildFlow operation disappeared during submission reconciliation")
			}
			wildFlowInternalError(c, err)
			return
		}
	}
	if !created && writeWildFlowExistingOperationReplay(c, operation) {
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
			if !created && writeWildFlowLegacyResultUnavailableRecovery(c, operation, getErr) {
				return
			}
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
		if operation.State == "succeeded" && writeWildFlowPersistedResult(c, operation, http.StatusOK, true) {
			return
		}
		writeWildFlowOperationHeaders(c, operation)
		c.JSON(http.StatusOK, wildFlowOperationResponse(operation, job.Artifacts))
		return
	}
	runtimeOfferingRef, err := service.ResolveWildFlowRuntimeOfferingRef(
		operation.ProductModelRef,
		operation.ModelVersionRef,
	)
	if err != nil {
		wildFlowInternalError(c, err)
		return
	}
	owner := "api:" + uuid.NewString()
	leaseToken := uuid.NewString()
	claimed, acquired, err := model.ClaimWildFlowOperationSubmission(
		operation.OperationID,
		owner,
		leaseToken,
		service.WildFlowSubmissionLeaseDeadline(),
	)
	if err != nil {
		wildFlowInternalError(c, err)
		return
	}
	operation = claimed
	if !acquired {
		if writeWildFlowExistingOperationReplay(c, operation) {
			return
		}
		writeWildFlowOperationHeaders(c, operation)
		c.Header("Retry-After", "5")
		c.JSON(http.StatusAccepted, wildFlowOperationResponse(operation, nil))
		return
	}

	client, err := newWildFlowInferenceClient()
	if err != nil {
		updated, transitionErr := model.MarkWildFlowOperationSubmissionRetryable(
			operation.OperationID,
			owner,
			leaseToken,
			"inference_unavailable",
			service.WildFlowSubmissionRetryDeadline(),
		)
		if transitionErr != nil {
			wildFlowInternalError(c, transitionErr)
			return
		}
		operation = updated
		c.Header("Retry-After", "5")
		wildFlowJobError(c, http.StatusServiceUnavailable, "inference_unavailable", "inference service is unavailable")
		return
	}
	reservedOperation, err := service.ReserveWildFlowOperationBilling(operation, request)
	if err != nil {
		if _, transitionErr := model.MarkWildFlowOperationSubmissionRetryable(
			operation.OperationID,
			owner,
			leaseToken,
			"billing_reservation_failed",
			service.WildFlowSubmissionRetryDeadline(),
		); transitionErr != nil {
			wildFlowInternalError(c, transitionErr)
			return
		}
		writeWildFlowBillingError(c, err)
		return
	}
	operation = reservedOperation
	operation, err = model.BeginWildFlowOperationSubmission(operation.OperationID, owner, leaseToken)
	if err != nil {
		if errors.Is(err, model.ErrWildFlowSubmissionLeaseLost) {
			current, loadErr := model.GetWildFlowOperationForUser(userID, operation.OperationID)
			if loadErr != nil || current == nil {
				wildFlowInternalError(c, errors.Join(err, loadErr))
				return
			}
			if !writeWildFlowExistingOperationReplay(c, current) {
				writeWildFlowOperationHeaders(c, current)
				c.Header("Retry-After", "5")
				c.JSON(http.StatusAccepted, wildFlowOperationResponse(current, nil))
			}
			return
		}
		wildFlowInternalError(c, err)
		return
	}
	deadlineAfter := 30 * time.Minute
	if operation.ProductModelRef == service.WildFlowModelExamDualASR {
		deadlineAfter = 6 * time.Hour
	}
	job, err := client.SubmitJob(c.Request.Context(), inferenceclient.JobCreateRequest{
		OperationID:          operation.OperationID,
		RequestDigest:        operation.RequestDigest,
		RequestID:            operation.RequestID,
		TenantRef:            wildFlowTenantRef(userID),
		ProductModelRef:      runtimeOfferingRef,
		ModelVersionRef:      operation.ModelVersionRef,
		InputArtifactIDs:     request.InputArtifactIDs,
		Parameters:           request.Parameters,
		DeadlineAt:           time.Now().UTC().Add(deadlineAfter),
		CallbackCapabilities: []string{},
	})
	if err != nil {
		var retryable *inferenceclient.RetryableError
		if errors.As(err, &retryable) {
			updated, updateErr := model.MarkWildFlowOperationSubmissionRetryable(
				operation.OperationID,
				owner,
				leaseToken,
				"inference_unavailable",
				service.WildFlowSubmissionRetryDeadline(),
			)
			if updateErr != nil {
				wildFlowInternalError(c, updateErr)
				return
			}
			operation = updated
			writeWildFlowInferenceError(c, err)
			return
		}
		markWildFlowSubmissionRecoveryRequired(c, operation, owner, leaseToken, "submission_unknown", err)
		return
	}
	operation, err = model.CompleteWildFlowOperationSubmission(operation.OperationID, owner, leaseToken, job.ID, job.State)
	if err != nil {
		logger.LogError(c.Request.Context(), "persist accepted WildFlow submission: "+err.Error())
		wildFlowJobError(c, http.StatusServiceUnavailable, "recovery_required", "job submission requires recovery")
		return
	}
	if !finalizeWildFlowOperationBilling(c, operation, job.Artifacts) {
		return
	}
	status := http.StatusAccepted
	writeWildFlowOperationHeaders(c, operation)
	if operation.State == "succeeded" && writeWildFlowPersistedResult(c, operation, status, false) {
		return
	}
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

func writeWildFlowExistingOperationReplay(c *gin.Context, operation *model.WildFlowOperation) bool {
	if operation.State == "succeeded" && operation.ResultJSON != "" {
		return writeWildFlowPersistedResult(c, operation, http.StatusOK, true)
	}
	switch operation.State {
	case "submitting":
		phase := operation.SubmissionPhase
		if phase == "" {
			phase = model.WildFlowSubmissionPhasePrepared
		}
		leaseActive := operation.SubmissionLeaseToken != "" && operation.SubmissionLeaseExpiresAt > time.Now().Unix()
		if operation.JobID == "" && !leaseActive &&
			(phase == model.WildFlowSubmissionPhasePrepared || phase == model.WildFlowSubmissionPhaseRetryable) {
			return false
		}
		writeWildFlowOperationHeaders(c, operation)
		c.Header("Retry-After", "5")
		c.JSON(http.StatusAccepted, wildFlowOperationResponse(operation, nil))
		return true
	case "queued", "running", "cancelling":
		writeWildFlowOperationHeaders(c, operation)
		c.Header("Retry-After", "5")
		c.JSON(http.StatusAccepted, wildFlowOperationResponse(operation, nil))
		return true
	case "recovery_required", "failed", "cancelled":
		writeWildFlowOperationHeaders(c, operation)
		c.JSON(http.StatusOK, wildFlowOperationResponse(operation, nil))
		return true
	default:
		return false
	}
}

func wildFlowSubmissionNeedsReconciliation(operation *model.WildFlowOperation) bool {
	if operation == nil || operation.JobID != "" {
		return false
	}
	now := time.Now().Unix()
	if operation.State == "submitting" && operation.SubmissionPhase == "" {
		return true
	}
	if operation.State == "submitting" && operation.SubmissionPhase == model.WildFlowSubmissionPhaseSubmitting {
		return operation.SubmissionLeaseExpiresAt > 0 && operation.SubmissionLeaseExpiresAt <= now
	}
	return operation.State == "submitting" &&
		(operation.SubmissionPhase == model.WildFlowSubmissionPhasePrepared || operation.SubmissionPhase == model.WildFlowSubmissionPhaseRetryable) &&
		operation.SubmissionRetryUntil > 0 && operation.SubmissionRetryUntil <= now
}

func writeWildFlowPersistedResult(c *gin.Context, operation *model.WildFlowOperation, status int, replay bool) bool {
	if operation == nil || operation.ResultJSON == "" {
		return false
	}
	if err := service.EnsureWildFlowOperationResultRetention(operation); err != nil {
		wildFlowInternalError(c, err)
		return true
	}
	writeWildFlowOperationHeaders(c, operation)
	if operation.ResultExpiresAt > 0 && time.Now().Unix() >= operation.ResultExpiresAt {
		wildFlowJobError(c, http.StatusGone, "result_expired", "idempotent result expired; use a new Idempotency-Key")
		return true
	}
	if replay {
		c.Header("X-Idempotent-Replay", "true")
	}
	c.Data(status, "application/json; charset=utf-8", []byte(operation.ResultJSON))
	return true
}

func GetWildFlowJob(c *gin.Context) {
	operation, ok := loadWildFlowOperation(c)
	if !ok {
		return
	}
	if wildFlowSubmissionNeedsReconciliation(operation) {
		if _, err := service.ReconcileWildFlowSubmissionLease(operation.OperationID, time.Now().Unix()); err != nil {
			wildFlowInternalError(c, err)
			return
		}
		reloaded, err := model.GetWildFlowOperationForUser(operation.UserID, operation.OperationID)
		if err != nil {
			wildFlowInternalError(c, err)
			return
		}
		if reloaded == nil {
			wildFlowJobError(c, http.StatusNotFound, "job_not_found", "job not found")
			return
		}
		operation = reloaded
	}
	if operation.State == "succeeded" && writeWildFlowPersistedResult(c, operation, http.StatusOK, false) {
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
		if writeWildFlowLegacyResultUnavailableRecovery(c, operation, err) {
			return
		}
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
	if operation.State == "succeeded" && writeWildFlowPersistedResult(c, operation, http.StatusOK, false) {
		return
	}
	c.JSON(http.StatusOK, wildFlowOperationResponse(operation, job.Artifacts))
}

func writeWildFlowLegacyResultUnavailableRecovery(c *gin.Context, operation *model.WildFlowOperation, err error) bool {
	var apiError *inferenceclient.APIError
	if operation == nil || operation.State != "succeeded" || operation.ResultJSON != "" ||
		!errors.As(err, &apiError) || apiError.StatusCode != http.StatusNotFound {
		return false
	}
	if updateErr := model.UpdateWildFlowOperationExecution(
		operation.OperationID,
		operation.JobID,
		"recovery_required",
		"result_unavailable",
	); updateErr != nil {
		wildFlowInternalError(c, updateErr)
		return true
	}
	operation.State = "recovery_required"
	operation.LastErrorCode = "result_unavailable"
	writeWildFlowOperationHeaders(c, operation)
	c.JSON(http.StatusOK, wildFlowOperationResponse(operation, nil))
	return true
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
	select {
	case wildFlowArtifactVerificationSlots <- struct{}{}:
		defer func() { <-wildFlowArtifactVerificationSlots }()
	default:
		wildFlowJobError(c, http.StatusServiceUnavailable, "artifact_download_busy", "artifact download is busy")
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
	} else if artifact.MediaType == "application/json" {
		filename = artifact.ID + ".json"
	}
	file, err := os.CreateTemp("", "wildflow-artifact-*")
	if err != nil {
		wildFlowInternalError(c, err)
		return
	}
	defer func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}()
	digest := sha256.New()
	written, err := io.Copy(io.MultiWriter(file, digest), content.Body)
	if err != nil {
		if updateErr := model.UpdateWildFlowOperationExecution(
			operation.OperationID,
			operation.JobID,
			"recovery_required",
			"artifact_stream_error",
		); updateErr != nil {
			logger.LogError(c.Request.Context(), "persist WildFlow artifact stream recovery: "+updateErr.Error())
		}
		logger.LogError(c.Request.Context(), "stream WildFlow artifact: "+err.Error())
		wildFlowJobError(c, http.StatusServiceUnavailable, "artifact_stream_error", "artifact content requires recovery")
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		wildFlowInternalError(c, err)
		return
	}
	magic := make([]byte, 12)
	magicSize, err := file.Read(magic)
	if err != nil && err != io.EOF {
		wildFlowInternalError(c, err)
		return
	}
	actualDigest := hex.EncodeToString(digest.Sum(nil))
	if written != artifact.SizeBytes ||
		actualDigest != strings.ToLower(artifact.SHA256) ||
		!validWildFlowArtifactMagic(artifact.MediaType, magic[:magicSize]) {
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
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		wildFlowInternalError(c, err)
		return
	}
	c.DataFromReader(http.StatusOK, written, content.MediaType, file, map[string]string{
		"Content-Disposition": fmt.Sprintf("attachment; filename=%q", safeWildFlowFilename(filename)),
	})
	if operation.ProductModelRef != service.WildFlowModelExamDualASR {
		return
	}
	tenantDigest := sha256.Sum256([]byte(wildFlowTenantRef(operation.UserID)))
	if _, _, err := model.RecordWildFlowArtifactDownloadReceipt(&model.WildFlowArtifactDownloadReceipt{
		OperationID: operation.OperationID, JobID: operation.JobID, ArtifactID: artifact.ID,
		UserID: operation.UserID, TenantRefSHA256: hex.EncodeToString(tenantDigest[:]),
		ArtifactMediaType: artifact.MediaType, ArtifactSizeBytes: written,
		ArtifactSHA256: actualDigest, CompletedAt: time.Now().UTC(),
	}); err != nil {
		logger.LogError(c.Request.Context(), "persist WildFlow artifact download receipt: "+err.Error())
	}
}

func validWildFlowArtifactMagic(mediaType string, magic []byte) bool {
	switch mediaType {
	case "application/json":
		for _, value := range magic {
			if value == ' ' || value == '\n' || value == '\r' || value == '\t' {
				continue
			}
			return value == '{' || value == '['
		}
		return false
	case "audio/mpeg":
		return bytes.HasPrefix(magic, []byte("ID3")) ||
			(len(magic) >= 2 && magic[0] == 0xff && magic[1]&0xe0 == 0xe0)
	case "audio/wav":
		return len(magic) >= 12 && bytes.Equal(magic[:4], []byte("RIFF")) && bytes.Equal(magic[8:12], []byte("WAVE"))
	case "image/png":
		return bytes.HasPrefix(magic, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	case "image/jpeg":
		return len(magic) >= 3 && magic[0] == 0xff && magic[1] == 0xd8 && magic[2] == 0xff
	case "image/gif":
		return bytes.HasPrefix(magic, []byte("GIF87a")) || bytes.HasPrefix(magic, []byte("GIF89a"))
	case "image/webp":
		return len(magic) >= 12 && bytes.Equal(magic[:4], []byte("RIFF")) && bytes.Equal(magic[8:12], []byte("WEBP"))
	default:
		return false
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
	if !authorizeWildFlowOperationModel(c, operation) {
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
	if !authorizeWildFlowOperationModel(c, operation) {
		return inferenceclient.Artifact{}, nil, false
	}
	internalTrialReady := operation.ProductModelRef == service.WildFlowModelExamDualASR &&
		operation.BillingState == model.WildFlowBillingStatePending
	retailResultReady := operation.ResultJSON != "" &&
		(operation.BillingState == model.WildFlowBillingStateReserved || operation.BillingState == model.WildFlowBillingStateSettled)
	if operation.State != "succeeded" || (!retailResultReady && !internalTrialReady) {
		wildFlowJobError(c, http.StatusConflict, "artifact_not_ready", "artifact is not ready")
		return inferenceclient.Artifact{}, nil, false
	}
	if operation.ResultExpiresAt > 0 && time.Now().Unix() >= operation.ResultExpiresAt {
		wildFlowJobError(c, http.StatusGone, "result_expired", "operation result expired")
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

func authorizeWildFlowOperationModel(c *gin.Context, operation *model.WildFlowOperation) bool {
	if operation.ProductModelRef != service.WildFlowModelExamDualASR ||
		wildFlowTokenAllowsModel(c, operation.ProductModelRef) {
		return true
	}
	wildFlowJobError(c, http.StatusForbidden, "model_forbidden", "token is not allowed to use this internal workflow")
	return false
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
	if err := common.Unmarshal(body, &fields); err != nil || len(fields) < 2 || len(fields) > 3 {
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
	if inputField, exists := fields["input_artifact_ids"]; exists {
		if err := common.Unmarshal(inputField, &request.InputArtifactIDs); err != nil {
			return service.WildFlowJobRequest{}, err
		}
	}
	for key := range fields {
		if key != "model" && key != "parameters" && key != "input_artifact_ids" {
			return service.WildFlowJobRequest{}, errors.New("invalid job request")
		}
	}
	return request, nil
}

func wildFlowTenantRef(userID int) string {
	return "user:" + strconv.Itoa(userID)
}

func publicWildFlowArtifact(artifact inferenceclient.Artifact) gin.H {
	return gin.H(service.PublicWildFlowArtifact(artifact))
}

func wildFlowOperationResponse(operation *model.WildFlowOperation, artifacts []inferenceclient.Artifact) gin.H {
	return gin.H(service.WildFlowOperationResult(operation, artifacts))
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

func markWildFlowSubmissionRecoveryRequired(
	c *gin.Context,
	operation *model.WildFlowOperation,
	owner string,
	leaseToken string,
	code string,
	cause error,
) {
	updated, err := model.MarkWildFlowOperationSubmissionRecoveryRequired(operation.OperationID, owner, leaseToken, code)
	if err != nil {
		logger.LogError(c.Request.Context(), "persist WildFlow recovery state: "+err.Error())
	} else {
		*operation = *updated
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

func writeWildFlowInputArtifactError(c *gin.Context, err error) {
	var apiError *inferenceclient.APIError
	if errors.As(err, &apiError) {
		switch apiError.StatusCode {
		case http.StatusBadRequest, http.StatusLengthRequired, http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType:
			wildFlowJobError(c, apiError.StatusCode, "invalid_input_artifact", "input artifact was rejected")
			return
		}
	}
	writeWildFlowInferenceError(c, err)
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
