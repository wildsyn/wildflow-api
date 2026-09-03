package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/internal/inferenceclient"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestQuoteWildFlowBillingUsesRetailCNYPrices(t *testing.T) {
	previousQuotaPerUnit := common.QuotaPerUnit
	previousExchangeRate := operation_setting.USDExchangeRate
	common.QuotaPerUnit = 500_000
	operation_setting.USDExchangeRate = 7.3
	t.Cleanup(func() {
		common.QuotaPerUnit = previousQuotaPerUnit
		operation_setting.USDExchangeRate = previousExchangeRate
	})

	tests := []struct {
		name         string
		request      WildFlowJobRequest
		amountMicros int64
		billingUnit  string
		billableUnit int64
		quota        int
	}{
		{
			name: "VoxCPM2 charges 0.8 CNY per ten thousand Unicode characters",
			request: WildFlowJobRequest{
				Model: WildFlowModelVoxCPM2,
				Parameters: map[string]any{
					"input": strings.Repeat("野", 10_000),
					"voice": "default",
				},
			},
			amountMicros: 800_000,
			billingUnit:  "10k_characters",
			billableUnit: 10_000,
			quota:        54_795,
		},
		{
			name: "FLUX charges 0.05 CNY for one image",
			request: WildFlowJobRequest{
				Model:      WildFlowModelFlux2,
				Parameters: map[string]any{"prompt": "一只熊猫"},
			},
			amountMicros: 50_000,
			billingUnit:  "image",
			billableUnit: 1,
			quota:        3_425,
		},
		{
			name: "dual ASR preauthorizes the two hour maximum at 0.10 CNY per audio minute",
			request: WildFlowJobRequest{
				Model: WildFlowModelExamDualASR, InputArtifactIDs: []string{"input-1"}, Parameters: map[string]any{},
			},
			amountMicros: 12_000_000,
			billingUnit:  "audio_millisecond",
			billableUnit: 7_200_000,
			quota:        821_918,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			quote, err := QuoteWildFlowBilling(test.request)
			require.NoError(t, err)
			assert.Equal(t, "CNY", quote.Currency)
			assert.Equal(t, test.amountMicros, quote.AmountMicros)
			assert.Equal(t, test.billingUnit, quote.Unit)
			assert.Equal(t, test.billableUnit, quote.BillableUnits)
			assert.Equal(t, test.quota, quote.Quota)
			assert.Equal(t, "500000", quote.QuotaPerUnit)
			assert.Equal(t, "7.3", quote.USDExchangeRate)
		})
	}
}

