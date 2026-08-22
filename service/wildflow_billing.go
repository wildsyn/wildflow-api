package service

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/internal/inferenceclient"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/shopspring/decimal"
)

const wildFlowRetailPriceVersion = "wildflow-retail-cny-v1"

var (
	ErrWildFlowBillingInsufficientQuota = errors.New("insufficient quota for WildFlow job")
	ErrWildFlowMissingArtifact          = errors.New("succeeded WildFlow job has no durable artifact")
	ErrWildFlowInvalidArtifact          = errors.New("succeeded WildFlow job has an incomplete or invalid artifact")
)

func QuoteWildFlowBilling(request WildFlowJobRequest) (model.WildFlowBillingQuote, error) {
	request, err := NormalizeWildFlowJobRequest(request)
	if err != nil {
		return model.WildFlowBillingQuote{}, err
	}
	if request.Model == WildFlowModelExamDualASR {
		return model.WildFlowBillingQuote{}, ErrWildFlowUnsupportedModel
	}
	offering, ok := FindWildFlowOffering(request.Model)
	if !ok {
		return model.WildFlowBillingQuote{}, ErrWildFlowUnsupportedModel
	}
	if err := validateWildFlowParameters(offering.Kind, request.Parameters); err != nil {
		return model.WildFlowBillingQuote{}, err
	}
	if common.QuotaPerUnit <= 0 || operation_setting.USDExchangeRate <= 0 {
		return model.WildFlowBillingQuote{}, fmt.Errorf("invalid quota conversion configuration")
	}

	var amountCNY decimal.Decimal
	var unit string
	var billableUnits int64
	switch request.Model {
	case WildFlowModelVoxCPM2:
		content, ok := request.Parameters["input"].(string)
		if !ok {
			content, ok = request.Parameters["text"].(string)
		}
		if !ok {
			return model.WildFlowBillingQuote{}, ErrWildFlowInvalidParameters
		}
		billableUnits = int64(utf8.RuneCountInString(content))
		unit = offering.Pricing.Unit
		amountCNY = decimal.NewFromFloat(offering.Pricing.Amount).
			Mul(decimal.NewFromInt(billableUnits)).
			Div(decimal.NewFromInt(10_000))
	case WildFlowModelFlux2:
		billableUnits = 1
		unit = offering.Pricing.Unit
		amountCNY = decimal.NewFromFloat(offering.Pricing.Amount)
	default:
		return model.WildFlowBillingQuote{}, ErrWildFlowUnsupportedModel
	}

	quotaDecimal := amountCNY.
		Div(decimal.NewFromFloat(operation_setting.USDExchangeRate)).
		Mul(decimal.NewFromFloat(common.QuotaPerUnit))
	quota, clamp := common.QuotaFromDecimalChecked(quotaDecimal)
	if clamp != nil {
		return model.WildFlowBillingQuote{}, clamp
	}
	quote := model.WildFlowBillingQuote{
		Currency:        "CNY",
		AmountMicros:    amountCNY.Mul(decimal.NewFromInt(1_000_000)).Round(0).IntPart(),
		Unit:            unit,
		BillableUnits:   billableUnits,
		Quota:           quota,
		QuotaPerUnit:    decimal.NewFromFloat(common.QuotaPerUnit).String(),
		USDExchangeRate: decimal.NewFromFloat(operation_setting.USDExchangeRate).String(),
		PriceVersion:    wildFlowRetailPriceVersion,
	}
	if err := quote.Validate(); err != nil {
		return model.WildFlowBillingQuote{}, err
	}
	return quote, nil
}

