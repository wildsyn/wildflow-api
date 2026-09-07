package controller

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"crypto/sha256"
	"fmt"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relay"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const taskImageOperationContextKey = "wildflow_task_image_operation"

// prepareTaskImageOperation handles a replay before admission, storage checks,
// provider calls or billing. Only the unique insert winner continues submission.
func prepareTaskImageOperation(c *gin.Context, request pluginruntime.ProtocolRequestContext, deps pluginProtocolBridgeDeps) bool {
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if key == "" || len(key) > 200 {
		respondPluginProtocolError(c, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key is required for image tasks")
		return true
	}
	body, err := common.Marshal(map[string]any{"protocol": request.Protocol, "model": request.Model, "body": request.Body})
	if err != nil {
		respondPluginProtocolError(c, 400, "invalid_request_error", "Invalid image request")
		return true
	}
	userID := common.GetContextKeyInt(c, constant.ContextKeyUserId)
	keyDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
	existing, lookupErr := model.GetWildFlowOperationByUserAndKey(userID, keyDigest)
	if lookupErr != nil {
		c.Header("Retry-After", "3")
		respondPluginProtocolError(c, 503, "operation_unavailable", "Image submission record is temporarily unavailable")
		return true
	}
	if existing == nil {
		if err := deps.imageStorageReady(c.Request.Context(), userID); err != nil {
			c.Header("Retry-After", "10")
			respondPluginProtocolError(c, 503, "artifact_storage_unavailable", "Image storage is temporarily unavailable")
			return true
		}
	}
	id := strings.ReplaceAll(uuid.NewString(), "-", "")
	operation, created, err := model.ReserveTaskOperation(&model.WildFlowOperation{
		OperationID: "op-" + id, TaskID: "task_" + id,
		UserID: common.GetContextKeyInt(c, constant.ContextKeyUserId), TokenID: common.GetContextKeyInt(c, constant.ContextKeyTokenId),
		IdempotencyKeyDigest: keyDigest, RequestDigest: fmt.Sprintf("%x", sha256.Sum256(body)),
		ProductModelRef: request.Model, SubmissionLeaseExpiresAt: time.Now().Add(deps.submissionTimeout).Unix(),
	})
	if err != nil {
		if errors.Is(err, model.ErrTaskOperationConflict) {
			respondPluginProtocolError(c, 409, "idempotency_conflict", "Idempotency-Key was already used for a different request")
		} else {
			respondPluginProtocolError(c, 503, "operation_unavailable", "Image submission record is temporarily unavailable")
		}
		return true
	}
	if created {
		c.Set(taskImageOperationContextKey, operation)
		return false
	}
	task, exists, err := deps.getByTaskId(operation.UserID, operation.TaskID)
	if err != nil {
		respondPluginProtocolError(c, 503, "operation_unavailable", "Image task is temporarily unavailable")
		return true
	}
	if exists && task != nil {
		if task.Status == model.TaskStatusSuccess || task.Status == model.TaskStatusFailure {
			c.Params = append(c.Params, gin.Param{Key: "response_id", Value: "resp_" + strings.TrimPrefix(operation.TaskID, "task_")})
			retrieveTaskPluginResponse(c, deps)
			return true
		}
	}
	writePendingTaskImageOperation(c, operation)
	return true
}

func writePendingTaskImageOperation(c *gin.Context, operation *model.WildFlowOperation) {
	responseID := "resp_" + strings.TrimPrefix(operation.TaskID, "task_")
	c.Header("Location", "/v1/responses/"+responseID)
	c.Header("Retry-After", "3")
	if operation.State == "recovery_required" {
		respondPluginProtocolError(c, 503, "recovery_required", "Image submission outcome is unknown; keep the same Idempotency-Key and do not resubmit with a new key")
		return
	}
	machine := relay.NewPluginResponsesMachine(operation.TaskID, operation.ProductModelRef, operation.CreatedTime, relay.PluginProtocolLimits{})
	machine.SetBackground(true)
	c.JSON(http.StatusAccepted, machine.PendingResponse("IN_PROGRESS"))
}

func findPendingTaskImageOperation(c *gin.Context, userID int, taskID string) bool {
	if model.DB == nil {
		return false
	}
	operation, err := model.GetTaskOperationForUserAndTask(userID, taskID)
	if err != nil {
		respondPluginProtocolError(c, 503, "operation_unavailable", "Image submission record is temporarily unavailable")
		return true
	}
	if operation == nil {
		return false
	}
	writePendingTaskImageOperation(c, operation)
	return true
}