func TestReserveWildFlowOperationBillingUsesSubscriptionPreferenceDurably(t *testing.T) {
	previousDB := model.DB
	previousLogDB := model.LOG_DB
	previousRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	db, err := gorm.Open(sqlite.Open("file:wildflow-subscription-billing-"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.User{},
		&model.Token{},
		&model.Log{},
		&model.SubscriptionPlan{},
		&model.UserSubscription{},
		&model.SubscriptionPreConsumeRecord{},
		&model.WildFlowOperation{},
		&model.WildFlowUsageEvent{},
		&model.WildFlowBillingLogEntry{},
		&model.WildFlowBillingLogProjectionReceipt{},
	))
	model.DB = db
	model.LOG_DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		model.LOG_DB = previousLogDB
		common.RedisEnabled = previousRedisEnabled
		sqlDB, sqlErr := db.DB()
		if sqlErr == nil {
			_ = sqlDB.Close()
		}
	})

	user := &model.User{
		Id:       501,
		Username: "subscription-billing-user",
		AffCode:  "subscription-billing-aff",
		Quota:    0,
		Group:    "default",
		Setting:  `{"billing_preference":"subscription_only"}`,
	}
	require.NoError(t, db.Create(user).Error)
	token := &model.Token{Id: 601, UserId: user.Id, Key: "subscription-token", RemainQuota: 100_000}
	require.NoError(t, db.Create(token).Error)
	allowWalletOverflow := false
	plan := &model.SubscriptionPlan{
		Id:                  700_001,
		Title:               "WildFlow test plan",
		Currency:            "CNY",
		DurationUnit:        model.SubscriptionDurationMonth,
		DurationValue:       1,
		Enabled:             true,
		AllowWalletOverflow: &allowWalletOverflow,
		TotalAmount:         100_000,
		QuotaResetPeriod:    model.SubscriptionResetNever,
	}
	require.NoError(t, db.Create(plan).Error)
	model.InvalidateSubscriptionPlanCache(plan.Id)
	subscription := &model.UserSubscription{
		Id:                  701,
		UserId:              user.Id,
		PlanId:              plan.Id,
		AmountTotal:         100_000,
		StartTime:           time.Now().Add(-time.Hour).Unix(),
		EndTime:             time.Now().Add(time.Hour).Unix(),
		Status:              "active",
		AllowWalletOverflow: false,
	}
	require.NoError(t, db.Create(subscription).Error)
	operation := &model.WildFlowOperation{
		OperationID:          "op-subscription-billing",
		UserID:               user.Id,
		TokenID:              token.Id,
		IdempotencyKeyDigest: "key-subscription-billing",
		RequestDigest:        "request-subscription-billing",
		RequestID:            "request-id-subscription-billing",
		ProductModelRef:      WildFlowModelFlux2,
		ModelVersionRef:      "black-forest-labs/FLUX.2-klein-4B",
		State:                "submitting",
	}
	require.NoError(t, db.Create(operation).Error)
	request := WildFlowJobRequest{Model: WildFlowModelFlux2, Parameters: map[string]any{"prompt": "一只熊猫"}}

	first, err := ReserveWildFlowOperationBilling(operation, request)
	require.NoError(t, err)
	second, err := ReserveWildFlowOperationBilling(first, request)
	require.NoError(t, err)
	assert.Equal(t, model.WildFlowBillingSourceSubscription, second.BillingSource)
	require.NoError(t, db.First(subscription, subscription.Id).Error)
	require.NoError(t, db.First(token, token.Id).Error)
	assert.Equal(t, int64(3_425), subscription.AmountUsed)
	assert.Equal(t, 100_000-3_425, token.RemainQuota)

	require.NoError(t, model.UpdateWildFlowOperationExecution(operation.OperationID, "job-subscription", "failed", "execution_failed"))
	second.State = "failed"
	second.JobID = "job-subscription"
	require.NoError(t, FinalizeWildFlowOperationBilling(context.Background(), second, nil))
	require.NoError(t, db.First(subscription, subscription.Id).Error)
	require.NoError(t, db.First(token, token.Id).Error)
	assert.Zero(t, subscription.AmountUsed)
	assert.Equal(t, 100_000, token.RemainQuota)
	assert.Equal(t, model.WildFlowBillingStateRefunded, second.BillingState)

	recoveryOperation := &model.WildFlowOperation{
		OperationID:          "op-subscription-refund-recovery",
		UserID:               user.Id,
		TokenID:              token.Id,
		IdempotencyKeyDigest: "key-subscription-refund-recovery",
		RequestDigest:        "request-subscription-refund-recovery",
		RequestID:            "request-id-subscription-refund-recovery",
		ProductModelRef:      WildFlowModelFlux2,
		ModelVersionRef:      "black-forest-labs/FLUX.2-klein-4B",
		State:                "submitting",
	}
	require.NoError(t, db.Create(recoveryOperation).Error)
	recoveryOperation, err = ReserveWildFlowOperationBilling(recoveryOperation, request)
	require.NoError(t, err)
	require.NoError(t, model.UpdateWildFlowOperationExecution(recoveryOperation.OperationID, "job-subscription-recovery", "failed", "execution_failed"))
	// Simulate a process stopping after it durably claimed the subscription
	// refund but before the idempotent refund completed.
	require.NoError(t, db.Model(&model.WildFlowOperation{}).
		Where("operation_id = ?", recoveryOperation.OperationID).
		Update("billing_state", model.WildFlowBillingStateRefunding).Error)
	recoveryOperation.State = "failed"
	recoveryOperation.JobID = "job-subscription-recovery"
	recoveryOperation.BillingState = model.WildFlowBillingStateRefunding
	require.NoError(t, FinalizeWildFlowOperationBilling(context.Background(), recoveryOperation, nil))
	require.NoError(t, db.First(subscription, subscription.Id).Error)
	assert.Zero(t, subscription.AmountUsed)
	assert.Equal(t, model.WildFlowBillingStateRefunded, recoveryOperation.BillingState)
}

func TestQuoteWildFlowBillingRejectsInvalidRuntimeConversion(t *testing.T) {
	previousExchangeRate := operation_setting.USDExchangeRate
	operation_setting.USDExchangeRate = 0
	t.Cleanup(func() { operation_setting.USDExchangeRate = previousExchangeRate })

	_, err := QuoteWildFlowBilling(WildFlowJobRequest{
		Model:      WildFlowModelFlux2,
		Parameters: map[string]any{"prompt": "一只熊猫"},
	})
	require.Error(t, err)
}

func TestQuoteWildFlowBillingRejectsUnsupportedModel(t *testing.T) {
	_, err := QuoteWildFlowBilling(WildFlowJobRequest{
		Model:      "unsupported-model",
		Parameters: map[string]any{"prompt": "test"},
	})
	require.ErrorIs(t, err, ErrWildFlowUnsupportedModel)
}

