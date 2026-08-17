package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetStatusNormalizesLegacyPublicBrandDefaults(t *testing.T) {
	previousSystemName := common.SystemName
	previousLogo := common.Logo
	previousOptionMap := common.OptionMap
	settings := operation_setting.GetGeneralSetting()
	previousDocsLink := settings.DocsLink
	t.Cleanup(func() {
		common.SystemName = previousSystemName
		common.Logo = previousLogo
		common.OptionMap = previousOptionMap
		settings.DocsLink = previousDocsLink
	})

	common.SystemName = "New API"
	common.Logo = ""
	common.OptionMap = map[string]string{}
	settings.DocsLink = "https://docs.newapi.pro"

	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/status", nil)

	GetStatus(context)

	var payload struct {
		Success bool           `json:"success"`
		Data    map[string]any `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
	require.True(t, payload.Success)
	assert.Equal(t, "野生流动", payload.Data["system_name"])
	assert.Equal(t, "/logo.png", payload.Data["logo"])
	assert.Equal(
		t,
		"https://github.com/wildsyn/wildflow/tree/main/docs",
		payload.Data["docs_link"],
	)
	assert.Equal(t, false, payload.Data["enable_drawing"])
	assert.Equal(t, false, payload.Data["enable_task"])
	assert.Equal(t, "野生流动", buildWaffoTopUpGoodsInfo(100).AppName)
}

func TestGetStatusPreservesExplicitPublicBrandOverrides(t *testing.T) {
	previousSystemName := common.SystemName
	previousLogo := common.Logo
	previousOptionMap := common.OptionMap
	settings := operation_setting.GetGeneralSetting()
	previousDocsLink := settings.DocsLink
	t.Cleanup(func() {
		common.SystemName = previousSystemName
		common.Logo = previousLogo
		common.OptionMap = previousOptionMap
		settings.DocsLink = previousDocsLink
	})

	common.SystemName = "WildFlow Preview"
	common.Logo = "https://assets.example.invalid/wildflow.svg"
	common.OptionMap = map[string]string{}
	settings.DocsLink = "https://docs.example.invalid/wildflow"

	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/status", nil)

	GetStatus(context)

	var payload struct {
		Success bool           `json:"success"`
		Data    map[string]any `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
	require.True(t, payload.Success)
	assert.Equal(t, "WildFlow Preview", payload.Data["system_name"])
	assert.Equal(
		t,
		"https://assets.example.invalid/wildflow.svg",
		payload.Data["logo"],
	)
	assert.Equal(
		t,
		"https://docs.example.invalid/wildflow",
		payload.Data["docs_link"],
	)
}
