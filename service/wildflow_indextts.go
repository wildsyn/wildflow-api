package service

import (
	"encoding/json"
	"math"
	"regexp"
	"strings"
)

var indexTTSVoiceID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)

func validIndexTTSControls(parameters map[string]any) bool {
	emotionModes := 0
	for key, value := range parameters {
		switch key {
		case "text":
		case "voice_id", "emotion_voice_id":
			id, ok := value.(string)
			if !ok || !indexTTSVoiceID.MatchString(id) {
				return false
			}
			if key == "emotion_voice_id" {
				emotionModes++
			}
		case "lang":
			switch value {
			case "zh", "en", "ja", "es", "ar":
			default:
				return false
			}
		case "stream", "use_random", "text_normalization", "use_emo_text", "do_sample":
			flag, ok := value.(bool)
			if !ok {
				return false
			}
			if key == "use_emo_text" && flag {
				emotionModes++
			}
		case "emo_text":
			text, ok := value.(string)
			if !ok || strings.TrimSpace(text) == "" || len(text) > 2048 || parameters["use_emo_text"] != true {
				return false
			}
		case "emo_vector":
			vector, ok := value.([]any)
			if !ok || len(vector) != 8 {
				return false
			}
			for _, item := range vector {
				if !indexTTSNumberInRange(item, 0, 1, false) {
					return false
				}
			}
			emotionModes++
		case "duration_factor":
			if !indexTTSNumberInRange(value, 0.5, 2, false) {
				return false
			}
		case "emo_alpha":
			if !indexTTSNumberInRange(value, 0, 1, false) {
				return false
			}
		case "top_p":
			if !indexTTSNumberInRange(value, 0.01, 1, false) {
				return false
			}
		case "temperature":
			if !indexTTSNumberInRange(value, 0.1, 2, false) {
				return false
			}
		case "repetition_penalty":
			if !indexTTSNumberInRange(value, 0.1, 20, false) {
				return false
			}
		case "top_k":
			if !indexTTSNumberInRange(value, 1, 100, true) {
				return false
			}
		case "interval_silence":
			if !indexTTSNumberInRange(value, 0, 2000, true) {
				return false
			}
		case "max_text_tokens_per_segment":
			if !indexTTSNumberInRange(value, 20, 200, true) {
				return false
			}
		default:
			return false
		}
	}
	return emotionModes <= 1
}

func indexTTSNumberInRange(value any, min, max float64, integer bool) bool {
	var number float64
	switch v := value.(type) {
	case float64:
		number = v
	case int:
		number = float64(v)
	case json.Number:
		var err error
		number, err = v.Float64()
		if err != nil {
			return false
		}
	default:
		return false
	}
	return !math.IsNaN(number) && !math.IsInf(number, 0) && number >= min && number <= max && (!integer || math.Trunc(number) == number)
}

// WildFlowRuntimeInputArtifactIDs resolves immutable custom voices while leaving
// the caller's normalized request (and its idempotency digest) unchanged.
func WildFlowRuntimeInputArtifactIDs(modelRef string, parameters map[string]any, inputs []string) []string {
	if modelRef != WildFlowModelIndexTTS25 {
		return inputs
	}
	result := []string{}
	for _, key := range []string{"voice_id", "emotion_voice_id"} {
		id, _ := parameters[key].(string)
		if id == "" || id == "legacy-default-v1" || id == "wangliqun" || id == "wangxiaozhang" || strings.HasPrefix(id, "official-") {
			continue
		}
		if len(result) == 0 || result[0] != id {
			result = append(result, id)
		}
	}
	return result
}