func TestDualASRUsesRetailBilling(t *testing.T) {
	request := WildFlowJobRequest{
		Model: WildFlowModelExamDualASR, InputArtifactIDs: []string{"input-1"}, Parameters: map[string]any{},
	}
	quote, err := QuoteWildFlowBilling(request)
	require.NoError(t, err)
	assert.Equal(t, "audio_millisecond", quote.Unit)
	assert.Equal(t, int64(7_200_000), quote.BillableUnits)
}

func TestWildFlowBillingServiceIgnoresUnbilledAndNonTerminalOperations(t *testing.T) {
	_, err := ReserveWildFlowOperationBilling(nil, WildFlowJobRequest{})
	require.Error(t, err)
	require.NoError(t, FinalizeWildFlowOperationBilling(context.Background(), nil, nil))
	require.NoError(t, FinalizeWildFlowOperationBilling(context.Background(), &model.WildFlowOperation{
		BillingState: model.WildFlowBillingStatePending,
		State:        "queued",
	}, nil))
	require.NoError(t, FinalizeWildFlowOperationBilling(context.Background(), &model.WildFlowOperation{
		BillingState: model.WildFlowBillingStateReserved,
		State:        "running",
	}, nil))
}

func TestValidateWildFlowCompletedArtifactsRequiresCanonicalVoxMP3Evidence(t *testing.T) {
	const digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	operation := &model.WildFlowOperation{
		ProductModelRef:      WildFlowModelVoxCPM2,
		BillingBillableUnits: 5,
	}
	validArtifact := inferenceclient.Artifact{
		ID:        "artifact-vox",
		JobID:     "job-vox",
		MediaType: "audio/mpeg",
		SizeBytes: 12,
		SHA256:    digest,
		Metadata: map[string]any{
			"codec":                   "mp3",
			"bitrate":                 float64(96_000),
			"sample_rate":             float64(48_000),
			"channels":                float64(1),
			"duration_ms":             float64(1_200),
			"input_characters":        float64(5),
			"completed_characters":    float64(5),
			"segment_count":           float64(1),
			"completed_segment_count": float64(1),
			"size_bytes":              float64(12),
			"sha256":                  digest,
		},
	}
	require.NoError(t, ValidateWildFlowCompletedArtifacts(operation, []inferenceclient.Artifact{validArtifact}))

	tests := []struct {
		name   string
		mutate func(*inferenceclient.Artifact)
	}{
		{name: "codec", mutate: func(artifact *inferenceclient.Artifact) { artifact.Metadata["codec"] = "wav" }},
		{name: "bitrate", mutate: func(artifact *inferenceclient.Artifact) { artifact.Metadata["bitrate"] = float64(128_000) }},
		{name: "sample rate", mutate: func(artifact *inferenceclient.Artifact) { artifact.Metadata["sample_rate"] = float64(44_100) }},
		{name: "channels", mutate: func(artifact *inferenceclient.Artifact) { artifact.Metadata["channels"] = float64(2) }},
		{name: "duration", mutate: func(artifact *inferenceclient.Artifact) { artifact.Metadata["duration_ms"] = float64(0) }},
		{name: "input characters", mutate: func(artifact *inferenceclient.Artifact) { artifact.Metadata["input_characters"] = float64(4) }},
		{name: "fractional metadata", mutate: func(artifact *inferenceclient.Artifact) { artifact.Metadata["segment_count"] = 1.5 }},
		{name: "artifact count", mutate: func(artifact *inferenceclient.Artifact) { artifact.Metadata["completed_segment_count"] = float64(0) }},
		{name: "digest format", mutate: func(artifact *inferenceclient.Artifact) { artifact.Metadata["sha256"] = strings.Repeat("z", 64) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			artifact := validArtifact
			artifact.Metadata = make(map[string]any, len(validArtifact.Metadata))
			for key, value := range validArtifact.Metadata {
				artifact.Metadata[key] = value
			}
			test.mutate(&artifact)
			require.ErrorIs(t, ValidateWildFlowCompletedArtifacts(operation, []inferenceclient.Artifact{artifact}), ErrWildFlowInvalidArtifact)
		})
	}
}

