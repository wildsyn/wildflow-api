package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const wildFlowCatalogResponseLimit = 256 * 1024

type WildFlowOffering struct {
	ID                 string                 `json:"id"`
	DisplayName        string                 `json:"display_name"`
	Kind               string                 `json:"kind"`
	Vendor             string                 `json:"vendor"`
	ModelVersionRef    string                 `json:"model_version_ref"`
	Description        string                 `json:"description"`
	RequiredParameters []string               `json:"required_parameters,omitempty"`
	Voices             []WildFlowVoice        `json:"voices,omitempty"`
	Pricing            WildFlowCatalogPricing `json:"pricing"`
	Callable           bool                   `json:"callable"`
	Status             string                 `json:"status"`
	RuntimeOfferingID  string                 `json:"-"`
	Visibility         string                 `json:"-"`
}

type WildFlowVoice struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category"`
}

type WildFlowCatalogPricing struct {
	Currency string  `json:"currency"`
	Amount   float64 `json:"amount"`
	Unit     string  `json:"unit"`
	Display  string  `json:"display"`
}

type wildFlowRuntimeOffering struct {
	ID              string `json:"id"`
	ModelVersionRef string `json:"model_version_ref"`
	Callable        bool   `json:"callable"`
}

var wildFlowCatalogHTTPClient = &http.Client{
	Timeout: 3 * time.Second,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

var canonicalWildFlowCatalog = []WildFlowOffering{
	{
		ID:                 "VoxCPM2",
		DisplayName:        "VoxCPM2",
		Kind:               "tts",
		Vendor:             "OpenBMB",
		ModelVersionRef:    "openbmb/VoxCPM2",
		Description:        "支持四个内置音色与王立群克隆音色的语音合成模型。",
		RequiredParameters: []string{"voice"},
		Voices: []WildFlowVoice{
			{ID: "shuoshuren", Name: "说书人", Category: "official"},
			{ID: "dabin", Name: "大斌", Category: "official"},
			{ID: "tingting", Name: "婷婷", Category: "official"},
			{ID: "default", Name: "默认", Category: "official"},
			{ID: "wangliqun", Name: "王立群", Category: "custom"},
		},
		Pricing: WildFlowCatalogPricing{
			Currency: "CNY", Amount: 0.8, Unit: "10k_characters", Display: "¥0.8 / 万字符",
		},
	},
	{
		ID:              "FLUX.2 [klein] 4B",
		DisplayName:     "FLUX.2 [klein] 4B 图片生成",
		Kind:            "image",
		Vendor:          "Black Forest Labs",
		ModelVersionRef: "black-forest-labs/FLUX.2-klein-4B",
		Description:     "支持中英文提示词的开源图片生成模型。",
		Pricing: WildFlowCatalogPricing{
			Currency: "CNY", Amount: 0.05, Unit: "image", Display: "¥0.05 / 张",
		},
	},
	{
		ID:                 WildFlowModelIdeogram4MixedV3,
		DisplayName:        "Ideogram 4 mixed-v3",
		Kind:               "image",
		Vendor:             "Ideogram",
		ModelVersionRef:    "ideogram-4-mixed-v3@bbee2ab2",
		RuntimeOfferingID:  "ideogram-4-mixed-v3",
		Description:        "Ideogram 4 图片生成，仅限团队内部非商业评测使用，不提供商用承诺。",
		RequiredParameters: []string{"prompt", "width", "height", "seed", "steps"},
		Pricing: WildFlowCatalogPricing{
			Currency: "CNY", Amount: 0, Unit: "team_trial", Display: "团队内部非商业评测 · 暂不扣零售余额",
		},
	},
	{
		ID:                 WildFlowModelInternalASR,
		DisplayName:        "VibeVoice + Faster Whisper 内部 ASR",
		Kind:               "asr",
		Vendor:             "WildFlow",
		ModelVersionRef:    WildFlowModelInternalASR,
		RuntimeOfferingID:  "internal-vibevoice-faster-whisper-asr",
		Visibility:         "company_internal_authenticated",
		Description:        "公司内部使用的复合 ASR Runtime，同时输出分段转写与逐词时间戳。",
		RequiredParameters: []string{"input_artifact_ids"},
		Pricing: WildFlowCatalogPricing{
			Currency: "CNY", Amount: 0, Unit: "team_trial", Display: "公司内部使用 · 不对外定价",
		},
	},
}

func ListCanonicalWildFlowOfferings() []WildFlowOffering {
	offerings := make([]WildFlowOffering, len(canonicalWildFlowCatalog))
	copy(offerings, canonicalWildFlowCatalog)
	return offerings
}

func GetWildFlowCatalog(ctx context.Context) []WildFlowOffering {
	catalog := ListCanonicalWildFlowOfferings()
	for index := range catalog {
		catalog[index].Status = "unavailable"
	}

	runtime, err := fetchWildFlowRuntimeCatalog(ctx)
	if err != nil {
		return catalog
	}
	availability := make(map[string]bool, len(runtime))
	for _, item := range runtime {
		availability[item.ID+"\x00"+item.ModelVersionRef] = item.Callable
	}
	for index := range catalog {
		runtimeOfferingID := catalog[index].RuntimeOfferingID
		if runtimeOfferingID == "" {
			runtimeOfferingID = catalog[index].ID
		}
		key := runtimeOfferingID + "\x00" + catalog[index].ModelVersionRef
		catalog[index].Callable = availability[key]
		if catalog[index].Callable {
			catalog[index].Status = "available"
		}
	}
	return catalog
}

func GetPublicWildFlowCatalog(ctx context.Context) []WildFlowOffering {
	catalog := GetWildFlowCatalog(ctx)
	public := make([]WildFlowOffering, 0, len(catalog))
	for _, offering := range catalog {
		if offering.Visibility == "company_internal_authenticated" {
			continue
		}
		public = append(public, offering)
	}
	return public
}

func IsWildFlowOfferingCallable(ctx context.Context, modelID string) bool {
	for _, offering := range GetWildFlowCatalog(ctx) {
		if offering.ID == modelID {
			return offering.Callable
		}
	}
	return false
}

func fetchWildFlowRuntimeCatalog(ctx context.Context) ([]wildFlowRuntimeOffering, error) {
	baseURL := strings.TrimSpace(os.Getenv("WILDFLOW_INFERENCE_URL"))
	token := strings.TrimSpace(os.Getenv("WILDFLOW_INTERNAL_TOKEN"))
	if baseURL == "" || token == "" {
		return nil, errors.New("inference catalog is not configured")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("invalid inference URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("invalid inference URL components")
	}
	parsed.Path = "/internal/v1/catalog"

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	response, err := wildFlowCatalogHTTPClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("inference catalog unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, wildFlowCatalogResponseLimit+1))
	if err != nil || len(body) > wildFlowCatalogResponseLimit {
		return nil, errors.New("invalid inference catalog response")
	}
	var payload struct {
		Data []wildFlowRuntimeOffering `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	return payload.Data, nil
}
