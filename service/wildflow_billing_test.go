package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
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
	require.NoError(t, FinalizeWildFlowOperationBilling(context.Background(), second, 0))
	require.NoError(t, db.First(subscription, subscription.Id).Error)
	require.NoError(t, db.First(token, token.Id).Error)
	assert.Zero(t, subscription.AmountUsed)
	assert.Equal(t, 100_000, token.RemainQuota)
	assert.Equal(t, model.WildFlowBillingStateRefunded, second.BillingState)
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

func TestWildFlowBillingServiceIgnoresUnbilledAndNonTerminalOperations(t *testing.T) {
	_, err := ReserveWildFlowOperationBilling(nil, WildFlowJobRequest{})
	require.Error(t, err)
	require.NoError(t, FinalizeWildFlowOperationBilling(context.Background(), nil, 0))
	require.NoError(t, FinalizeWildFlowOperationBilling(context.Background(), &model.WildFlowOperation{
		BillingState: model.WildFlowBillingStatePending,
		State:        "queued",
	}, 0))
	require.NoError(t, FinalizeWildFlowOperationBilling(context.Background(), &model.WildFlowOperation{
		BillingState: model.WildFlowBillingStateReserved,
		State:        "running",
	}, 0))
}
