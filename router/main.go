package router

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/gin-gonic/gin"
)

func SetRouter(router *gin.Engine, _ WebAssets) {
	SetApiRouter(router)
	SetDashboardRouter(router)
	SetRelayRouter(router)
	SetVideoRouter(router)
	frontendBaseUrl := os.Getenv("FRONTEND_BASE_URL")
	if common.IsMasterNode && frontendBaseUrl != "" {
		frontendBaseUrl = ""
		common.SysLog("FRONTEND_BASE_URL is ignored on master node")
	}
	if frontendBaseUrl == "" {
		router.GET("/", middleware.RouteTag("api"), func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{
				"service":       "WildFlow API",
				"api_base":      "/v1",
				"api_version":   "v1",
				"documentation": operation_setting.WildFlowDocsLink,
				"website":       "https://wildflow.cn",
			})
		})
		router.NoRoute(func(c *gin.Context) {
			c.Set(middleware.RouteTagKey, "web")
			c.JSON(http.StatusNotFound, gin.H{
				"error": "not found",
				"hint":  "frontend is deployed separately as wildflow-web",
			})
		})
		return
	}
	if frontendBaseUrl != "" {
		frontendBaseUrl = strings.TrimSuffix(frontendBaseUrl, "/")
		router.NoRoute(func(c *gin.Context) {
			c.Set(middleware.RouteTagKey, "web")
			c.Redirect(http.StatusMovedPermanently, fmt.Sprintf("%s%s", frontendBaseUrl, c.Request.RequestURI))
		})
		return
	}
}
