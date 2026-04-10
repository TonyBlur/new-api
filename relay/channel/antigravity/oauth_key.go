package antigravity

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// Environment variable names for OAuth credentials (following OmniRoute convention)
const (
	EnvAntigravityClientID     = "ANTIGRAVITY_OAUTH_CLIENT_ID"
	EnvAntigravityClientSecret = "ANTIGRAVITY_OAUTH_CLIENT_SECRET"
)

// Hardcoded OAuth credentials from CLIProxyAPI
// Source: https://github.com/router-for-me/CLIProxyAPI/blob/main/internal/auth/antigravity/constants.go
const (
	DefaultAntigravityClientID     = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
	DefaultAntigravityClientSecret = "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"
)

// OAuthKey stores Antigravity OAuth credentials
type OAuthKey struct {
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ProjectID    string `json:"project_id,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	Email        string `json:"email,omitempty"`
	ExpiredAt    int64  `json:"expired_at,omitempty"` // Unix timestamp in seconds
}

// ParseOAuthKey parses the raw JSON key string
func ParseOAuthKey(raw string) (*OAuthKey, error) {
	if raw == "" {
		return nil, errors.New("antigravity channel: empty oauth key")
	}
	var key OAuthKey
	if err := common.Unmarshal([]byte(raw), &key); err != nil {
		return nil, errors.New("antigravity channel: invalid oauth key json")
	}
	return &key, nil
}

// IsExpired checks if the access token is expired or about to expire (5 min buffer)
func (k *OAuthKey) IsExpired() bool {
	if k.ExpiredAt == 0 {
		return true
	}
	// 5 minute buffer before actual expiration
	return time.Now().Unix()+300 >= k.ExpiredAt
}

// RefreshTokenResponse represents Google OAuth2 token refresh response
type RefreshTokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

// getClientCredentials returns client_id and client_secret
// Priority: 1) OAuthKey, 2) Environment variables, 3) Hardcoded defaults (from CLIProxyAPI)
func (k *OAuthKey) getClientCredentials() (clientID, clientSecret string) {
	// First try from the key itself
	if k.ClientID != "" && k.ClientSecret != "" {
		return k.ClientID, k.ClientSecret
	}
	// Try environment variables (OmniRoute convention)
	clientID = os.Getenv(EnvAntigravityClientID)
	clientSecret = os.Getenv(EnvAntigravityClientSecret)
	if clientID != "" && clientSecret != "" {
		return clientID, clientSecret
	}
	// Fall back to hardcoded defaults (from CLIProxyAPI)
	return DefaultAntigravityClientID, DefaultAntigravityClientSecret
}

// RefreshAccessToken refreshes the access token using refresh_token
// Returns the new access token and expiration time
func (k *OAuthKey) RefreshAccessToken() (string, int64, error) {
	if k.RefreshToken == "" {
		return "", 0, errors.New("antigravity channel: refresh_token is required for token refresh")
	}

	clientID, clientSecret := k.getClientCredentials()
	if clientID == "" || clientSecret == "" {
		return "", 0, errors.New("antigravity channel: client_id and client_secret are required for token refresh")
	}

	// Google OAuth2 token endpoint - shared by Gemini and Antigravity
	url := "https://oauth2.googleapis.com/token"

	payload := map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": k.RefreshToken,
		"client_id":     clientID,
		"client_secret": clientSecret,
	}

	jsonPayload, err := common.Marshal(payload)
	if err != nil {
		return "", 0, err
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonPayload))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	// Use proxy-aware HTTP client (respects HTTP_PROXY/HTTPS_PROXY env vars)
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: transport}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, err
	}

	if resp.StatusCode != http.StatusOK {
		return "", 0, errors.New("antigravity channel: token refresh failed: " + string(body))
	}

	var refreshResp RefreshTokenResponse
	if err := common.Unmarshal(body, &refreshResp); err != nil {
		return "", 0, err
	}

	newExpiredAt := time.Now().Unix() + int64(refreshResp.ExpiresIn)
	return refreshResp.AccessToken, newExpiredAt, nil
}
