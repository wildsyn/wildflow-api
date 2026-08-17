package service

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
