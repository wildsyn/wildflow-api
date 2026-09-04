package service

import (
	"testing"

	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestASRProductsPriceAndTrustedEngineSelection(t *testing.T) {
	previous := operation_setting.USDExchangeRate
	operation_setting.USDExchangeRate = 7.2
	t.Cleanup(func() { operation_setting.USDExchangeRate = previous })
	for _, tc := range []struct {
		id, engine string
		price      float64
		reserve    int64
	}{
		{"wildflow/whisper-asr-v1", "whisper", 0.02, 2_400_000},
		{"wildflow/vibevoice-asr-v1", "vibevoice", 0.04, 4_800_000},
		{"wildflow/dual-asr-v1", "dual", 0.05, 6_000_000},
	} {
		t.Run(tc.id, func(t *testing.T) {
			request, err := NormalizeWildFlowJobRequest(WildFlowJobRequest{Model: tc.id, InputArtifactIDs: []string{"input-1"}})
			require.NoError(t, err)
			offering, ok := FindWildFlowOffering(tc.id)
			require.True(t, ok)
			assert.Equal(t, tc.price, offering.Pricing.Amount)
			assert.Equal(t, "audio_minute", offering.Pricing.Unit)
			quote, err := QuoteWildFlowBilling(request)
			require.NoError(t, err)
			assert.Equal(t, tc.reserve, quote.AmountMicros)
			assert.Equal(t, int64(7_200_000), quote.BillableUnits)
			parameters, err := PrepareWildFlowRuntimeParameters(tc.id, offering.ModelVersionRef, request.Parameters)
			require.NoError(t, err)
			if tc.engine != "dual" {
				assert.Equal(t, tc.engine, parameters["asr_engine"])
			}
			assert.Empty(t, request.Parameters)
			request.Parameters["asr_engine"] = "dual"
			_, err = QuoteWildFlowBilling(request)
			assert.ErrorIs(t, err, ErrWildFlowInvalidParameters)
		})
	}
}
