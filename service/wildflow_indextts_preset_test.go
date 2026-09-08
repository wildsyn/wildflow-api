package service

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestSharedWangliqunSkipsTenantArtifactResolution(t *testing.T) {
	assert.Empty(t, WildFlowRuntimeInputArtifactIDs(WildFlowModelIndexTTS25, map[string]any{"voice_id": "wangliqun", "emotion_voice_id": "wangliqun"}, nil))
	assert.Equal(t, []string{"wangliqun-other"}, WildFlowRuntimeInputArtifactIDs(WildFlowModelIndexTTS25, map[string]any{"voice_id": "wangliqun-other"}, nil))
}
