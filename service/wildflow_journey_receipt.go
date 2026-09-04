package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

type WildFlowPublicJourneyReceipt struct {
	SchemaVersion        int    `json:"schema_version"`
	State                string `json:"state"`
	ReceiptCreatedAt     string `json:"receipt_created_at"`
	TenantRefSHA256      string `json:"tenant_ref_sha256"`
	OperationID          string `json:"operation_id"`
	OperationCreatedAt   string `json:"operation_created_at"`
	OperationState       string `json:"operation_state"`
	RequestID            string `json:"request_id"`
	IdempotencyKeyDigest string `json:"idempotency_key_digest"`
	RequestDigest        string `json:"request_digest"`
	PublicModelRef       string `json:"public_model_ref"`
	RuntimeOfferingRef   string `json:"runtime_offering_ref"`
	ModelVersionRef      string `json:"model_version_ref"`
	JobID                string `json:"job_id"`
	BillingMode          string `json:"billing_mode"`
	BillingState         string `json:"billing_state"`
	UsageEventID         string `json:"usage_event_id"`
	UsagePayloadDigest   string `json:"usage_payload_digest"`
	UsageIngestedAt      string `json:"usage_ingested_at"`
	ArtifactID           string `json:"artifact_id"`
	ArtifactMediaType    string `json:"artifact_media_type"`
	ArtifactSizeBytes    int64  `json:"artifact_size_bytes"`
	ArtifactSHA256       string `json:"artifact_sha256"`
	ArtifactDownloadedAt string `json:"artifact_downloaded_at"`
}

type WildFlowPublicJourneyReceiptEnvelope struct {
	Receipt       WildFlowPublicJourneyReceipt `json:"public_journey_receipt"`
	ReceiptSHA256 string                       `json:"public_journey_receipt_sha256"`
}

var wildFlowJourneySafeRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9./:@_+\-]{0,255}$`)

type storedWildFlowOperationResult struct {
	ID              string `json:"id"`
	OperationID     string `json:"operation_id"`
	RequestID       string `json:"request_id"`
	Model           string `json:"model"`
	ModelVersionRef string `json:"model_version_ref"`
	JobID           string `json:"job_id"`
	State           string `json:"state"`
	Artifacts       []struct {
		ID        string `json:"id"`
		JobID     string `json:"job_id"`
		MediaType string `json:"media_type"`
		SizeBytes int64  `json:"size_bytes"`
		SHA256    string `json:"sha256"`
	} `json:"artifacts"`
}

func MaterializeWildFlowPublicJourneyReceipt(
	ctx context.Context,
	operationID string,
	jobID string,
	artifactID string,
	receiptCreatedAt time.Time,
) (*WildFlowPublicJourneyReceiptEnvelope, error) {
	if receiptCreatedAt.IsZero() {
		return nil, model.ErrWildFlowJourneyEvidenceInvalid
	}
	receiptCreatedAt = receiptCreatedAt.UTC().Truncate(time.Millisecond)
	record, err := model.LoadOrCreateWildFlowPublicJourneyReceipt(
		ctx,
		operationID,
		jobID,
		artifactID,
		func(evidence *model.WildFlowJourneyEvidence) (*model.WildFlowJourneyReceiptMaterial, error) {
			receipt, buildErr := buildWildFlowPublicJourneyReceipt(
				evidence, artifactID, receiptCreatedAt,
			)
			if buildErr != nil {
				return nil, buildErr
			}
			canonical, canonicalErr := canonicalWildFlowPublicJourneyReceipt(*receipt)
			if canonicalErr != nil {
				return nil, errors.Join(model.ErrWildFlowJourneyEvidenceConflict, canonicalErr)
			}
			digest := sha256.Sum256(canonical)
			return &model.WildFlowJourneyReceiptMaterial{
				ReceiptJSON: string(canonical), ReceiptSHA256: hex.EncodeToString(digest[:]),
				ReceiptCreatedAt: receiptCreatedAt,
			}, nil
		},
	)
	if err != nil {
		return nil, err
	}
	return wildFlowPublicJourneyReceiptEnvelopeFromRecord(record)
}

func GetWildFlowPublicJourneyReceipt(
	ctx context.Context,
	operationID string,
) (*WildFlowPublicJourneyReceiptEnvelope, error) {
	record, err := model.LoadWildFlowPublicJourneyReceiptRecord(ctx, operationID)
	if err != nil {
		return nil, err
	}
	return wildFlowPublicJourneyReceiptEnvelopeFromRecord(record)
}

func wildFlowPublicJourneyReceiptEnvelopeFromRecord(
	record *model.WildFlowPublicJourneyReceiptRecord,
) (*WildFlowPublicJourneyReceiptEnvelope, error) {
	if record == nil {
		return nil, model.ErrWildFlowJourneyEvidenceConflict
	}
	var receipt WildFlowPublicJourneyReceipt
	if err := common.UnmarshalJsonStr(record.ReceiptJSON, &receipt); err != nil {
		return nil, model.ErrWildFlowJourneyEvidenceConflict
	}
	canonical, err := canonicalWildFlowPublicJourneyReceipt(receipt)
	if err != nil || string(canonical) != record.ReceiptJSON ||
		!validStoredWildFlowPublicJourneyReceipt(record, &receipt) {
		return nil, model.ErrWildFlowJourneyEvidenceConflict
	}
	return &WildFlowPublicJourneyReceiptEnvelope{
		Receipt: receipt, ReceiptSHA256: record.ReceiptSHA256,
	}, nil
}

func validStoredWildFlowPublicJourneyReceipt(
	record *model.WildFlowPublicJourneyReceiptRecord,
	receipt *WildFlowPublicJourneyReceipt,
) bool {
	if record == nil || receipt == nil || receipt.SchemaVersion != 1 ||
		receipt.State != "public_journey_succeeded" || receipt.OperationState != "succeeded" ||
		!validWildFlowJourneyReceiptBilling(receipt.BillingMode, receipt.BillingState) ||
		receipt.ArtifactMediaType != "application/json" || receipt.ArtifactSizeBytes <= 0 ||
		receipt.OperationID != record.OperationID || receipt.JobID != record.JobID ||
		receipt.ArtifactID != record.ArtifactID ||
		!validWildFlowASRJourneyIdentity(receipt.PublicModelRef, receipt.ModelVersionRef) {
		return false
	}
	for _, value := range []string{
		receipt.OperationID,
		receipt.RequestID,
		receipt.PublicModelRef,
		receipt.RuntimeOfferingRef,
		receipt.ModelVersionRef,
		receipt.JobID,
		receipt.UsageEventID,
		receipt.ArtifactID,
	} {
		if !validWildFlowJourneySafeRef(value) {
			return false
		}
	}
	for _, value := range []string{
		receipt.TenantRefSHA256,
		receipt.IdempotencyKeyDigest,
		receipt.RequestDigest,
		receipt.UsagePayloadDigest,
		receipt.ArtifactSHA256,
	} {
		if !validWildFlowJourneyDigest(value) {
			return false
		}
	}
	runtimeOfferingRef, err := ResolveWildFlowRuntimeOfferingRef(
		receipt.PublicModelRef, receipt.ModelVersionRef,
	)
	if err != nil || runtimeOfferingRef != receipt.RuntimeOfferingRef {
		return false
	}
	operationCreatedAt, err := time.Parse(time.RFC3339Nano, receipt.OperationCreatedAt)
	if err != nil {
		return false
	}
	usageIngestedAt, err := time.Parse(time.RFC3339Nano, receipt.UsageIngestedAt)
	if err != nil {
		return false
	}
	artifactDownloadedAt, err := time.Parse(time.RFC3339Nano, receipt.ArtifactDownloadedAt)
	if err != nil {
		return false
	}
	receiptCreatedAt, err := time.Parse(time.RFC3339Nano, receipt.ReceiptCreatedAt)
	return err == nil &&
		!operationCreatedAt.After(usageIngestedAt) &&
		!usageIngestedAt.After(artifactDownloadedAt) &&
		!artifactDownloadedAt.After(receiptCreatedAt) &&
		receiptCreatedAt.Equal(record.ReceiptCreatedAt.UTC())
}

func buildWildFlowPublicJourneyReceipt(
	evidence *model.WildFlowJourneyEvidence,
	artifactID string,
	receiptCreatedAt time.Time,
) (*WildFlowPublicJourneyReceipt, error) {
	operation := &evidence.Operation
	usage := &evidence.UsageEvent
	download := &evidence.DownloadReceipt
	runtimeOfferingRef, err := ResolveWildFlowRuntimeOfferingRef(
		operation.ProductModelRef, operation.ModelVersionRef,
	)
	if err != nil {
		return nil, model.ErrWildFlowJourneyEvidenceConflict
	}
	billingMode, billingOK := wildFlowJourneyOperationBillingMode(operation, usage)
	if !billingOK || operation.State != "succeeded" ||
		operation.ResultJSON == "" || operation.ResultValidatedTime <= 0 ||
		operation.ResultExpiresAt <= receiptCreatedAt.Unix() ||
		operation.CreatedTime <= 0 ||
		!validWildFlowJourneySafeRef(operation.OperationID) ||
		!validWildFlowJourneySafeRef(operation.RequestID) ||
		!validWildFlowJourneySafeRef(operation.ProductModelRef) ||
		!validWildFlowJourneySafeRef(runtimeOfferingRef) ||
		!validWildFlowJourneySafeRef(operation.ModelVersionRef) ||
		!validWildFlowJourneySafeRef(operation.JobID) ||
		!validWildFlowJourneyDigest(operation.IdempotencyKeyDigest) ||
		!validWildFlowJourneyDigest(operation.RequestDigest) {
		return nil, model.ErrWildFlowJourneyEvidenceConflict
	}

	var result storedWildFlowOperationResult
	if err := common.UnmarshalJsonStr(operation.ResultJSON, &result); err != nil ||
		result.ID != operation.OperationID || result.OperationID != operation.OperationID ||
		result.RequestID != operation.RequestID || result.Model != operation.ProductModelRef ||
		result.ModelVersionRef != operation.ModelVersionRef || result.JobID != operation.JobID ||
		result.State != operation.State || len(result.Artifacts) != 1 {
		return nil, model.ErrWildFlowJourneyEvidenceConflict
	}
	artifact := result.Artifacts[0]
	if artifact.ID != artifactID || artifact.JobID != operation.JobID ||
		artifact.MediaType != "application/json" || artifact.SizeBytes <= 0 ||
		!validWildFlowJourneyDigest(artifact.SHA256) {
		return nil, model.ErrWildFlowJourneyEvidenceConflict
	}

	expectedTenantDigest := sha256.Sum256([]byte("user:" + decimalWildFlowUserID(operation.UserID)))
	expectedTenantRefSHA256 := hex.EncodeToString(expectedTenantDigest[:])
	usageIngestedAt := usage.IngestedAt.UTC().Truncate(time.Millisecond)
	downloadedAt := download.CompletedAt.UTC().Truncate(time.Millisecond)
	if download.OperationID != operation.OperationID || download.JobID != operation.JobID ||
		download.ArtifactID != artifact.ID || download.UserID != operation.UserID ||
		download.TenantRefSHA256 != expectedTenantRefSHA256 ||
		download.ArtifactMediaType != artifact.MediaType ||
		download.ArtifactSizeBytes != artifact.SizeBytes ||
		download.ArtifactSHA256 != artifact.SHA256 ||
		usage.EventID == "" || usage.OperationID != operation.OperationID ||
		usage.JobID != operation.JobID || usage.ModelVersionRef != operation.ModelVersionRef ||
		usage.ChannelType != "gpu_agent" || usage.Kind != "audio_duration" ||
		usage.Quantity <= 0 || usage.Unit != "millisecond" ||
		usage.EvidenceRef != "artifact:"+artifact.ID ||
		!validWildFlowJourneySafeRef(usage.EventID) ||
		!validWildFlowJourneyDigest(usage.PayloadDigest) ||
		usage.IngestedAt.IsZero() || usage.StartedAt.IsZero() || usage.EndedAt.Before(usage.StartedAt) ||
		time.Unix(operation.CreatedTime, 0).After(usage.StartedAt) ||
		usage.EndedAt.After(usageIngestedAt) || usageIngestedAt.After(downloadedAt) ||
		downloadedAt.After(receiptCreatedAt) {
		return nil, model.ErrWildFlowJourneyEvidenceConflict
	}

	receipt := &WildFlowPublicJourneyReceipt{
		SchemaVersion: 1, State: "public_journey_succeeded",
		ReceiptCreatedAt:     receiptCreatedAt.Format(time.RFC3339Nano),
		TenantRefSHA256:      expectedTenantRefSHA256,
		OperationID:          operation.OperationID,
		OperationCreatedAt:   time.Unix(operation.CreatedTime, 0).UTC().Format(time.RFC3339Nano),
		OperationState:       operation.State,
		RequestID:            operation.RequestID,
		IdempotencyKeyDigest: operation.IdempotencyKeyDigest,
		RequestDigest:        operation.RequestDigest,
		PublicModelRef:       operation.ProductModelRef,
		RuntimeOfferingRef:   runtimeOfferingRef,
		ModelVersionRef:      operation.ModelVersionRef,
		JobID:                operation.JobID,
		BillingMode:          billingMode,
		BillingState:         operation.BillingState,
		UsageEventID:         usage.EventID,
		UsagePayloadDigest:   usage.PayloadDigest,
		UsageIngestedAt:      usageIngestedAt.Format(time.RFC3339Nano),
		ArtifactID:           artifact.ID,
		ArtifactMediaType:    artifact.MediaType,
		ArtifactSizeBytes:    artifact.SizeBytes,
		ArtifactSHA256:       artifact.SHA256,
		ArtifactDownloadedAt: downloadedAt.Format(time.RFC3339Nano),
	}
	return receipt, nil
}

func validWildFlowASRJourneyIdentity(publicModelRef string, modelVersionRef string) bool {
	return modelVersionRef == wildFlowModelVersionDualASR &&
		(IsWildFlowASRModel(publicModelRef) || publicModelRef == wildFlowModelVersionDualASR)
}

func validWildFlowJourneyReceiptBilling(mode string, state string) bool {
	return (mode == "retail_audio_duration" && state == model.WildFlowBillingStateSettled) ||
		(mode == "team_trial_no_charge" && state == model.WildFlowBillingStatePending)
}

func wildFlowJourneyOperationBillingMode(
	operation *model.WildFlowOperation,
	usage *model.WildFlowUsageEvent,
) (string, bool) {
	if operation == nil || usage == nil || !validWildFlowASRJourneyIdentity(
		operation.ProductModelRef, operation.ModelVersionRef,
	) || operation.BillingUsageEventID != usage.EventID {
		return "", false
	}
	if operation.ProductModelRef == wildFlowModelVersionDualASR &&
		operation.BillingState == model.WildFlowBillingStatePending &&
		operation.BillingSource == model.WildFlowBillingSourceTeamTrial &&
		operation.BillingSubscriptionID == 0 && operation.BillingQuota == 0 &&
		operation.BillingTokenQuota == 0 && operation.BillingCurrency == "" &&
		operation.BillingAmountMicros == 0 && operation.BillingUnit == "" &&
		operation.BillingBillableUnits == 0 && operation.BillingQuotaPerUnit == "" &&
		operation.BillingUSDExchangeRate == "" && operation.BillingPriceVersion == "" &&
		operation.BillingSettledTime == 0 {
		return "team_trial_no_charge", true
	}
	if IsWildFlowASRModel(operation.ProductModelRef) &&
		operation.BillingState == model.WildFlowBillingStateSettled &&
		((operation.BillingSource == model.WildFlowBillingSourceWallet && operation.BillingSubscriptionID == 0) ||
			(operation.BillingSource == model.WildFlowBillingSourceSubscription && operation.BillingSubscriptionID > 0)) &&
		operation.BillingQuota > 0 && operation.BillingCurrency == "CNY" &&
		operation.BillingAmountMicros > 0 && operation.BillingUnit == "audio_millisecond" &&
		operation.BillingBillableUnits == usage.Quantity && usage.Quantity > 0 &&
		operation.BillingQuotaPerUnit != "" && operation.BillingUSDExchangeRate != "" &&
		supportedWildFlowRetailPriceVersion(operation.BillingPriceVersion) && operation.BillingSettledTime > 0 {
		return "retail_audio_duration", true
	}
	return "", false
}

func canonicalWildFlowPublicJourneyReceipt(receipt WildFlowPublicJourneyReceipt) ([]byte, error) {
	value := map[string]any{
		"artifact_downloaded_at": receipt.ArtifactDownloadedAt,
		"artifact_id":            receipt.ArtifactID,
		"artifact_media_type":    receipt.ArtifactMediaType,
		"artifact_sha256":        receipt.ArtifactSHA256,
		"artifact_size_bytes":    receipt.ArtifactSizeBytes,
		"billing_mode":           receipt.BillingMode,
		"billing_state":          receipt.BillingState,
		"idempotency_key_digest": receipt.IdempotencyKeyDigest,
		"job_id":                 receipt.JobID,
		"model_version_ref":      receipt.ModelVersionRef,
		"operation_created_at":   receipt.OperationCreatedAt,
		"operation_id":           receipt.OperationID,
		"operation_state":        receipt.OperationState,
		"public_model_ref":       receipt.PublicModelRef,
		"receipt_created_at":     receipt.ReceiptCreatedAt,
		"request_digest":         receipt.RequestDigest,
		"request_id":             receipt.RequestID,
		"runtime_offering_ref":   receipt.RuntimeOfferingRef,
		"schema_version":         receipt.SchemaVersion,
		"state":                  receipt.State,
		"tenant_ref_sha256":      receipt.TenantRefSHA256,
		"usage_event_id":         receipt.UsageEventID,
		"usage_ingested_at":      receipt.UsageIngestedAt,
		"usage_payload_digest":   receipt.UsagePayloadDigest,
	}
	encoded, err := common.Marshal(value)
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func validWildFlowJourneySafeRef(value string) bool {
	return wildFlowJourneySafeRefPattern.MatchString(value)
}

func validWildFlowJourneyDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for index := range value {
		if (value[index] < '0' || value[index] > '9') && (value[index] < 'a' || value[index] > 'f') {
			return false
		}
	}
	return true
}

func decimalWildFlowUserID(userID int) string {
	if userID <= 0 {
		return ""
	}
	var digits [20]byte
	position := len(digits)
	for userID > 0 {
		position--
		digits[position] = byte('0' + userID%10)
		userID /= 10
	}
	return string(digits[position:])
}