func ReserveWildFlowOperationBilling(operation *model.WildFlowOperation, request WildFlowJobRequest) (*model.WildFlowOperation, error) {
	if operation == nil {
		return nil, fmt.Errorf("nil WildFlow operation")
	}
	request, err := NormalizeWildFlowJobRequest(request)
	if err != nil {
		return nil, err
	}
	if request.Model == WildFlowModelExamDualASR {
		if err := validateWildFlowRequest("asr", request); err != nil {
			return nil, err
		}
		if operation.ProductModelRef != WildFlowModelExamDualASR ||
			(operation.BillingState != "" && operation.BillingState != model.WildFlowBillingStatePending) {
			return nil, model.ErrWildFlowBillingStateConflict
		}
		return operation, nil
	}
	if operation.BillingState == model.WildFlowBillingStateReserved || operation.BillingState == model.WildFlowBillingStateSettled {
		return operation, nil
	}
	quote, err := QuoteWildFlowBilling(request)
	if err != nil {
		return nil, err
	}

	userSetting, err := model.GetUserSetting(operation.UserID, true)
	if err != nil {
		return nil, err
	}
	preference := common.NormalizeBillingPreference(userSetting.BillingPreference)

	tryWallet := func() (*model.WildFlowOperation, error) {
		reserved, reserveErr := model.ReserveWildFlowWalletBilling(operation.OperationID, quote)
		if errors.Is(reserveErr, model.ErrWildFlowInsufficientUserQuota) || errors.Is(reserveErr, model.ErrWildFlowInsufficientTokenQuota) {
			return nil, fmt.Errorf("%w: %v", ErrWildFlowBillingInsufficientQuota, reserveErr)
		}
		return reserved, reserveErr
	}
	trySubscription := func() (*model.WildFlowOperation, error) {
		reserved, reserveErr := model.ReserveWildFlowSubscriptionBilling(operation.OperationID, quote)
		if reserveErr != nil {
			if errors.Is(reserveErr, model.ErrWildFlowInsufficientTokenQuota) ||
				strings.Contains(reserveErr.Error(), "no active subscription") ||
				strings.Contains(reserveErr.Error(), "subscription quota insufficient") {
				return nil, fmt.Errorf("%w: %v", ErrWildFlowBillingInsufficientQuota, reserveErr)
			}
			return nil, reserveErr
		}
		return reserved, nil
	}

	switch preference {
	case "subscription_only":
		return trySubscription()
	case "wallet_only":
		return tryWallet()
	case "wallet_first":
		reserved, reserveErr := tryWallet()
		if errors.Is(reserveErr, ErrWildFlowBillingInsufficientQuota) {
			return trySubscription()
		}
		return reserved, reserveErr
	case "subscription_first":
		fallthrough
	default:
		hasSubscription, subscriptionErr := model.HasActiveUserSubscription(operation.UserID)
		if subscriptionErr != nil {
			return nil, subscriptionErr
		}
		if !hasSubscription {
			return tryWallet()
		}
		reserved, reserveErr := trySubscription()
		if !errors.Is(reserveErr, ErrWildFlowBillingInsufficientQuota) {
			return reserved, reserveErr
		}
		allowWalletOverflow, overflowErr := model.UserActiveSubscriptionsAllowWalletOverflow(operation.UserID)
		if overflowErr != nil {
			return nil, overflowErr
		}
		if allowWalletOverflow {
			return tryWallet()
		}
		return nil, reserveErr
	}
}

func ValidateWildFlowCompletedArtifacts(operation *model.WildFlowOperation, artifacts []inferenceclient.Artifact) error {
	if len(artifacts) == 0 {
		return ErrWildFlowMissingArtifact
	}
	if operation == nil {
		return nil
	}
	if operation.ProductModelRef == WildFlowModelExamDualASR {
		return validateWildFlowExamDualASRArtifact(artifacts)
	}
	if operation.ProductModelRef != WildFlowModelVoxCPM2 {
		return nil
	}
	if len(artifacts) != 1 || operation.BillingBillableUnits <= 0 {
		return ErrWildFlowInvalidArtifact
	}

	artifact := artifacts[0]
	if artifact.MediaType != "audio/mpeg" || artifact.SizeBytes <= 0 || !validWildFlowSHA256(artifact.SHA256) {
		return ErrWildFlowInvalidArtifact
	}
	if codec, ok := artifact.Metadata["codec"].(string); !ok || codec != "mp3" {
		return ErrWildFlowInvalidArtifact
	}

	requiredIntegers := map[string]int64{
		"bitrate":     96_000,
		"sample_rate": 48_000,
		"channels":    1,
	}
	for key, expected := range requiredIntegers {
		value, ok := wildFlowArtifactInteger(artifact.Metadata[key])
		if !ok || value != expected {
			return ErrWildFlowInvalidArtifact
		}
	}
	duration, durationOK := wildFlowArtifactInteger(artifact.Metadata["duration_ms"])
	inputCharacters, inputOK := wildFlowArtifactInteger(artifact.Metadata["input_characters"])
	completedCharacters, completedOK := wildFlowArtifactInteger(artifact.Metadata["completed_characters"])
	segmentCount, segmentOK := wildFlowArtifactInteger(artifact.Metadata["segment_count"])
	completedSegmentCount, completedSegmentOK := wildFlowArtifactInteger(artifact.Metadata["completed_segment_count"])
	metadataSize, sizeOK := wildFlowArtifactInteger(artifact.Metadata["size_bytes"])
	metadataSHA256, shaOK := artifact.Metadata["sha256"].(string)
	if !durationOK || duration <= 0 ||
		!inputOK || inputCharacters != operation.BillingBillableUnits ||
		!completedOK || completedCharacters != operation.BillingBillableUnits ||
		!segmentOK || segmentCount <= 0 ||
		!completedSegmentOK || completedSegmentCount != segmentCount ||
		!sizeOK || metadataSize != artifact.SizeBytes ||
		!shaOK || !validWildFlowSHA256(metadataSHA256) || !strings.EqualFold(metadataSHA256, artifact.SHA256) {
		return ErrWildFlowInvalidArtifact
	}
	return nil
}

