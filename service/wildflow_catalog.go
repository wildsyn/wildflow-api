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
	ID              string `json:"id"`
	DisplayName     string `json:"display_name"`
	Kind            string `json:"kind"`
	Vendor          string `json:"vendor"`
	ModelVersionRef string `json:"model_version_ref"`
	Profile         string `json:"profile"`
	Description     string `json:"description"`
	Callable        bool   `json:"callable"`
	Status          string `json:"status"`
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
		ID:              "tts-standard",
		DisplayName:     "VoxCPM2 标准语音合成",
		Kind:            "tts",
		Vendor:          "OpenBMB",
		ModelVersionRef: "openbmb/VoxCPM2",
		Profile:         "standard",
		Description:     "通用音色、声音设计与声音克隆。",
	},
	{
		ID:              "tts-premium",
		DisplayName:     "VoxCPM2 王立群精品语音",
		Kind:            "tts",
		Vendor:          "OpenBMB",
		ModelVersionRef: "openbmb/VoxCPM2",
		Profile:         "wangliqun-premium",
		Description:     "基于同一 VoxCPM2 底模的王立群精品音色产品。",
	},
	{
		ID:              "flux2-klein-4b",
		DisplayName:     "FLUX.2 [klein] 4B 图片生成",
		Kind:            "image",
		Vendor:          "Black Forest Labs",
		ModelVersionRef: "black-forest-labs/FLUX.2-klein-4B",
		Profile:         "default",
		Description:     "支持中英文提示词的开源图片生成模型。",
	},
}

func GetWildFlowCatalog(ctx context.Context) []WildFlowOffering {
	catalog := make([]WildFlowOffering, len(canonicalWildFlowCatalog))
	copy(catalog, canonicalWildFlowCatalog)
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
		key := catalog[index].ID + "\x00" + catalog[index].ModelVersionRef
		catalog[index].Callable = availability[key]
		if catalog[index].Callable {
			catalog[index].Status = "available"
		}
	}
	return catalog
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
