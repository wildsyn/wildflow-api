package service

import (
	"fmt"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/shopspring/decimal"
)

const wildFlowRetailPriceVersion = "wildflow-retail-cny-v1"

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
