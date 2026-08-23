package service

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWildFlowOperationResultRetentionHasExplicitDefaultAndOverride(t *testing.T) {
	t.Setenv("WILDFLOW_OPERATION_RESULT_RETENTION_SECONDS", "")
	require.Equal(t, 30*24*time.Hour, wildFlowOperationResultRetention())

	t.Setenv("WILDFLOW_OPERATION_RESULT_RETENTION_SECONDS", "3600")
	require.Equal(t, time.Hour, wildFlowOperationResultRetention())

	t.Setenv("WILDFLOW_OPERATION_RESULT_RETENTION_SECONDS", "59")
	require.Equal(t, 30*24*time.Hour, wildFlowOperationResultRetention())
}

func TestValidateWildFlowParametersRejectsUnsafeOrUnsupportedShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		kind       string
		parameters map[string]any
	}{
		{name: "missing tts text", kind: "tts", parameters: map[string]any{"voice": "default"}},
		{name: "missing required tts voice", kind: "tts", parameters: map[string]any{"input": "a"}},
		{name: "unknown tts voice", kind: "tts", parameters: map[string]any{"input": "a", "voice": "narrator"}},
		{name: "ambiguous tts text", kind: "tts", parameters: map[string]any{"input": "a", "text": "b"}},
		{name: "unknown tts option", kind: "tts", parameters: map[string]any{"input": "a", "callback": "https://example.com"}},
		{name: "invalid image dimension", kind: "image", parameters: map[string]any{"prompt": "a", "width": float64(257)}},
		{name: "oversized image", kind: "image", parameters: map[string]any{"prompt": "a", "width": float64(4096)}},
		{name: "unknown image option", kind: "image", parameters: map[string]any{"prompt": "a", "url": "file:///etc/passwd"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.ErrorIs(t, validateWildFlowParameters(test.kind, test.parameters), ErrWildFlowInvalidParameters)
		})
	}
}

func TestNormalizeWildFlowJobRequestKeepsTwoCanonicalModelsAndMapsLegacyAliases(t *testing.T) {
	t.Parallel()

	canonical, err := NormalizeWildFlowJobRequest(WildFlowJobRequest{
		Model:      "VoxCPM2",
		Parameters: map[string]any{"input": "hello", "voice": "tingting"},
	})
	require.NoError(t, err)
	require.Equal(t, "VoxCPM2", canonical.Model)
	require.Equal(t, "tingting", canonical.Parameters["voice"])

	legacyPremium, err := NormalizeWildFlowJobRequest(WildFlowJobRequest{
		Model:      "tts-premium",
		Parameters: map[string]any{"input": "hello"},
	})
	require.NoError(t, err)
	require.Equal(t, "VoxCPM2", legacyPremium.Model)
	require.Equal(t, "wangliqun", legacyPremium.Parameters["voice"])

	legacyImage, err := NormalizeWildFlowJobRequest(WildFlowJobRequest{
		Model:      "flux2-klein-4b",
		Parameters: map[string]any{"prompt": "panda"},
	})
	require.NoError(t, err)
	require.Equal(t, "FLUX.2 [klein] 4B", legacyImage.Model)
}

func TestValidateWildFlowParametersAcceptsEveryDocumentedVoice(t *testing.T) {
	t.Parallel()

	for _, voice := range []string{"shuoshuren", "dabin", "tingting", "default", "wangliqun"} {
		t.Run(voice, func(t *testing.T) {
			require.NoError(t, validateWildFlowParameters("tts", map[string]any{
				"input": "hello",
				"voice": voice,
			}))
		})
	}
}

func TestNormalizeAndValidateInternalExamDualASRRequest(t *testing.T) {
	t.Parallel()

	request, err := NormalizeWildFlowJobRequest(WildFlowJobRequest{
		Model:            WildFlowModelExamDualASR,
		InputArtifactIDs: []string{"input-artifact-1"},
		Parameters: map[string]any{
			"language": "zh", "context": "申论课程",
			"hotwords": []any{"青蜂六边形", "归纳概括"}, "source_offset_seconds": float64(0),
		},
	})
	require.NoError(t, err)
	require.Equal(t, WildFlowModelExamDualASR, request.Model)
	offering, ok := findWildFlowJobOffering(request.Model)
	require.True(t, ok)
	require.Equal(t, "asr", offering.Kind)
	require.NoError(t, validateWildFlowRequest(offering.Kind, request))

	invalid := []WildFlowJobRequest{
		{Model: WildFlowModelExamDualASR, Parameters: map[string]any{}},
		{Model: WildFlowModelExamDualASR, InputArtifactIDs: []string{"input-1", "input-2"}, Parameters: map[string]any{}},
		{Model: WildFlowModelExamDualASR, InputArtifactIDs: []string{"../private"}, Parameters: map[string]any{}},
		{Model: WildFlowModelExamDualASR, InputArtifactIDs: []string{"input-1"}, Parameters: map[string]any{"context": strings.Repeat("x", 20_001)}},
		{Model: WildFlowModelExamDualASR, InputArtifactIDs: []string{"input-1"}, Parameters: map[string]any{"hotwords": []any{strings.Repeat("x", 101)}}},
		{Model: WildFlowModelExamDualASR, InputArtifactIDs: []string{"input-1"}, Parameters: map[string]any{"source_offset_seconds": float64(7201)}},
	}
	for _, candidate := range invalid {
		candidate, err = NormalizeWildFlowJobRequest(candidate)
		require.NoError(t, err)
		require.ErrorIs(t, validateWildFlowRequest("asr", candidate), ErrWildFlowInvalidParameters)
	}
}

func TestPublicWildFlowCatalogExposesTeamDualASR(t *testing.T) {
	t.Parallel()

	public, visible := FindWildFlowOffering(WildFlowModelExamDualASR)
	require.True(t, visible)
	require.Equal(t, WildFlowModelExamDualASR, public.ModelVersionRef)
	require.Equal(t, "直播回放双 ASR", public.DisplayName)
}
