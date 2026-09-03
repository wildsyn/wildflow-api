package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/google/uuid"
)

var (
	ErrWildFlowIdempotencyRequired = errors.New("Idempotency-Key is required")
	ErrWildFlowIdempotencyConflict = errors.New("Idempotency-Key was already used with a different request")
	ErrWildFlowUnsupportedModel    = errors.New("unsupported WildFlow model")
	ErrWildFlowRetiredModel        = errors.New("retired WildFlow model")
	ErrWildFlowInvalidParameters   = errors.New("invalid model parameters")
)

type WildFlowJobRequest struct {
	Model            string         `json:"model"`
	InputArtifactIDs []string       `json:"input_artifact_ids,omitempty"`
	Parameters       map[string]any `json:"parameters"`
}

const (
	WildFlowModelVoxCPM2          = "VoxCPM2"
	WildFlowModelFlux2            = "FLUX.2 [klein] 4B"
	WildFlowModelIdeogram4MixedV3 = "Ideogram 4 mixed-v3"
	WildFlowModelInternalASR      = "wildflow/internal-vibevoice-faster-whisper-asr-v1"
	WildFlowModelExamDualASR      = "wildflow/exam-replay-dual-asr-v1"
	wildFlowIdeogram4Entitlement  = "internal-noncommercial-evaluation-only"
)

var wildFlowTTSVoices = map[string]struct{}{
	"shuoshuren": {},
	"dabin":      {},
	"tingting":   {},
	"default":    {},
	"wangliqun":  {},
}

func NormalizeWildFlowJobRequest(request WildFlowJobRequest) (WildFlowJobRequest, error) {
	request.Model = strings.TrimSpace(request.Model)
	parameters := make(map[string]any, len(request.Parameters)+1)
	for key, value := range request.Parameters {
		parameters[key] = value
	}
	request.Parameters = parameters
	request.InputArtifactIDs = append([]string{}, request.InputArtifactIDs...)

	switch request.Model {
	case WildFlowModelVoxCPM2:
	case "tts-standard", "tts-voxcpm2", "voxcpm2", "openbmb/VoxCPM2":
		request.Model = WildFlowModelVoxCPM2
		if _, exists := request.Parameters["voice"]; !exists {
			request.Parameters["voice"] = "default"
		}
	case "tts-premium":
		request.Model = WildFlowModelVoxCPM2
		request.Parameters["voice"] = "wangliqun"
	case WildFlowModelFlux2:
	case "flux2-klein-4b", "flux2", "black-forest-labs/FLUX.2-klein-4B":
		request.Model = WildFlowModelFlux2
	case WildFlowModelIdeogram4MixedV3:
		if _, exists := request.Parameters["guidance_scale"]; !exists {
			request.Parameters["guidance_scale"] = float64(7)
		}
	case WildFlowModelInternalASR:
	case WildFlowModelExamDualASR:
		return WildFlowJobRequest{}, ErrWildFlowRetiredModel
	default:
		return WildFlowJobRequest{}, ErrWildFlowUnsupportedModel
	}

	return request, nil
}

func PrepareWildFlowOperation(
	userID int,
	tokenID int,
	idempotencyKey string,
	requestID string,
	request WildFlowJobRequest,
) (*model.WildFlowOperation, bool, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" || len(idempotencyKey) > 200 {
		return nil, false, ErrWildFlowIdempotencyRequired
	}
	request, err := NormalizeWildFlowJobRequest(request)
	if err != nil {
		return nil, false, err
	}
	offering, ok := findWildFlowJobOffering(request.Model)
	if !ok {
		return nil, false, ErrWildFlowUnsupportedModel
	}
	if err := validateWildFlowRequest(offering.Kind, request); err != nil {
		return nil, false, err
	}
	canonical, err := common.Marshal(request)
	if err != nil {
		return nil, false, fmt.Errorf("encode WildFlow request: %w", err)
	}
	requestDigest := sha256Hex(canonical)
	keyDigest := sha256Hex([]byte(idempotencyKey))
	existing, err := model.GetWildFlowOperationByUserAndKey(userID, keyDigest)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		if existing.RequestDigest != requestDigest {
			return nil, false, ErrWildFlowIdempotencyConflict
		}
		return existing, false, nil
	}

	operation := &model.WildFlowOperation{
		OperationID:            "op-" + uuid.NewString(),
		UserID:                 userID,
		TokenID:                tokenID,
		IdempotencyKeyDigest:   keyDigest,
		RequestDigest:          requestDigest,
		RequestID:              requestID,
		ProductModelRef:        offering.ID,
		ModelVersionRef:        offering.ModelVersionRef,
		State:                  "submitting",
		SubmissionPhase:        model.WildFlowSubmissionPhasePrepared,
		SubmissionRetryUntil:   WildFlowSubmissionRetryDeadline(),
		ResultRetentionSeconds: int64(wildFlowOperationResultRetention() / time.Second),
	}
	if offering.Pricing.Unit == "team_trial" {
		operation.BillingSource = model.WildFlowBillingSourceTeamTrial
	}
	if err := model.CreateWildFlowOperation(operation); err != nil {
		existing, lookupErr := model.GetWildFlowOperationByUserAndKey(userID, keyDigest)
		if lookupErr != nil {
			return nil, false, lookupErr
		}
		if existing == nil {
			return nil, false, err
		}
		if existing.RequestDigest != requestDigest {
			return nil, false, ErrWildFlowIdempotencyConflict
		}
		return existing, false, nil
	}
	return operation, true, nil
}

