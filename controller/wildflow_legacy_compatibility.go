package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

func CreateWildFlowLegacySpeechJob(c *gin.Context) {
	request, _, err := decodeWildFlowLegacySpeechRequest(c.Request.Body, true)
	if err != nil {
		wildFlowJobError(c, http.StatusBadRequest, "invalid_request", "invalid speech request")
		return
	}
	createWildFlowJob(c, request)
}

func CreateWildFlowLegacyImageJob(c *gin.Context) {
	request, _, err := decodeWildFlowLegacyImageRequest(c.Request.Body, true)
	if err != nil {
		wildFlowJobError(c, http.StatusBadRequest, "invalid_request", "invalid image request")
		return
	}
	createWildFlowJob(c, request)
}

func TryWildFlowSpeechCompatibility(c *gin.Context) {
	body, err := readAndRestoreWildFlowBody(c)
	if err != nil {
		wildFlowJobError(c, http.StatusBadRequest, "invalid_request", "invalid speech request")
		c.Abort()
		return
	}
	request, recognized, err := decodeWildFlowLegacySpeechRequest(bytes.NewReader(body), false)
	if !recognized {
		return
	}
	if err != nil {
		wildFlowJobError(c, http.StatusBadRequest, "invalid_request", "invalid speech request")
		c.Abort()
		return
	}
	createWildFlowJob(c, request)
	c.Abort()
}

func TryWildFlowImageCompatibility(c *gin.Context) {
	body, err := readAndRestoreWildFlowBody(c)
	if err != nil {
		wildFlowJobError(c, http.StatusBadRequest, "invalid_request", "invalid image request")
		c.Abort()
		return
	}
	request, recognized, err := decodeWildFlowLegacyImageRequest(bytes.NewReader(body), false)
	if !recognized {
		return
	}
	if err != nil {
		wildFlowJobError(c, http.StatusBadRequest, "invalid_request", "invalid image request")
		c.Abort()
		return
	}
	createWildFlowJob(c, request)
	c.Abort()
}

func decodeWildFlowLegacySpeechRequest(
	reader io.Reader,
	defaultModel bool,
) (service.WildFlowJobRequest, bool, error) {
	fields, err := decodeWildFlowLegacyFields(reader)
	if err != nil {
		return service.WildFlowJobRequest{}, true, err
	}
	modelName, err := optionalWildFlowString(fields, "model")
	if err != nil {
		return service.WildFlowJobRequest{}, true, err
	}
	if modelName == "" && defaultModel {
		modelName = "tts-voxcpm2"
	}
	if modelName == "" {
		return service.WildFlowJobRequest{}, false, nil
	}
	if _, probeErr := service.NormalizeWildFlowJobRequest(service.WildFlowJobRequest{
		Model: modelName, Parameters: map[string]any{},
	}); errors.Is(probeErr, service.ErrWildFlowUnsupportedModel) && !defaultModel {
		return service.WildFlowJobRequest{}, false, nil
	}
	allowed := map[string]bool{
		"model": true, "input": true, "voice": true, "speed": true, "response_format": true,
	}
	if !onlyWildFlowFields(fields, allowed) {
		return service.WildFlowJobRequest{}, true, errors.New("unsupported speech field")
	}
	parameters := make(map[string]any)
	for _, key := range []string{"input", "voice", "speed"} {
		if raw, exists := fields[key]; exists {
			var value any
			if err := common.Unmarshal(raw, &value); err != nil {
				return service.WildFlowJobRequest{}, true, err
			}
			parameters[key] = value
		}
	}
	if defaultModel {
		if _, exists := parameters["voice"]; !exists {
			parameters["voice"] = "wangliqun"
		}
	}
	if format, err := optionalWildFlowString(fields, "response_format"); err != nil || (format != "" && format != "wav") {
		return service.WildFlowJobRequest{}, true, errors.New("unsupported speech response format")
	}
	request, normalizeErr := service.NormalizeWildFlowJobRequest(service.WildFlowJobRequest{
		Model: modelName, Parameters: parameters,
	})
	if errors.Is(normalizeErr, service.ErrWildFlowUnsupportedModel) && !defaultModel {
		return service.WildFlowJobRequest{}, false, nil
	}
	return request, true, normalizeErr
}

func decodeWildFlowLegacyImageRequest(
	reader io.Reader,
	defaultModel bool,
) (service.WildFlowJobRequest, bool, error) {
	fields, err := decodeWildFlowLegacyFields(reader)
	if err != nil {
		return service.WildFlowJobRequest{}, true, err
	}
	modelName, err := optionalWildFlowString(fields, "model")
	if err != nil {
		return service.WildFlowJobRequest{}, true, err
	}
	if modelName == "" && defaultModel {
		modelName = "flux2-klein-4b"
	}
	if modelName == "" {
		return service.WildFlowJobRequest{}, false, nil
	}
	if _, probeErr := service.NormalizeWildFlowJobRequest(service.WildFlowJobRequest{
		Model: modelName, Parameters: map[string]any{},
	}); errors.Is(probeErr, service.ErrWildFlowUnsupportedModel) && !defaultModel {
		return service.WildFlowJobRequest{}, false, nil
	}
	allowed := map[string]bool{
		"model": true, "prompt": true, "width": true, "height": true,
		"num_inference_steps": true, "guidance_scale": true, "seed": true,
	}
	if !onlyWildFlowFields(fields, allowed) {
		return service.WildFlowJobRequest{}, true, errors.New("unsupported image field")
	}
	parameters := make(map[string]any)
	for _, key := range []string{"prompt", "width", "height", "num_inference_steps", "guidance_scale", "seed"} {
		if raw, exists := fields[key]; exists {
			var value any
			if err := common.Unmarshal(raw, &value); err != nil {
				return service.WildFlowJobRequest{}, true, err
			}
			parameters[key] = value
		}
	}
	request, normalizeErr := service.NormalizeWildFlowJobRequest(service.WildFlowJobRequest{
		Model: modelName, Parameters: parameters,
	})
	if errors.Is(normalizeErr, service.ErrWildFlowUnsupportedModel) && !defaultModel {
		return service.WildFlowJobRequest{}, false, nil
	}
	return request, true, normalizeErr
}

func decodeWildFlowLegacyFields(reader io.Reader) (map[string]json.RawMessage, error) {
	body, err := io.ReadAll(io.LimitReader(reader, wildFlowJobRequestLimit+1))
	if err != nil || len(body) > wildFlowJobRequestLimit {
		return nil, errors.New("request body too large")
	}
	var fields map[string]json.RawMessage
	if err := common.Unmarshal(body, &fields); err != nil || fields == nil {
		return nil, errors.New("invalid JSON request")
	}
	return fields, nil
}

func optionalWildFlowString(fields map[string]json.RawMessage, key string) (string, error) {
	raw, exists := fields[key]
	if !exists {
		return "", nil
	}
	var value string
	if err := common.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return value, nil
}

func onlyWildFlowFields(fields map[string]json.RawMessage, allowed map[string]bool) bool {
	for key := range fields {
		if !allowed[key] {
			return false
		}
	}
	return true
}

func readAndRestoreWildFlowBody(c *gin.Context) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, wildFlowJobRequestLimit+1))
	if err != nil || len(body) > wildFlowJobRequestLimit {
		return nil, errors.New("request body too large")
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}
