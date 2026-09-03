package service

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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

func TestNormalizeWildFlowJobRequestKeepsCanonicalModelsAndMapsLegacyAliases(t *testing.T) {
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

	ideogram, err := NormalizeWildFlowJobRequest(WildFlowJobRequest{
		Model: WildFlowModelIdeogram4MixedV3,
		Parameters: map[string]any{
			"prompt": "panda", "width": float64(1024), "height": float64(1024), "seed": float64(0), "steps": float64(28),
		},
	})
	require.NoError(t, err)
	require.Equal(t, float64(7), ideogram.Parameters["guidance_scale"])
}

func TestNormalizeAndValidateIndexTTS25InternalRequest(t *testing.T) {
	t.Parallel()

	request, err := NormalizeWildFlowJobRequest(WildFlowJobRequest{
		Model:      "indextts-2.5",
		Parameters: map[string]any{"input": "野生流动内部语音测试"},
	})
	require.NoError(t, err)
	require.Equal(t, WildFlowModelIndexTTS25, request.Model)
	require.Equal(t, "野生流动内部语音测试", request.Parameters["text"])
	assert.NotContains(t, request.Parameters, "input")
	offering, ok := findWildFlowJobOffering(request.Model)
	require.True(t, ok)
	require.Equal(t, "tts", offering.Kind)
	require.Equal(t, "indextts-2.5@0b328234", offering.ModelVersionRef)
	require.Equal(t, "indextts25-internal", offering.RuntimeOfferingID)
	require.True(t, IsWildFlowTeamTrialOffering(request.Model))
	require.NoError(t, validateWildFlowRequest(offering.Kind, request))

	runtimeRef, err := ResolveWildFlowRuntimeOfferingRef(request.Model, offering.ModelVersionRef)
	require.NoError(t, err)
	require.Equal(t, "indextts25-internal", runtimeRef)

	invalid := []map[string]any{
		{"text": ""},
		{"text": " ok "},
		{"text": "ok", "reference_audio": "user.wav"},
		{"text": "ok", "voice": "custom"},
		{"text": "ok", "lang": "en"},
		{"text": strings.Repeat("x", 8193)},
	}
	for _, parameters := range invalid {
		candidate, normalizeErr := NormalizeWildFlowJobRequest(WildFlowJobRequest{
			Model: WildFlowModelIndexTTS25, Parameters: parameters,
		})
		require.NoError(t, normalizeErr)
		require.ErrorIs(t, validateWildFlowRequest("tts", candidate), ErrWildFlowInvalidParameters)
	}
}

func TestValidateWildFlowIdeogram4Parameters(t *testing.T) {
	t.Parallel()

	valid := WildFlowJobRequest{
		Model: WildFlowModelIdeogram4MixedV3,
		Parameters: map[string]any{
			"prompt": "一只熊猫", "width": float64(1024), "height": float64(1536),
			"seed": float64(4_294_967_295), "steps": float64(100), "guidance_scale": float64(30),
		},
	}
	require.NoError(t, validateWildFlowRequest("image", valid))

	invalidParameters := []map[string]any{
		{"prompt": "   ", "width": float64(1024), "height": float64(1024), "seed": float64(0), "steps": float64(1), "guidance_scale": float64(7)},
		{"prompt": strings.Repeat("x", 4_001), "width": float64(1024), "height": float64(1024), "seed": float64(0), "steps": float64(1), "guidance_scale": float64(7)},
		{"prompt": "panda", "width": float64(1536), "height": float64(1536), "seed": float64(0), "steps": float64(1), "guidance_scale": float64(7)},
		{"prompt": "panda", "width": float64(1024), "height": float64(1024), "seed": float64(4_294_967_296), "steps": float64(1), "guidance_scale": float64(7)},
		{"prompt": "panda", "width": float64(1024), "height": float64(1024), "seed": float64(0), "steps": float64(0), "guidance_scale": float64(7)},
		{"prompt": "panda", "width": float64(1024), "height": float64(1024), "seed": float64(0), "steps": float64(1), "guidance_scale": float64(0)},
		{"prompt": "panda", "width": float64(1024), "height": float64(1024), "seed": float64(0), "steps": float64(1), "guidance_scale": float64(30.1)},
		{"prompt": "panda", "width": float64(1024), "height": float64(1024), "seed": float64(0), "steps": float64(1), "guidance_scale": float64(7), "license_entitlement": "caller-controlled"},
	}
	for _, parameters := range invalidParameters {
		require.ErrorIs(t, validateWildFlowRequest("image", WildFlowJobRequest{
			Model: WildFlowModelIdeogram4MixedV3, Parameters: parameters,
		}), ErrWildFlowInvalidParameters)
	}
}