func findWildFlowJobOffering(id string) (WildFlowOffering, bool) {
	return FindWildFlowOffering(id)
}

func FindWildFlowOffering(id string) (WildFlowOffering, bool) {
	for _, offering := range canonicalWildFlowCatalog {
		if offering.ID == id {
			return offering, true
		}
	}
	return WildFlowOffering{}, false
}

// ResolveWildFlowRuntimeOfferingRef maps the public model identity persisted on
// an Operation to the exact Offering identity accepted by inference. The model
// version is checked independently so catalog drift fails before submission.
func ResolveWildFlowRuntimeOfferingRef(publicModelRef string, modelVersionRef string) (string, error) {
	if publicModelRef == WildFlowModelExamDualASR && modelVersionRef == WildFlowModelExamDualASR {
		return "exam-replay-dual-asr", nil
	}
	offering, ok := FindWildFlowOffering(publicModelRef)
	if !ok || offering.ModelVersionRef != modelVersionRef {
		return "", ErrWildFlowUnsupportedModel
	}
	if offering.RuntimeOfferingID != "" {
		return offering.RuntimeOfferingID, nil
	}
	return offering.ID, nil
}

func IsWildFlowASRModelRef(modelRef string) bool {
	return modelRef == WildFlowModelInternalASR || modelRef == WildFlowModelExamDualASR
}

// PrepareWildFlowRuntimeParameters copies public parameters into the trusted
// inference request and appends any entitlement that only the API may provide.
func PrepareWildFlowRuntimeParameters(publicModelRef string, modelVersionRef string, parameters map[string]any) (map[string]any, error) {
	offering, ok := FindWildFlowOffering(publicModelRef)
	if !ok || offering.ModelVersionRef != modelVersionRef {
		return nil, ErrWildFlowUnsupportedModel
	}
	runtimeParameters := make(map[string]any, len(parameters)+1)
	for key, value := range parameters {
		runtimeParameters[key] = value
	}
	if offering.ID == WildFlowModelIdeogram4MixedV3 {
		runtimeParameters["license_entitlement"] = wildFlowIdeogram4Entitlement
	}
	return runtimeParameters, nil
}

func IsWildFlowTeamTrialOffering(publicModelRef string) bool {
	offering, ok := FindWildFlowOffering(publicModelRef)
	return ok && offering.Pricing.Unit == "team_trial"
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func validateWildFlowParameters(kind string, parameters map[string]any) error {
	if parameters == nil || len(parameters) == 0 || len(parameters) > 20 {
		return ErrWildFlowInvalidParameters
	}
	if kind == "tts" {
		return validateWildFlowTTSParameters(parameters)
	}
	if kind == "image" {
		return validateWildFlowImageParameters(parameters)
	}
	return ErrWildFlowUnsupportedModel
}

func validateWildFlowIdeogram4Parameters(parameters map[string]any) error {
	allowed := map[string]bool{
		"prompt": true, "width": true, "height": true, "seed": true, "steps": true, "guidance_scale": true,
	}
	for key := range parameters {
		if !allowed[key] {
			return ErrWildFlowInvalidParameters
		}
	}
	prompt, ok := parameters["prompt"].(string)
	if !ok || strings.TrimSpace(prompt) == "" || utf8.RuneCountInString(prompt) > 4_000 {
		return ErrWildFlowInvalidParameters
	}
	width, widthOK := boundedInteger(parameters["width"], 1_024, 1_536)
	height, heightOK := boundedInteger(parameters["height"], 1_024, 1_536)
	if !widthOK || !heightOK || !((width == 1_024 && height == 1_024) ||
		(width == 1_024 && height == 1_536) || (width == 1_536 && height == 1_024)) {
		return ErrWildFlowInvalidParameters
	}
	if _, valid := boundedInteger(parameters["seed"], 0, 4_294_967_295); !valid {
		return ErrWildFlowInvalidParameters
	}
	if _, valid := boundedInteger(parameters["steps"], 1, 100); !valid {
		return ErrWildFlowInvalidParameters
	}
	guidanceScale, valid := finiteNumber(parameters["guidance_scale"])
	if !valid || guidanceScale <= 0 || guidanceScale > 30 {
		return ErrWildFlowInvalidParameters
	}
	return nil
}

func validateWildFlowRequest(kind string, request WildFlowJobRequest) error {
	if kind == "asr" {
		return validateWildFlowASRRequest(request)
	}
	if len(request.InputArtifactIDs) != 0 {
		return ErrWildFlowInvalidParameters
	}
	if request.Model == WildFlowModelIdeogram4MixedV3 {
		return validateWildFlowIdeogram4Parameters(request.Parameters)
	}
	return validateWildFlowParameters(kind, request.Parameters)
}

func validateWildFlowASRRequest(request WildFlowJobRequest) error {
	if len(request.InputArtifactIDs) != 1 || !validWildFlowResourceID(request.InputArtifactIDs[0]) ||
		request.Parameters == nil || len(request.Parameters) > 4 {
		return ErrWildFlowInvalidParameters
	}
	allowed := map[string]bool{
		"language": true, "context": true, "hotwords": true, "source_offset_seconds": true,
	}
	for key := range request.Parameters {
		if !allowed[key] {
			return ErrWildFlowInvalidParameters
		}
	}
	if raw, exists := request.Parameters["language"]; exists {
		language, ok := raw.(string)
		if !ok || utf8.RuneCountInString(language) < 1 || utf8.RuneCountInString(language) > 16 || strings.TrimSpace(language) != language {
			return ErrWildFlowInvalidParameters
		}
	}
	if raw, exists := request.Parameters["context"]; exists {
		context, ok := raw.(string)
		if !ok || utf8.RuneCountInString(context) > 20_000 {
			return ErrWildFlowInvalidParameters
		}
	}
	if raw, exists := request.Parameters["hotwords"]; exists {
		hotwords, ok := raw.([]any)
		if !ok || len(hotwords) > 200 {
			return ErrWildFlowInvalidParameters
		}
		for _, rawHotword := range hotwords {
			hotword, ok := rawHotword.(string)
			if !ok || utf8.RuneCountInString(hotword) < 1 || utf8.RuneCountInString(hotword) > 100 || strings.TrimSpace(hotword) != hotword {
				return ErrWildFlowInvalidParameters
			}
		}
	}
	if raw, exists := request.Parameters["source_offset_seconds"]; exists {
		offset, ok := finiteNumber(raw)
		if !ok || offset < 0 || offset > 7_200 {
			return ErrWildFlowInvalidParameters
		}
	}
	return nil
}

func validWildFlowResourceID(value string) bool {
	if len(value) < 1 || len(value) > 200 {
		return false
	}
	for index, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || (index > 0 && strings.ContainsRune("._-", character)) {
			continue
		}
		return false
	}
	return true
}

