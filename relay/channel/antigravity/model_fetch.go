package antigravity

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/service"
)

// AntigravityModel represents a model from Antigravity API
type AntigravityModel struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Description string `json:"description,omitempty"`
	Version     string `json:"version,omitempty"`
}

// AntigravityModelsResponse represents the response from fetchAvailableModels endpoint
type AntigravityModelsResponse struct {
	Models []AntigravityModel `json:"models"`
}

// FetchAntigravityModels fetches available models from Antigravity API
// It tries multiple endpoints: daily, sandbox, production
func FetchAntigravityModels(baseURL, oauthKeyStr, proxyURL string) ([]string, error) {
	// Parse OAuth key to get access token and project ID
	oauthKey, err := ParseOAuthKey(oauthKeyStr)
	if err != nil {
		return nil, fmt.Errorf("解析OAuth key失败: %v", err)
	}

	// Refresh token if needed
	if oauthKey.IsExpired() && oauthKey.RefreshToken != "" {
		newAccessToken, newExpiredAt, refreshErr := oauthKey.RefreshAccessToken()
		if refreshErr != nil {
			common.SysError("antigravity: failed to refresh token when fetching models: " + refreshErr.Error())
		} else {
			oauthKey.AccessToken = newAccessToken
			oauthKey.ExpiredAt = newExpiredAt
		}
	}

	accessToken := strings.TrimSpace(oauthKey.AccessToken)
	if accessToken == "" {
		return nil, fmt.Errorf("access_token is required")
	}

	projectID := oauthKey.ProjectID
	if projectID == "" {
		return nil, fmt.Errorf("project_id is required")
	}

	// Try multiple endpoints in order: daily -> sandbox -> production
	endpoints := []string{
		"https://daily-cloudcode-pa.googleapis.com",
		"https://daily-cloudcode-pa.sandbox.googleapis.com",
		"https://cloudcode-pa.googleapis.com",
	}

	// Use provided baseURL if it's not empty, otherwise use default endpoints
	if baseURL != "" && baseURL != "https://cloudcode-pa.googleapis.com" {
		endpoints = append([]string{baseURL}, endpoints...)
	}

	var lastErr error
	for _, endpoint := range endpoints {
		models, err := fetchModelsFromEndpoint(endpoint, accessToken, projectID, proxyURL)
		if err == nil {
			return models, nil
		}
		lastErr = err
		common.SysError(fmt.Sprintf("antigravity: failed to fetch models from %s: %v", endpoint, err))
	}

	return nil, fmt.Errorf("所有端点都失败，最后一个错误: %v", lastErr)
}

func fetchModelsFromEndpoint(baseURL, accessToken, projectID, proxyURL string) ([]string, error) {
	client, err := service.GetHttpClientWithProxy(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("创建HTTP客户端失败: %v", err)
	}

	// Build request URL: /v1internal:fetchAvailableModels
	url := fmt.Sprintf("%s/v1internal:fetchAvailableModels", baseURL)

	// Build request body
	requestBody := map[string]interface{}{
		"project": projectID,
	}

	jsonBody, err := common.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("编码请求体失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(string(jsonBody)))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %v", err)
	}

	// Set headers (use antigravity User-Agent to avoid 404)
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "antigravity/1.104.0 darwin/arm64")

	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		return nil, fmt.Errorf("服务器返回错误 %d: %s", response.StatusCode, string(body))
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %v", err)
	}

	var modelsResponse AntigravityModelsResponse
	if err = common.Unmarshal(body, &modelsResponse); err != nil {
		return nil, fmt.Errorf("解析响应失败: %v", err)
	}

	// Extract model names
	models := make([]string, 0, len(modelsResponse.Models))
	for _, model := range modelsResponse.Models {
		if model.Name != "" {
			// Skip internal/experimental models
			if shouldSkipModel(model.Name) {
				continue
			}
			models = append(models, model.Name)
		}
	}

	return models, nil
}

// shouldSkipModel returns true if the model should be skipped
// Based on CLIProxyAPI implementation + known non-working models
func shouldSkipModel(modelName string) bool {
	skipModels := []string{
		"chat_20706",
		"chat_23310",
		"tab_flash_lite_preview",
		"tab_jump_flash_lite_preview",
		"gemini-2.5-flash-thinking",
		"gemini-2.5-pro",
		"gemini-2.5-flash",       // 429 quota exhausted
		"gemini-2.5-flash-lite",  // 429 quota exhausted
	}

	for _, skip := range skipModels {
		if modelName == skip {
			return true
		}
	}
	return false
}
