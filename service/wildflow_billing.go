package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/shopspring/decimal"
)

const wildFlowRetailPriceVersion = "wildflow-retail-cny-v1"

var ErrWildFlowBillingInsufficientQuota = errors.New("insufficient quota for WildFlow job")

func QuoteWildFlowBilling(request WildFlowJobRequest) (model.WildFlowBillingQuote, error) {
	request, err := NormalizeWildFlowJobRequest(request)
	if err != nil {
		return model.WildFlowBillingQuote{}, err
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
		unit = "10k_characters"
		amountCNY = decimal.RequireFromString("0.8").
			Mul(decimal.NewFromInt(billableUnits)).
			Div(decimal.NewFromInt(10_000))
	case WildFlowModelFlux2:
		billableUnits = 1
		unit = "image"
		amountCNY = decimal.RequireFromString("0.05")
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
	quote, err := QuoteWildFlowBilling(request)
	if err != nil {
		return nil, err
	}
	if operation.BillingState == model.WildFlowBillingStateReserved || operation.BillingState == model.WildFlowBillingStateSettled {
		return model.ReserveWildFlowWalletBilling(operation.OperationID, quote)
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
		result, preConsumeErr := model.PreConsumeUserSubscription(
			operation.OperationID,
			operation.UserID,
			operation.ProductModelRef,
			0,
			int64(quote.Quota),
		)
		if preConsumeErr != nil {
			if strings.Contains(preConsumeErr.Error(), "no active subscription") || strings.Contains(preConsumeErr.Error(), "subscription quota insufficient") {
				return nil, fmt.Errorf("%w: %v", ErrWildFlowBillingInsufficientQuota, preConsumeErr)
			}
			return nil, preConsumeErr
		}
		reserved, attachErr := model.AttachWildFlowSubscriptionBilling(operation.OperationID, quote, result.UserSubscriptionId)
		if errors.Is(attachErr, model.ErrWildFlowInsufficientTokenQuota) {
			if refundErr := model.RefundSubscriptionPreConsume(operation.OperationID); refundErr != nil {
				return nil, fmt.Errorf("refund subscription after token quota failure: %w", refundErr)
			}
			return nil, fmt.Errorf("%w: %v", ErrWildFlowBillingInsufficientQuota, attachErr)
		}
		return reserved, attachErr
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

func FinalizeWildFlowOperationBilling(ctx context.Context, operation *model.WildFlowOperation, artifactCount int) error {
	if operation == nil || operation.BillingState == "" || operation.BillingState == model.WildFlowBillingStatePending {
		return nil
	}
	switch operation.State {
	case "succeeded":
		if artifactCount == 0 {
			return fmt.Errorf("succeeded WildFlow job has no durable artifact")
		}
		settled, _, err := model.SettleWildFlowOperationBilling(operation.OperationID)
		if err != nil {
			return err
		}
		if settled != nil {
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
