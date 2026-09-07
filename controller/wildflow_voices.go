package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/QuantumNous/new-api/internal/inferenceclient"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

func voiceClient(c *gin.Context) (*inferenceclient.Client, bool) {
	if !wildFlowTokenAllowsModel(c, service.WildFlowModelIndexTTS25) {
		wildFlowJobError(c, 403, "model_forbidden", "token is not allowed to use this model")
		return nil, false
	}
	client, err := newWildFlowInferenceClient()
	if err != nil {
		wildFlowJobError(c, 503, "inference_unavailable", "inference service is unavailable")
		return nil, false
	}
	return client, true
}

func CreateWildFlowVoice(c *gin.Context) {
	client, ok := voiceClient(c)
	if !ok {
		return
	}
	media, params, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || media != "audio/wav" || len(params) != 0 {
		wildFlowJobError(c, 415, "unsupported_media_type", "voice must be PCM16 WAV")
		return
	}
	size := c.Request.ContentLength
	if size < 44 || size > 12<<20 {
		wildFlowJobError(c, 400, "invalid_voice_size", "voice must have a bounded size between 44 bytes and 12 MiB")
		return
	}
	name := strings.TrimSpace(c.GetHeader("X-WildFlow-Voice-Name"))
	if decoded, err := url.PathUnescape(name); err == nil {
		name = strings.TrimSpace(decoded)
	} else {
		wildFlowJobError(c, 400, "invalid_voice_name", "invalid voice name encoding")
		return
	}
	digest := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(c.GetHeader("X-WildFlow-Content-SHA256"))), "sha256:")
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != sha256.Size || name == "" || len(name) > 200 || strings.ContainsAny(name, "\r\n") {
		wildFlowJobError(c, 400, "invalid_voice", "voice name and SHA-256 are required")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 12<<20)
	voice, err := client.UploadVoice(c.Request.Context(), c.Request.Body, size, digest, wildFlowTenantRef(c.GetInt("id")), name)
	if err != nil {
		writeWildFlowInputArtifactError(c, err)
		return
	}
	c.JSON(201, voice)
}

func ListWildFlowVoices(c *gin.Context) {
	client, ok := voiceClient(c)
	if !ok {
		return
	}
	voices, err := client.ListVoices(c.Request.Context(), wildFlowTenantRef(c.GetInt("id")), c.Query("after"))
	if err != nil {
		writeWildFlowInputArtifactError(c, err)
		return
	}
	c.JSON(200, voices)
}

func GetWildFlowVoice(c *gin.Context) {
	client, ok := voiceClient(c)
	if !ok {
		return
	}
	voice, err := client.GetVoice(c.Request.Context(), c.Param("voice_id"), wildFlowTenantRef(c.GetInt("id")))
	if err != nil {
		writeWildFlowInputArtifactError(c, err)
		return
	}
	c.JSON(200, voice)
}

func DownloadWildFlowVoice(c *gin.Context) {
	client, ok := voiceClient(c)
	if !ok {
		return
	}
	data, err := client.VoiceContent(c.Request.Context(), c.Param("voice_id"), wildFlowTenantRef(c.GetInt("id")))
	if err != nil {
		writeWildFlowInputArtifactError(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	c.Data(200, "audio/wav", data)
}

func CreateWildFlowVoiceJob(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, wildFlowJobRequestLimit)
	request, err := decodeWildFlowJobRequest(c.Request.Body)
	if err != nil || request.Model != service.WildFlowModelIndexTTS25 {
		wildFlowJobError(c, 400, "invalid_request", "IndexTTS-2.5 is required")
		return
	}
	createWildFlowJob(c, request)
}

func GetWildFlowVoicePreference(c *gin.Context) {
	user, err := model.GetUserById(c.GetInt("id"), true)
	if err != nil {
		wildFlowInternalError(c, err)
		return
	}
	settings := user.GetSetting()
	c.JSON(200, gin.H{"voice_id": settings.IndexTTSDefaultVoice, "content_accounts": settings.IndexTTSAccountVoices})
}

func SetWildFlowVoicePreference(c *gin.Context) {
	client, ok := voiceClient(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1024)
	var request struct {
		VoiceID        string `json:"voice_id"`
		ContentAccount string `json:"content_account"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		wildFlowJobError(c, 400, "invalid_request", "voice_id is required")
		return
	}
	request.ContentAccount = strings.TrimSpace(request.ContentAccount)
	if len(request.ContentAccount) > 200 || strings.ContainsAny(request.ContentAccount, "\r\n\x00") {
		wildFlowJobError(c, 400, "invalid_request", "content_account must be at most 200 bytes without control characters")
		return
	}
	voice, err := client.GetVoice(c.Request.Context(), request.VoiceID, wildFlowTenantRef(c.GetInt("id")))
	if err != nil {
		writeWildFlowInputArtifactError(c, err)
		return
	}
	if voice.RetentionState != "active" {
		wildFlowJobError(c, 400, "invalid_voice", "voice is unavailable")
		return
	}
	user, err := model.GetUserById(c.GetInt("id"), true)
	if err != nil {
		wildFlowInternalError(c, err)
		return
	}
	settings := user.GetSetting()
	if request.ContentAccount == "" {
		settings.IndexTTSDefaultVoice = voice.ID
	} else {
		if settings.IndexTTSAccountVoices == nil {
			settings.IndexTTSAccountVoices = map[string]string{}
		}
		if _, exists := settings.IndexTTSAccountVoices[request.ContentAccount]; !exists && len(settings.IndexTTSAccountVoices) >= 100 {
			wildFlowJobError(c, 400, "invalid_request", "at most 100 content accounts are supported")
			return
		}
		settings.IndexTTSAccountVoices[request.ContentAccount] = voice.ID
	}
	if err := model.UpdateUserSetting(user.Id, settings); err != nil {
		wildFlowInternalError(c, err)
		return
	}
	c.JSON(200, gin.H{"voice_id": voice.ID, "content_account": request.ContentAccount})
}
