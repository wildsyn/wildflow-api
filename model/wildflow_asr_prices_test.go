package model

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestASRPricesSettleActualMillisecondsFromReservation(t *testing.T) {
	for _, tc := range []struct {
		id                string
		reserve, expected int64
	}{
		{"wildflow/whisper-asr-v1", 2_400_000, 20_001},
		{"wildflow/vibevoice-asr-v1", 4_800_000, 40_001},
		{"wildflow/dual-asr-v1", 6_000_000, 50_001},
	} {
		t.Run(tc.id, func(t *testing.T) {
			op := &WildFlowOperation{ProductModelRef: tc.id, BillingUnit: "audio_millisecond",
				BillingAmountMicros: tc.reserve, BillingBillableUnits: 7_200_000, BillingQuota: 100_000}
			amount, _, _, units, err := wildFlowActualUsageBilling(op, &WildFlowUsageEvent{Quantity: 60_001})
			require.NoError(t, err)
			assert.Equal(t, tc.expected, amount)
			assert.Equal(t, int64(60_001), units)
			for _, invalid := range []int64{0, -1, 7_200_001} {
				_, _, _, _, err := wildFlowActualUsageBilling(op, &WildFlowUsageEvent{Quantity: invalid})
				assert.ErrorIs(t, err, ErrWildFlowBillingStateConflict)
			}
		})
	}
}
