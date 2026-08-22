package controller

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

const wildFlowUsageEventBodyLimit = 64 << 10

type wildFlowUsageEventEnvelope struct {
	EventID       string                    `json:"event_id"`
	AggregateType string                    `json:"aggregate_type"`
	AggregateID   string                    `json:"aggregate_id"`
	EventType     string                    `json:"event_type"`
	Payload       wildFlowUsageEventPayload `json:"payload"`
}

type wildFlowUsageEventPayload struct {
	UsageEventID    string    `json:"usage_event_id"`
	OperationID     string    `json:"operation_id"`
	JobID           string    `json:"job_id"`
	AttemptID       string    `json:"attempt_id"`
	ModelVersionRef string    `json:"model_version_ref"`
	ChannelType     string    `json:"channel_type"`
	Kind            string    `json:"kind"`
	Quantity        int64     `json:"quantity"`
	Unit            string    `json:"unit"`
	StartedAt       time.Time `json:"started_at"`
	EndedAt         time.Time `json:"ended_at"`
	EvidenceRef     string    `json:"evidence_ref"`
}

func ReceiveWildFlowUsageEvent(c *gin.Context) {
	token := os.Getenv("WILDFLOW_USAGE_EVENT_TOKEN")
	if len(token) < 32 {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage event receiver is not configured"})
		return
	}
	const prefix = "Bearer "
	authorization := c.GetHeader("Authorization")
	if !strings.HasPrefix(authorization, prefix) || !constantTimeEqual(strings.TrimPrefix(authorization, prefix), token) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthenticated"})
		return
	}
	if !strings.HasPrefix(c.GetHeader("Content-Type"), "application/json") {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"error": "unsupported media type"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, wildFlowUsageEventBodyLimit)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil || len(body) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid usage event"})
		return
	}
	var raw map[string]any
	if err := common.Unmarshal(body, &raw); err != nil || !exactKeys(raw, "event_id", "aggregate_type", "aggregate_id", "event_type", "payload") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid usage event"})
		return
	}
	rawPayload, ok := raw["payload"].(map[string]any)
	if !ok || !exactKeys(rawPayload, "usage_event_id", "operation_id", "job_id", "attempt_id", "model_version_ref",
		"channel_type", "kind", "quantity", "unit", "started_at", "ended_at", "evidence_ref") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid usage event"})
		return
	}
	var event wildFlowUsageEventEnvelope
	if err := common.Unmarshal(body, &event); err != nil || !validUsageEvent(event) ||
		c.GetHeader("Idempotency-Key") != event.EventID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid usage event"})
		return
	}
	canonical, err := common.Marshal(event)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid usage event"})
		return
	}
	digest := sha256.Sum256(canonical)
	replayed, err := model.RecordWildFlowUsageEvent(&model.WildFlowUsageEvent{
		EventID: event.EventID, AggregateType: event.AggregateType, AggregateID: event.AggregateID,
		EventType: event.EventType, PayloadDigest: stringHex(digest[:]),
		OperationID: event.Payload.OperationID, JobID: event.Payload.JobID, AttemptID: event.Payload.AttemptID,
		ModelVersionRef: event.Payload.ModelVersionRef, ChannelType: event.Payload.ChannelType,
		Kind: event.Payload.Kind, Quantity: event.Payload.Quantity, Unit: event.Payload.Unit,
		StartedAt: event.Payload.StartedAt, EndedAt: event.Payload.EndedAt, EvidenceRef: event.Payload.EvidenceRef,
	})
	if errors.Is(err, model.ErrWildFlowUsageEventConflict) {
		c.JSON(http.StatusConflict, gin.H{"error": "usage event conflict"})
		return
	}
	if errors.Is(err, model.ErrWildFlowUsageEventUnknown) {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown operation"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "usage event persistence failed"})
		return
	}
	if replayed {
		c.Header("X-WildFlow-Event-Replayed", "true")
		c.JSON(http.StatusOK, gin.H{"accepted": true, "replayed": true})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"accepted": true, "replayed": false})
}

func constantTimeEqual(candidate, expected string) bool {
	candidateDigest := sha256.Sum256([]byte(candidate))
	expectedDigest := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(candidateDigest[:], expectedDigest[:]) == 1
}

func exactKeys(values map[string]any, keys ...string) bool {
	if len(values) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, ok := values[key]; !ok {
			return false
		}
	}
	return true
}

func validUsageEvent(event wildFlowUsageEventEnvelope) bool {
	payload := event.Payload
	if !safeUsageField(event.EventID, 128) || event.AggregateType != "job" ||
		!safeUsageField(event.AggregateID, 128) || event.EventType != "usage.recorded.v1" ||
		event.EventID != payload.UsageEventID || event.AggregateID != payload.JobID ||
		!safeUsageField(payload.OperationID, 64) || !safeUsageField(payload.JobID, 128) ||
		!safeUsageField(payload.AttemptID, 128) || !safeUsageField(payload.ModelVersionRef, 256) ||
		(payload.ChannelType != "provider_connector" && payload.ChannelType != "gpu_agent") ||
		payload.Quantity < 0 || payload.EndedAt.Before(payload.StartedAt) || payload.StartedAt.IsZero() ||
		!safeUsageField(payload.EvidenceRef, 256) {
		return false
	}
	return (payload.Kind == "provider_tokens" && payload.Unit == "token") ||
		(payload.Kind == "characters" && payload.Unit == "character" && payload.Quantity > 0) ||
		(payload.Kind == "images" && payload.Unit == "image" && payload.Quantity == 1) ||
		(payload.Kind == "audio_duration" && payload.Unit == "millisecond" && payload.Quantity > 0)
}

func safeUsageField(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && strings.TrimSpace(value) == value &&
		!strings.ContainsAny(value, "\x00\r\n")
}

func stringHex(value []byte) string {
	const alphabet = "0123456789abcdef"
	result := make([]byte, len(value)*2)
	for index, item := range value {
		result[index*2] = alphabet[item>>4]
		result[index*2+1] = alphabet[item&0x0f]
	}
	return string(result)
}
