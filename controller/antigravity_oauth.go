package controller

import (
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

// AntigravityAuthURLRequest generates the OAuth authorization URL
type AntigravityAuthURLRequest struct {
	ChannelID int `json:"channel_id"`
}

// AntigravityAuthURLResponse returns the authorization URL
type AntigravityAuthURLResponse struct {
	AuthURL string `json:"authorize_url"`
}

// AntigravityExchangeRequest exchanges the authorization code for tokens
type AntigravityExchangeRequest struct {
	Code        string `json:"code"`
	RedirectURI string `json:"redirect_uri"`
}

// AntigravityExchangeResponse returns the OAuth key
type AntigravityExchangeResponse struct {
	OAuthKey string `json:"oauth_key"`
}

// GenerateAntigravityAuthURL generates the Google OAuth authorization URL
func GenerateAntigravityAuthURL(c *gin.Context) {
	var req AntigravityAuthURLRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "invalid request: " + err.Error(),
		})
		return
	}

	// Generate a random state for CSRF protection
	state := common.GetRandomString(32)

	oauthService := service.NewAntigravityOAuthService()
	// Use localhost redirect URI - user will copy the callback URL manually
	// Note: This must match the redirect_uri registered in Google Cloud Console
	redirectURI := "http://localhost:3000/oauth/callback"
	authURL := oauthService.GenerateAuthURL(redirectURI, state)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": AntigravityAuthURLResponse{
			AuthURL: authURL,
		},
	})
}

// ExchangeAntigravityToken exchanges the authorization code for access token
// and automatically calls loadCodeAssist and onboardUser to get project_id
func ExchangeAntigravityToken(c *gin.Context) {
	var req AntigravityExchangeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "invalid request: " + err.Error(),
		})
		return
	}

	// Extract code from callback URL if full URL was provided
	code := req.Code
	if strings.HasPrefix(code, "http://") || strings.HasPrefix(code, "https://") {
		extractedCode, err := service.ExtractAuthCode(code)
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "failed to extract auth code: " + err.Error(),
			})
			return
		}
		code = extractedCode
	}

	if code == "" {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "authorization code is required",
		})
		return
	}

	oauthService := service.NewAntigravityOAuthService()
	// Must match the redirect_uri used in GenerateAuthURL
	redirectURI := "http://localhost:3000/oauth/callback"

	oauthKey, err := oauthService.CompleteOAuth(code, redirectURI)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "oauth failed: " + err.Error(),
		})
		return
	}

	// Convert OAuth key to JSON string
	oauthKeyJSON, err := common.Marshal(oauthKey)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "failed to marshal oauth key: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": AntigravityExchangeResponse{
			OAuthKey: string(oauthKeyJSON),
		},
	})
}

// UpdateAntigravityChannelKey updates the channel key with the OAuth credentials
func UpdateAntigravityChannelKey(c *gin.Context) {
	type UpdateRequest struct {
		ChannelID int    `json:"channel_id"`
		OAuthKey  string `json:"oauth_key"`
	}

	var req UpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "invalid request: " + err.Error(),
		})
		return
	}

	// Verify the channel exists
	channel, err := model.GetChannelById(req.ChannelID, false)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "channel not found: " + err.Error(),
		})
		return
	}

	// Update the channel key
	channel.Key = req.OAuthKey
	err = channel.Update()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "failed to update channel key: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "channel key updated successfully",
	})
}