func TestValidateWildFlowCompletedArtifactsAcceptsVersionedExamDualASRJSON(t *testing.T) {
	artifact := inferenceclient.Artifact{
		ID: "artifact-asr", JobID: "job-asr", MediaType: "application/json", SizeBytes: 128,
		SHA256: strings.Repeat("a", 64),
		Metadata: map[string]any{
			"schema_version":                float64(1),
			"model_version_ref":             wildFlowModelVersionDualASR,
			"model_revision":                "d0c9efdb8d614685062c04425d91e01b6f37d944_edaa852ec7e145841d8ffdb056a99866b5f0a478",
			"vibevoice_model_revision":      "d0c9efdb8d614685062c04425d91e01b6f37d944",
			"faster_whisper_model_revision": "edaa852ec7e145841d8ffdb056a99866b5f0a478",
			"duration_seconds":              float64(120),
			"source_artifact_id":            "input-1",
			"runtime_version_ref":           "exam-dual-asr-http-runtime-v1-ed59136",
		},
	}
	operation := &model.WildFlowOperation{ProductModelRef: WildFlowModelExamDualASR}
	artifact.Metadata["model_revision"] = "vibevoice-d0c9efdb-plus-faster-whisper-edaa852e"
	require.NoError(t, ValidateWildFlowCompletedArtifacts(operation, []inferenceclient.Artifact{artifact}))
	artifact.Metadata["model_revision"] = "d0c9efdb8d614685062c04425d91e01b6f37d944_edaa852ec7e145841d8ffdb056a99866b5f0a478"
	require.NoError(t, ValidateWildFlowCompletedArtifacts(operation, []inferenceclient.Artifact{artifact}))
	artifact.Metadata["runtime_version_ref"] = "exam-dual-asr-runtime-v1-a09e48e-94da20d"
	require.NoError(t, ValidateWildFlowCompletedArtifacts(operation, []inferenceclient.Artifact{artifact}))
	artifact.Metadata["runtime_version_ref"] = "exam-dual-asr-http-runtime-v1-ed59136"
	require.NoError(t, FinalizeWildFlowOperationBilling(context.Background(), &model.WildFlowOperation{
		ProductModelRef: WildFlowModelExamDualASR, State: "succeeded", BillingState: model.WildFlowBillingStatePending,
	}, []inferenceclient.Artifact{artifact}))

	artifact.Metadata["faster_whisper_model_revision"] = "mutable-latest"
	require.ErrorIs(t, ValidateWildFlowCompletedArtifacts(operation, []inferenceclient.Artifact{artifact}), ErrWildFlowInvalidArtifact)
	artifact.Metadata["faster_whisper_model_revision"] = "edaa852ec7e145841d8ffdb056a99866b5f0a478"
	artifact.Metadata["runtime_version_ref"] = "mutable-runtime"
	require.ErrorIs(t, ValidateWildFlowCompletedArtifacts(operation, []inferenceclient.Artifact{artifact}), ErrWildFlowInvalidArtifact)
}

func TestValidateWildFlowCompletedArtifactsPreservesFluxArtifactContract(t *testing.T) {
	operation := &model.WildFlowOperation{ProductModelRef: WildFlowModelFlux2}
	artifact := inferenceclient.Artifact{
		ID:        "artifact-image",
		JobID:     "job-image",
		MediaType: "image/png",
		SizeBytes: 12,
		SHA256:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}

	require.NoError(t, ValidateWildFlowCompletedArtifacts(operation, []inferenceclient.Artifact{artifact}))
	require.ErrorIs(t, ValidateWildFlowCompletedArtifacts(operation, nil), ErrWildFlowMissingArtifact)
}

func TestValidateWildFlowCompletedArtifactsRequiresVersionedIndexTTSWAV(t *testing.T) {
	operation := &model.WildFlowOperation{ProductModelRef: WildFlowModelIndexTTS25}
	artifact := inferenceclient.Artifact{
		ID:        "artifact-indextts",
		JobID:     "job-indextts",
		MediaType: "audio/wav",
		SizeBytes: 209452,
		SHA256:    strings.Repeat("a", 64),
		Metadata: map[string]any{
			"codec":                "pcm_s16le",
			"sample_rate":          float64(24000),
			"channels":             float64(1),
			"duration_ms":          float64(4362),
			"size_bytes":           float64(209452),
			"sha256":               strings.Repeat("a", 64),
			"lang":                 "zh",
			"reference_audio_mode": "server_fixed",
		},
	}

	require.NoError(t, ValidateWildFlowCompletedArtifacts(operation, []inferenceclient.Artifact{artifact}))
	require.ErrorIs(t, ValidateWildFlowCompletedArtifacts(operation, []inferenceclient.Artifact{artifact, artifact}), ErrWildFlowInvalidArtifact)

	artifact.Metadata["reference_audio_mode"] = "client_supplied"
	require.ErrorIs(t, ValidateWildFlowCompletedArtifacts(operation, []inferenceclient.Artifact{artifact}), ErrWildFlowInvalidArtifact)
	artifact.Metadata["reference_audio_mode"] = "server_fixed"
	artifact.Metadata["sha256"] = strings.Repeat("b", 64)
	require.ErrorIs(t, ValidateWildFlowCompletedArtifacts(operation, []inferenceclient.Artifact{artifact}), ErrWildFlowInvalidArtifact)
}
