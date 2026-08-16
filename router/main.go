package router

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"

	"github.com/gin-gonic/gin"
)

func SetRouter(router *gin.Engine, assets WebAssets) {
	SetApiRouter(router)
	SetDashboardRouter(router)
	SetRelayRouter(router)
	SetVideoRouter(router)
	frontendBaseUrl := os.Getenv("FRONTEND_BASE_URL")
	serveEmbeddedFrontend := os.Getenv("SERVE_EMBEDDED_FRONTEND") == "true"
	if common.IsMasterNode && frontendBaseUrl != "" {
		frontendBaseUrl = ""
		common.SysLog("FRONTEND_BASE_URL is ignored on master node")
	}
	if frontendBaseUrl == "" && !serveEmbeddedFrontend {
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
	SetWebRouter(router, assets)
}
