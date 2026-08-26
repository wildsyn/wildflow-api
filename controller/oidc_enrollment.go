package controller

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
)

const (
	oidcEnrollmentCookieName = "wf_oidc_enroll_flow"
	oidcEnrollmentCookiePath = "/api/oauth/oidc/enroll"
)

type oidcEnrollmentEndpoints struct {
	authorization *url.URL
	invalidation  *url.URL
	enrollment    *url.URL
	callbackURL   string
	returnURL     string
	signInURL     string
}

func parseOIDCHTTPSURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, errors.New("invalid OIDC endpoint")
	}
	return parsed, nil
}

func sameOIDCOrigin(first, second *url.URL) bool {
	return strings.EqualFold(first.Scheme, second.Scheme) && strings.EqualFold(first.Host, second.Host)
}

func validatedApplicationBaseURL() (string, error) {
	application, err := parseOIDCHTTPSURL(strings.TrimRight(system_setting.ServerAddress, "/"))
	if err != nil || (application.Path != "" && application.Path != "/") || application.RawQuery != "" {
		return "", errors.New("invalid application server address")
	}
	application.Path = ""
	return strings.TrimRight(application.String(), "/"), nil
}

func validatedOIDCEnrollmentEndpoints() (*oidcEnrollmentEndpoints, error) {
	settings := system_setting.GetOIDCSettings()
	if !settings.Enabled || strings.TrimSpace(settings.ClientId) == "" || strings.TrimSpace(settings.ClientSecret) == "" {
		return nil, errors.New("OIDC is not enabled")
	}
	authorization, err := parseOIDCHTTPSURL(settings.AuthorizationEndpoint)
	if err != nil {
		return nil, err
	}
	endSession, err := parseOIDCHTTPSURL(settings.EndSessionEndpoint)
	if err != nil {
		return nil, err
	}
	enrollment, err := parseOIDCHTTPSURL(settings.EnrollmentEndpoint)
	if err != nil {
		return nil, err
	}
	if !sameOIDCOrigin(authorization, endSession) || !sameOIDCOrigin(authorization, enrollment) {
		return nil, errors.New("OIDC enrollment endpoints must share one origin")
	}
	invalidation := *authorization
	invalidation.Path = "/if/flow/default-invalidation-flow/"
	invalidation.RawPath = ""
	invalidation.RawQuery = ""
	base, err := validatedApplicationBaseURL()
	if err != nil {
		return nil, err
	}
	return &oidcEnrollmentEndpoints{
		authorization: authorization,
		invalidation:  &invalidation,
		enrollment:    enrollment,
		callbackURL:   base + "/oauth/oidc",
		returnURL:     base + "/api/oauth/oidc/enroll/start",
		signInURL:     base + "/sign-in",
	}, nil
}

func writeOIDCEnrollmentUnavailable(c *gin.Context) {
	c.JSON(http.StatusServiceUnavailable, gin.H{
		"success": false,
		"message": "统一注册暂不可用，请稍后重试",
	})
}

// BeginOIDCLogout ends the central identity-provider session after the local
// New API session has already been revoked, allowing a different account to
// authenticate instead of immediately restoring the previous SSO identity.
func BeginOIDCLogout(c *gin.Context) {
	base, err := validatedApplicationBaseURL()
	if err != nil {
		writeOIDCEnrollmentUnavailable(c)
		return
	}
	settings := system_setting.GetOIDCSettings()
	if !settings.Enabled {
		c.Redirect(http.StatusFound, base+"/sign-in")
		return
	}
	authorization, err := parseOIDCHTTPSURL(settings.AuthorizationEndpoint)
	if err != nil {
		writeOIDCEnrollmentUnavailable(c)
		return
	}
	endSession, err := parseOIDCHTTPSURL(settings.EndSessionEndpoint)
	if err != nil || !sameOIDCOrigin(authorization, endSession) {
		writeOIDCEnrollmentUnavailable(c)
		return
	}
	invalidation := *authorization
	invalidation.Path = "/if/flow/default-invalidation-flow/"
	invalidation.RawPath = ""
	query := invalidation.Query()
	query.Set("next", base+"/sign-in")
	invalidation.RawQuery = query.Encode()
	c.Redirect(http.StatusFound, invalidation.String())
}

// BeginOIDCEnrollment first ends any central Authentik session. Authentik's
// enrollment flow rejects an already-authenticated identity.
func BeginOIDCEnrollment(c *gin.Context) {
	endpoints, err := validatedOIDCEnrollmentEndpoints()
	if err != nil {
		writeOIDCEnrollmentUnavailable(c)
		return
	}
	affiliateCode := strings.TrimSpace(c.Query("aff"))
	if len(affiliateCode) > 32 {
		affiliateCode = ""
	}
	state, _, err := createOAuthFlow("oidc", model.AuthFlowIntentLogin, affiliateCode, 0, "")
	if err != nil {
		writeOIDCEnrollmentUnavailable(c)
		return
	}

	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(
		oidcEnrollmentCookieName,
		state,
		int(oauthAuthFlowTTL.Seconds()),
		oidcEnrollmentCookiePath,
		"",
		true,
		true,
	)
	logoutURL := *endpoints.invalidation
	query := logoutURL.Query()
	query.Set("next", endpoints.returnURL)
	logoutURL.RawQuery = query.Encode()
	c.Redirect(http.StatusFound, logoutURL.String())
}

// ContinueOIDCEnrollment sends the browser through Authentik enrollment and
// then directly into the ordinary one-time OAuth callback flow.
func ContinueOIDCEnrollment(c *gin.Context) {
	state, err := c.Cookie(oidcEnrollmentCookieName)
	if err != nil || strings.TrimSpace(state) == "" {
		c.JSON(http.StatusForbidden, gin.H{"success": false, "message": "统一注册链接已过期，请重新开始"})
		return
	}
	if _, err := model.GetAuthFlow(state, model.AuthFlowMatch{
		Purpose:  model.AuthFlowPurposeOAuth,
		Provider: "oidc",
		Intent:   model.AuthFlowIntentLogin,
	}); err != nil {
		c.JSON(http.StatusForbidden, gin.H{"success": false, "message": "统一注册链接已过期，请重新开始"})
		return
	}
	endpoints, err := validatedOIDCEnrollmentEndpoints()
	if err != nil {
		writeOIDCEnrollmentUnavailable(c)
		return
	}

	authorizationURL := *endpoints.authorization
	authorizationQuery := authorizationURL.Query()
	authorizationQuery.Set("client_id", system_setting.GetOIDCSettings().ClientId)
	authorizationQuery.Set("redirect_uri", endpoints.callbackURL)
	authorizationQuery.Set("response_type", "code")
	authorizationQuery.Set("scope", "openid email profile")
	authorizationQuery.Set("state", state)
	authorizationURL.RawQuery = authorizationQuery.Encode()

	enrollmentURL := *endpoints.enrollment
	enrollmentQuery := enrollmentURL.Query()
	enrollmentQuery.Set("next", authorizationURL.String())
	enrollmentURL.RawQuery = enrollmentQuery.Encode()

	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(oidcEnrollmentCookieName, "", -1, oidcEnrollmentCookiePath, "", true, true)
	c.Redirect(http.StatusFound, enrollmentURL.String())
}
