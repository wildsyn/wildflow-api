package controller

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

const wildFlowJourneyReceiptBodyLimit = 8 << 10

type wildFlowJourneyReceiptRequest struct {
	OperationID string `json:"operation_id"`
	JobID       string `json:"job_id"`
	ArtifactID  string `json:"artifact_id"`
}

func MaterializeWildFlowJourneyReceipt(c *gin.Context) {
	if !authenticateWildFlowJourneyReceipt(c) {
		return
	}
	if !strings.HasPrefix(c.GetHeader("Content-Type"), "application/json") {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"error": "unsupported media type"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, wildFlowJourneyReceiptBodyLimit)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil || len(body) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid journey receipt request"})
		return
	}
	var raw map[string]any
	if err := common.Unmarshal(body, &raw); err != nil ||
		!exactKeys(raw, "operation_id", "job_id", "artifact_id") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid journey receipt request"})
		return
	}
	var request wildFlowJourneyReceiptRequest
	if err := common.Unmarshal(body, &request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid journey receipt request"})
		return
	}
	envelope, err := service.MaterializeWildFlowPublicJourneyReceipt(
		c.Request.Context(),
		request.OperationID,
		request.JobID,
		request.ArtifactID,
		time.Now().UTC(),
	)
	if err != nil {
		writeWildFlowJourneyReceiptError(c, err)
		return
	}
	c.JSON(http.StatusOK, envelope)
}

func GetWildFlowJourneyReceipt(c *gin.Context) {
	if !authenticateWildFlowJourneyReceipt(c) {
		return
	}
	envelope, err := service.GetWildFlowPublicJourneyReceipt(
		c.Request.Context(), c.Param("operation_id"),
	)
	if err != nil {
		writeWildFlowJourneyReceiptError(c, err)
		return
	}
	c.JSON(http.StatusOK, envelope)
}

func authenticateWildFlowJourneyReceipt(c *gin.Context) bool {
	token := os.Getenv("WILDFLOW_JOURNEY_RECEIPT_TOKEN")
	if len(token) < 32 {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "journey receipt service is not configured"})
		return false
	}
	const prefix = "Bearer "
	authorization := c.GetHeader("Authorization")
	if !strings.HasPrefix(authorization, prefix) ||
		!constantTimeEqual(strings.TrimPrefix(authorization, prefix), token) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthenticated"})
		return false
	}
	return true
}

func writeWildFlowJourneyReceiptError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, model.ErrWildFlowJourneyEvidenceInvalid):
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid journey receipt request"})
	case errors.Is(err, model.ErrWildFlowJourneyEvidenceNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "journey receipt evidence is incomplete"})
	case errors.Is(err, model.ErrWildFlowJourneyEvidenceConflict):
		c.JSON(http.StatusConflict, gin.H{"error": "journey receipt evidence conflicts"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "journey receipt persistence failed"})
	}
}
