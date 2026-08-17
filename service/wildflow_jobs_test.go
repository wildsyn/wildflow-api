package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

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