func TestPrepareWildFlowRuntimeParametersInjectsOnlyTrustedIdeogramEntitlement(t *testing.T) {
	public := map[string]any{"prompt": "panda", "width": float64(1024)}
	runtime, err := PrepareWildFlowRuntimeParameters(
		WildFlowModelIdeogram4MixedV3,
		"ideogram-4-mixed-v3@bbee2ab2",
		public,
	)
	require.NoError(t, err)
	require.Equal(t, "internal-noncommercial-evaluation-only", runtime["license_entitlement"])
	_, suppliedByClient := public["license_entitlement"]
	assert.False(t, suppliedByClient)

	retail, err := PrepareWildFlowRuntimeParameters(
		WildFlowModelFlux2,
		"black-forest-labs/FLUX.2-klein-4B",
		public,
	)
	require.NoError(t, err)
	_, hasEntitlement := retail["license_entitlement"]
	assert.False(t, hasEntitlement)

	_, err = PrepareWildFlowRuntimeParameters(WildFlowModelIdeogram4MixedV3, "ideogram-4-mixed-v3@untrusted", public)
	require.ErrorIs(t, err, ErrWildFlowUnsupportedModel)
}

func TestWildFlowDurableJobOfferingRejectsQwenChatCatalogEntry(t *testing.T) {
	t.Parallel()

	offering, ok := findWildFlowJobOffering("Qwen/Qwen3.8-27B-FP8@gpu-4090-06")
	require.False(t, ok)
	require.Empty(t, offering.ID)

	_, err := NormalizeWildFlowJobRequest(WildFlowJobRequest{
		Model:      "Qwen/Qwen3.8-27B-FP8@gpu-4090-06",
		Parameters: map[string]any{"input": "hello"},
	})
	require.ErrorIs(t, err, ErrWildFlowUnsupportedModel)
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

func TestNormalizeLegacyASRModelNameToNeutralPublicID(t *testing.T) {
	request, err := NormalizeWildFlowJobRequest(WildFlowJobRequest{
		Model: "wildflow/exam-replay-dual-asr-v1", InputArtifactIDs: []string{"input-1"}, Parameters: map[string]any{},
	})
	require.NoError(t, err)
	assert.Equal(t, "wildflow/dual-asr-v1", request.Model)
}

func TestPublicWildFlowCatalogExposesTeamDualASR(t *testing.T) {
	t.Parallel()

	public, visible := FindWildFlowOffering(WildFlowModelExamDualASR)
	require.True(t, visible)
	require.Equal(t, WildFlowModelExamDualASR, public.ModelVersionRef)
	require.Equal(t, "直播回放双 ASR", public.DisplayName)
}

func TestResolveWildFlowRuntimeOfferingRefKeepsPublicAndRuntimeIdentityDistinct(t *testing.T) {
	t.Parallel()

	runtimeRef, err := ResolveWildFlowRuntimeOfferingRef(
		WildFlowModelExamDualASR,
		WildFlowModelExamDualASR,
	)
	require.NoError(t, err)
	require.Equal(t, "exam-replay-dual-asr", runtimeRef)

	runtimeRef, err = ResolveWildFlowRuntimeOfferingRef(
		WildFlowModelIdeogram4MixedV3,
		"ideogram-4-mixed-v3@bbee2ab2",
	)
	require.NoError(t, err)
	require.Equal(t, "ideogram-4-mixed-v3", runtimeRef)

	runtimeRef, err = ResolveWildFlowRuntimeOfferingRef(
		WildFlowModelVoxCPM2,
		"openbmb/VoxCPM2",
	)
	require.NoError(t, err)
	require.Equal(t, WildFlowModelVoxCPM2, runtimeRef)

	_, err = ResolveWildFlowRuntimeOfferingRef(
		WildFlowModelExamDualASR,
		"wildflow/exam-replay-dual-asr-v0",
	)
	require.ErrorIs(t, err, ErrWildFlowUnsupportedModel)
}
