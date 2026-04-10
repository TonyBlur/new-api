package service

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// OmniRoute built-in Antigravity OAuth credentials
// These credentials are for localhost development only
// For production, users should register their own OAuth app at Google Cloud Console
const (
	antigravityClientID     = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
	antigravityClientSecret = "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"
)

// Antigravity OAuth endpoints
const (
	googleAuthorizeURL = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL     = "https://oauth2.googleapis.com/token"
	googleUserInfoURL  = "https://www.googleapis.com/oauth2/v1/userinfo"
)

// Antigravity API endpoints
const (
	antigravityLoadCodeAssistEndpoint = "https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist"
	antigravityOnboardUserEndpoint    = "https://cloudcode-pa.googleapis.com/v1internal:onboardUser"
)

// Antigravity headers (from OmniRoute)
const (
	antigravityUserAgent     = "google-api-nodejs-client/9.15.1"
	antigravityApiClient     = "google-cloud-sdk vscode_cloudshelleditor/0.1"
	antigravityClientMetadata = `{"ideType":"IDE_UNSPECIFIED","platform":"PLATFORM_UNSPECIFIED","pluginType":"GEMINI"}`
)

// AntigravityOAuthService handles Antigravity (Google Cloud Code) OAuth flow
type AntigravityOAuthService struct{}

// NewAntigravityOAuthService creates a new Antigravity OAuth service
func NewAntigravityOAuthService() *AntigravityOAuthService {
	return &AntigravityOAuthService{}
}

// GenerateAuthURL generates the Google OAuth authorization URL
func (s *AntigravityOAuthService) GenerateAuthURL(redirectURI, state string) string {
	params := url.Values{}
	params.Set("client_id", antigravityClientID)
	params.Set("redirect_uri", redirectURI)
	params.Set("response_type", "code")
	params.Set("scope", "https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/userinfo.profile https://www.googleapis.com/auth/cclog https://www.googleapis.com/auth/experimentsandconfigs")
	params.Set("state", state)
	params.Set("access_type", "offline")
	params.Set("prompt", "consent")

	return fmt.Sprintf("%s?%s", googleAuthorizeURL, params.Encode())
}

// ExchangeToken exchanges authorization code for access token
func (s *AntigravityOAuthService) ExchangeToken(code, redirectURI string) (*AntigravityTokenResponse, error) {
	data := url.Values{}
	data.Set("grant_type", "authorization_code")
	data.Set("client_id", antigravityClientID)
	data.Set("client_secret", antigravityClientSecret)
	data.Set("code", code)
	data.Set("redirect_uri", redirectURI)

	req, err := http.NewRequest("POST", googleTokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange failed: %s", string(body))
	}

	var tokenResp AntigravityTokenResponse
	if err := common.Unmarshal(body, &tokenResp); err != nil {
		return nil, err
	}

	return &tokenResp, nil
}

// GetUserInfo retrieves user info using access token
func (s *AntigravityOAuthService) GetUserInfo(accessToken string) (*AntigravityUserInfo, error) {
	req, err := http.NewRequest("GET", fmt.Sprintf("%s?alt=json", googleUserInfoURL), nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to get user info: %s", string(body))
	}

	var userInfo AntigravityUserInfo
	if err := common.Unmarshal(body, &userInfo); err != nil {
		return nil, err
	}

	return &userInfo, nil
}

// LoadCodeAssistResponse represents the response from loadCodeAssist endpoint
type LoadCodeAssistResponse struct {
	CloudAICompanionProject interface{} `json:"cloudaicompanionProject"`
	AllowedTiers            []struct {
		ID        string `json:"id"`
		IsDefault bool   `json:"isDefault"`
	} `json:"allowedTiers"`
}

// OnboardUserResponse represents the response from onboardUser endpoint
type OnboardUserResponse struct {
	Done     bool `json:"done"`
	Response *struct {
		CloudAICompanionProject interface{} `json:"cloudaicompanionProject"`
	} `json:"response,omitempty"`
}

// LoadCodeAssist calls the loadCodeAssist endpoint to get project info
// This mimics the behavior of OmniRoute's antigravity OAuth flow
func (s *AntigravityOAuthService) LoadCodeAssist(accessToken string) (string, string, error) {
	headers := map[string]string{
		"Authorization":     "Bearer " + accessToken,
		"Content-Type":      "application/json",
		"User-Agent":        antigravityUserAgent,
		"X-Goog-Api-Client": antigravityApiClient,
		"Client-Metadata":   antigravityClientMetadata,
	}

	metadata := map[string]string{
		"ideType":    "IDE_UNSPECIFIED",
		"platform":   "PLATFORM_UNSPECIFIED",
		"pluginType": "GEMINI",
	}

	payload := map[string]interface{}{
		"metadata": metadata,
	}

	jsonPayload, err := common.Marshal(payload)
	if err != nil {
		return "", "", err
	}

	req, err := http.NewRequest("POST", antigravityLoadCodeAssistEndpoint, bytes.NewBuffer(jsonPayload))
	if err != nil {
		return "", "", err
	}

	for key, value := range headers {
		req.Header.Set(key, value)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}

	if resp.StatusCode != http.StatusOK {
		return "", "", errors.New("loadCodeAssist failed: " + string(body))
	}

	var loadResp LoadCodeAssistResponse
	if err := common.Unmarshal(body, &loadResp); err != nil {
		return "", "", err
	}

	// Extract project ID
	projectID := ""
	if loadResp.CloudAICompanionProject != nil {
		switch v := loadResp.CloudAICompanionProject.(type) {
		case string:
			projectID = v
		case map[string]interface{}:
			if id, ok := v["id"].(string); ok {
				projectID = id
			}
		}
	}

	// Extract default tier ID
	tierID := "legacy-tier"
	for _, tier := range loadResp.AllowedTiers {
		if tier.IsDefault && tier.ID != "" {
			tierID = tier.ID
			break
		}
	}

	return projectID, tierID, nil
}

