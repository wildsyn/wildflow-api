package service

import (
	"github.com/QuantumNous/new-api/internal/inferenceclient"
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestSharedWangliqunSkipsTenantArtifactResolution(t *testing.T) {
	assert.Empty(t, WildFlowRuntimeInputArtifactIDs(WildFlowModelIndexTTS25, map[string]any{"voice_id": "wangliqun", "emotion_voice_id": "wangliqun"}, nil))
	assert.Equal(t, []string{"wangliqun-other"}, WildFlowRuntimeInputArtifactIDs(WildFlowModelIndexTTS25, map[string]any{"voice_id": "wangliqun-other"}, nil))
}

func TestSharedWangxiaozhangUsesPresetAndKeepsPersonalIDsScoped(t *testing.T) {
	parameters := map[string]any{"voice_id": "wangxiaozhang", "emotion_voice_id": "personal-voice"}
	assert.Equal(t, []string{"personal-voice"}, WildFlowRuntimeInputArtifactIDs(WildFlowModelIndexTTS25, parameters, nil))
	assert.Equal(t, "wangxiaozhang", parameters["voice_id"])
	assert.Equal(t, []string{"wangxiaozhang-other"}, WildFlowRuntimeInputArtifactIDs(WildFlowModelIndexTTS25, map[string]any{"voice_id": "wangxiaozhang-other"}, nil))
	result := PublicWildFlowArtifact(inferenceclient.Artifact{Metadata: map[string]any{"voice_id": "wangxiaozhang", "audio_postprocess": "small-hall-v1", "reference_path": "private"}})
	assert.Equal(t, map[string]any{"voice_id": "wangxiaozhang", "audio_postprocess": "small-hall-v1"}, result["metadata"])
}
