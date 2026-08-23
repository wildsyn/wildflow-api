package service

import (
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/internal/inferenceclient"
	"github.com/QuantumNous/new-api/model"
)

const (
	wildFlowOperationResultRetentionDefault = 30 * 24 * time.Hour
	wildFlowOperationResultRetentionMinimum = time.Minute
	wildFlowOperationResultRetentionMaximum = 365 * 24 * time.Hour
)

func wildFlowOperationResultRetention() time.Duration {
	seconds := common.GetEnvOrDefault(
		"WILDFLOW_OPERATION_RESULT_RETENTION_SECONDS",
		int(wildFlowOperationResultRetentionDefault/time.Second),
	)
	if seconds < int(wildFlowOperationResultRetentionMinimum/time.Second) ||
		seconds > int(wildFlowOperationResultRetentionMaximum/time.Second) {
		return wildFlowOperationResultRetentionDefault
	}
	return time.Duration(seconds) * time.Second
}

func PersistWildFlowOperationResult(
	operation *model.WildFlowOperation,
	artifacts []inferenceclient.Artifact,
) (*model.WildFlowOperation, error) {
	if operation == nil || operation.State != "succeeded" {
		return nil, fmt.Errorf("invalid successful WildFlow operation")
	}
	resultJSON, err := common.Marshal(WildFlowOperationResult(operation, artifacts))
	if err != nil {
		return nil, fmt.Errorf("encode WildFlow operation result: %w", err)
	}
	retention := wildFlowOperationResultRetention()
	if operation.ResultRetentionSeconds >= int64(wildFlowOperationResultRetentionMinimum/time.Second) &&
		operation.ResultRetentionSeconds <= int64(wildFlowOperationResultRetentionMaximum/time.Second) {
		retention = time.Duration(operation.ResultRetentionSeconds) * time.Second
	}
	stored, err := model.StoreWildFlowOperationResult(
		operation.OperationID,
		string(resultJSON),
		time.Now().Add(retention).Unix(),
	)
	if err != nil {
		return nil, fmt.Errorf("persist WildFlow operation result: %w", err)
	}
	*operation = *stored
	return stored, nil
}

func EnsureWildFlowOperationResultRetention(operation *model.WildFlowOperation) error {
	if operation == nil || operation.ResultJSON == "" || operation.ResultExpiresAt > 0 {
		return nil
	}
	stored, err := model.StoreWildFlowOperationResult(
		operation.OperationID,
		operation.ResultJSON,
		time.Now().Add(wildFlowOperationResultRetention()).Unix(),
	)
	if err != nil {
		return fmt.Errorf("backfill WildFlow operation result retention: %w", err)
	}
	*operation = *stored
	return nil
}

func WildFlowOperationResult(
	operation *model.WildFlowOperation,
	artifacts []inferenceclient.Artifact,
) map[string]any {
	response := map[string]any{
		"id":                operation.OperationID,
		"operation_id":      operation.OperationID,
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
		publicArtifacts := make([]map[string]any, 0, len(artifacts))
		for _, artifact := range artifacts {
			publicArtifacts = append(publicArtifacts, PublicWildFlowArtifact(artifact))
		}
		response["artifacts"] = publicArtifacts
	}
	return response
}

func PublicWildFlowArtifact(artifact inferenceclient.Artifact) map[string]any {
	return map[string]any{
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
		"schema_version", "model_version_ref", "model_revision", "vibevoice_model_revision",
		"faster_whisper_model_revision", "runtime_version_ref", "duration_seconds", "source_artifact_id",
	} {
		if value, ok := metadata[key]; ok {
			public[key] = value
		}
	}
	return public
}