func validateWildFlowExamDualASRArtifact(artifacts []inferenceclient.Artifact) error {
	if len(artifacts) != 1 {
		return ErrWildFlowInvalidArtifact
	}
	artifact := artifacts[0]
	if artifact.MediaType != "application/json" || artifact.SizeBytes <= 0 || !validWildFlowSHA256(artifact.SHA256) {
		return ErrWildFlowInvalidArtifact
	}
	schemaVersion, schemaOK := wildFlowArtifactInteger(artifact.Metadata["schema_version"])
	duration, durationOK := finiteNumber(artifact.Metadata["duration_seconds"])
	modelVersion, versionOK := artifact.Metadata["model_version_ref"].(string)
	modelRevision, revisionOK := artifact.Metadata["model_revision"].(string)
	vibeVoiceRevision, vibeVoiceOK := artifact.Metadata["vibevoice_model_revision"].(string)
	whisperRevision, whisperOK := artifact.Metadata["faster_whisper_model_revision"].(string)
	runtimeVersion, runtimeOK := artifact.Metadata["runtime_version_ref"].(string)
	sourceArtifactID, sourceOK := artifact.Metadata["source_artifact_id"].(string)
	const vibeVoice = "d0c9efdb8d614685062c04425d91e01b6f37d944"
	const whisper = "edaa852ec7e145841d8ffdb056a99866b5f0a478"
	const runtime = "exam-dual-asr-runtime-v1-a09e48e-94da20d"
	if !schemaOK || schemaVersion != 1 || !durationOK || duration <= 0 || duration > 7_200 ||
		!versionOK || modelVersion != WildFlowModelExamDualASR ||
		!revisionOK || modelRevision != vibeVoice+"_"+whisper ||
		!vibeVoiceOK || vibeVoiceRevision != vibeVoice || !whisperOK || whisperRevision != whisper ||
		!runtimeOK || runtimeVersion != runtime ||
		!sourceOK || !validWildFlowResourceID(sourceArtifactID) {
		return ErrWildFlowInvalidArtifact
	}
	return nil
}

func wildFlowArtifactInteger(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int8:
		return int64(typed), true
	case int16:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case uint:
		if uint64(typed) > math.MaxInt64 {
			return 0, false
		}
		return int64(typed), true
	case uint8:
		return int64(typed), true
	case uint16:
		return int64(typed), true
	case uint32:
		return int64(typed), true
	case uint64:
		if typed > math.MaxInt64 {
			return 0, false
		}
		return int64(typed), true
	case float32:
		value := float64(typed)
		if math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value || value < math.MinInt64 || value > math.MaxInt64 {
			return 0, false
		}
		return int64(value), true
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || math.Trunc(typed) != typed || typed < math.MinInt64 || typed > math.MaxInt64 {
			return 0, false
		}
		return int64(typed), true
	default:
		return 0, false
	}
}

func validWildFlowSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func FinalizeWildFlowOperationBilling(ctx context.Context, operation *model.WildFlowOperation, artifacts []inferenceclient.Artifact) error {
	if operation == nil {
		return nil
	}
	if operation.State == "succeeded" {
		if err := ValidateWildFlowCompletedArtifacts(operation, artifacts); err != nil {
			return err
		}
	}
	if operation.BillingState == "" || operation.BillingState == model.WildFlowBillingStatePending {
		return nil
	}
	switch operation.State {
	case "succeeded":
		settled, _, err := model.SettleWildFlowOperationBilling(operation.OperationID)
		if err != nil {
			return err
		}
		if settled != nil {
			if settled.BillingState != model.WildFlowBillingStateSettled {
				return model.ErrWildFlowBillingStateConflict
			}
			if err := model.RecordWildFlowBillingLog(settled, model.LogTypeConsume, "WildFlow job settled"); err != nil {
				return err
			}
			*operation = *settled
		}
		return nil
	case "failed", "cancelled":
		refunded, _, err := model.RefundWildFlowOperationBilling(operation.OperationID)
		if err != nil {
			return err
		}
		if refunded != nil {
			if refunded.BillingState != model.WildFlowBillingStateRefunded {
				return model.ErrWildFlowBillingStateConflict
			}
			if err := model.RecordWildFlowBillingLog(refunded, model.LogTypeRefund, "WildFlow job refunded"); err != nil {
				return err
			}
			*operation = *refunded
		}
		return nil
	case "recovery_required":
		return nil
	default:
		_ = ctx
		return nil
	}
}