func validateWildFlowTTSParameters(parameters map[string]any) error {
	allowed := map[string]bool{"input": true, "text": true, "voice": true, "speed": true}
	for key := range parameters {
		if !allowed[key] {
			return ErrWildFlowInvalidParameters
		}
	}
	input, hasInput := parameters["input"]
	text, hasText := parameters["text"]
	if hasInput == hasText {
		return ErrWildFlowInvalidParameters
	}
	value := input
	if hasText {
		value = text
	}
	content, ok := value.(string)
	if !ok || utf8.RuneCountInString(content) == 0 || utf8.RuneCountInString(content) > 20_000 {
		return ErrWildFlowInvalidParameters
	}
	voiceValue, valid := parameters["voice"].(string)
	if !valid {
		return ErrWildFlowInvalidParameters
	}
	if _, allowed := wildFlowTTSVoices[voiceValue]; !allowed {
		return ErrWildFlowInvalidParameters
	}
	if speed, exists := parameters["speed"]; exists {
		value, valid := finiteNumber(speed)
		if !valid || value < 0.25 || value > 4 {
			return ErrWildFlowInvalidParameters
		}
	}
	return nil
}

func validateWildFlowImageParameters(parameters map[string]any) error {
	allowed := map[string]bool{
		"prompt": true, "width": true, "height": true,
		"num_inference_steps": true, "guidance_scale": true, "seed": true,
	}
	for key := range parameters {
		if !allowed[key] {
			return ErrWildFlowInvalidParameters
		}
	}
	prompt, ok := parameters["prompt"].(string)
	if !ok || len(prompt) == 0 || len(prompt) > 20_000 {
		return ErrWildFlowInvalidParameters
	}
	for _, key := range []string{"width", "height"} {
		if raw, exists := parameters[key]; exists {
			value, valid := boundedInteger(raw, 256, 2048)
			if !valid || value%8 != 0 {
				return ErrWildFlowInvalidParameters
			}
		}
	}
	if raw, exists := parameters["num_inference_steps"]; exists {
		if _, valid := boundedInteger(raw, 1, 100); !valid {
			return ErrWildFlowInvalidParameters
		}
	}
	if raw, exists := parameters["seed"]; exists {
		if _, valid := boundedInteger(raw, 0, math.MaxInt64); !valid {
			return ErrWildFlowInvalidParameters
		}
	}
	if raw, exists := parameters["guidance_scale"]; exists {
		value, valid := finiteNumber(raw)
		if !valid || value < 0 || value > 20 {
			return ErrWildFlowInvalidParameters
		}
	}
	return nil
}

func boundedInteger(value any, minimum int64, maximum int64) (int64, bool) {
	number, ok := finiteNumber(value)
	if !ok || math.Trunc(number) != number || number < float64(minimum) || number > float64(maximum) {
		return 0, false
	}
	return int64(number), true
}

func finiteNumber(value any) (float64, bool) {
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, false
	}
	return number, true
}