// OnboardUser calls the onboardUser endpoint to complete user registration
// This mimics the behavior of OmniRoute's antigravity OAuth flow
func (s *AntigravityOAuthService) OnboardUser(accessToken, projectID, tierID string) (string, error) {
	if projectID == "" {
		return "", errors.New("project_id is required")
	}

	headers := map[string]string{
		"Authorization":     "Bearer " + accessToken,
		"Content-Type":      "application/json",
		"User-Agent":        antigravityUserAgent,
		"X-Goog-Api-Client": antigravityApiClient,
		"Client-Metadata":   antigravityClientMetadata,
	}

	metadata := map[string]string{
		"ideType":    "IDE_UNSPECIFIED",
		"platform":   "PLATFORM_UNSPECIFIED",
		"pluginType": "GEMINI",
	}

	payload := map[string]interface{}{
		"tierId":                  tierID,
		"metadata":                metadata,
		"cloudaicompanionProject": projectID,
	}

	jsonPayload, err := common.Marshal(payload)
	if err != nil {
		return "", err
	}

	// Retry up to 10 times with 5 second delay (same as OmniRoute)
	for i := 0; i < 10; i++ {
		req, err := http.NewRequest("POST", antigravityOnboardUserEndpoint, bytes.NewBuffer(jsonPayload))
		if err != nil {
			return "", err
		}

		for key, value := range headers {
			req.Header.Set(key, value)
		}

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			common.SysLog(fmt.Sprintf("antigravity onboardUser attempt %d failed: %v", i+1, err))
			time.Sleep(5 * time.Second)
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			common.SysLog(fmt.Sprintf("antigravity onboardUser attempt %d failed to read body: %v", i+1, err))
			time.Sleep(5 * time.Second)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			common.SysLog(fmt.Sprintf("antigravity onboardUser attempt %d failed: %s", i+1, string(body)))
			time.Sleep(5 * time.Second)
			continue
		}

		var onboardResp OnboardUserResponse
		if err := common.Unmarshal(body, &onboardResp); err != nil {
			common.SysLog(fmt.Sprintf("antigravity onboardUser attempt %d failed to parse response: %v", i+1, err))
			time.Sleep(5 * time.Second)
			continue
		}

		if onboardResp.Done {
			// Extract project ID from response if available
			if onboardResp.Response != nil && onboardResp.Response.CloudAICompanionProject != nil {
				switch v := onboardResp.Response.CloudAICompanionProject.(type) {
				case string:
					return v, nil
				case map[string]interface{}:
					if id, ok := v["id"].(string); ok {
						return id, nil
					}
				}
			}
			return projectID, nil
		}

		common.SysLog(fmt.Sprintf("antigravity onboardUser attempt %d not done yet, retrying...", i+1))
		time.Sleep(5 * time.Second)
	}

	return "", errors.New("onboardUser failed after 10 retries")
}

// CompleteOAuth completes the OAuth flow and returns the OAuth key
func (s *AntigravityOAuthService) CompleteOAuth(code, redirectURI string) (*AntigravityOAuthKey, error) {
	// Exchange code for token
	tokenResp, err := s.ExchangeToken(code, redirectURI)
	if err != nil {
		return nil, fmt.Errorf("token exchange failed: %w", err)
	}

	// Get user info
	userInfo, err := s.GetUserInfo(tokenResp.AccessToken)
	if err != nil {
		common.SysLog("antigravity: failed to get user info: " + err.Error())
		// Continue without user info
		userInfo = &AntigravityUserInfo{}
	}

	// Load code assist to get project info
	projectID, tierID, err := s.LoadCodeAssist(tokenResp.AccessToken)
	if err != nil {
		common.SysLog("antigravity: failed to load code assist: " + err.Error())
		// Continue without project info - user can provide it manually
	}

	// Onboard user if we have project info
	finalProjectID := projectID
	if projectID != "" {
		onboardedProjectID, err := s.OnboardUser(tokenResp.AccessToken, projectID, tierID)
		if err != nil {
			common.SysLog("antigravity: failed to onboard user: " + err.Error())
			// Use the original project ID
		} else {
			finalProjectID = onboardedProjectID
		}
	}

	// Calculate expiration time
	expiredAt := time.Now().Unix() + int64(tokenResp.ExpiresIn)

	return &AntigravityOAuthKey{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		ProjectID:    finalProjectID,
		Email:        userInfo.Email,
		ExpiredAt:    expiredAt,
	}, nil
}

// AntigravityTokenResponse represents the OAuth token response
type AntigravityTokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// AntigravityUserInfo represents the user info response
type AntigravityUserInfo struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	VerifiedEmail bool   `json:"verified_email"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
}

// AntigravityOAuthKey represents the stored OAuth key
type AntigravityOAuthKey struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ProjectID    string `json:"project_id"`
	Email        string `json:"email"`
	ExpiredAt    int64  `json:"expired_at"`
}

// ExtractAuthCode extracts the authorization code from callback URL
func ExtractAuthCode(callbackURL string) (string, error) {
	parsedURL, err := url.Parse(callbackURL)
	if err != nil {
		return "", err
	}

	code := parsedURL.Query().Get("code")
	if code == "" {
		return "", errors.New("no authorization code found in URL")
	}

	return code, nil
}
