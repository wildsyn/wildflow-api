package service

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/internal/inferenceclient"
	"github.com/QuantumNous/new-api/model"
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

func TestASRProductArtifactMustProveSelectedEngine(t *testing.T) {
	for _, tc := range []struct{ id, mode string }{
		{WildFlowModelWhisperASR, "whisper"}, {WildFlowModelVibeVoiceASR, "vibevoice"},
		{WildFlowModelExamDualASR, "dual"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			artifact := inferenceclient.Artifact{ID: "artifact-1", MediaType: "application/json", SizeBytes: 128,
				SHA256: strings.Repeat("a", 64), Metadata: map[string]any{
					"schema_version": float64(1), "duration_seconds": float64(60), "source_artifact_id": "input-1",
					"model_version_ref":             wildFlowModelVersionDualASR,
					"model_revision":                "vibevoice-d0c9efdb-plus-faster-whisper-edaa852e",
					"vibevoice_model_revision":      "d0c9efdb8d614685062c04425d91e01b6f37d944",
					"faster_whisper_model_revision": "edaa852ec7e145841d8ffdb056a99866b5f0a478",
					"runtime_version_ref":           "exam-dual-asr-http-runtime-v1-0123456789ab",
					"asr_engine":                    tc.mode,
				}}
			op := &model.WildFlowOperation{ProductModelRef: tc.id}
			require.NoError(t, ValidateWildFlowCompletedArtifacts(op, []inferenceclient.Artifact{artifact}))
			for _, wrong := range []any{"unknown", float64(1), nil} {
				artifact.Metadata["asr_engine"] = wrong
				assert.ErrorIs(t, ValidateWildFlowCompletedArtifacts(op, []inferenceclient.Artifact{artifact}), ErrWildFlowInvalidArtifact)
			}
			delete(artifact.Metadata, "asr_engine")
			err := ValidateWildFlowCompletedArtifacts(op, []inferenceclient.Artifact{artifact})
			if tc.mode == "dual" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrWildFlowInvalidArtifact)
			}
		})
	}
}
