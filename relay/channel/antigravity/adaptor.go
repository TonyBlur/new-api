package antigravity

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/samber/lo"
)

type Adaptor struct {
	oauthKey   *OAuthKey
	forceStream bool // set to true for gpt-oss non-streaming requests (upstream bug workaround)
}

func (a *Adaptor) ConvertGeminiRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeminiChatRequest) (any, error) {
	return nil, errors.New("antigravity channel: endpoint not supported")
}

func (a *Adaptor) ConvertClaudeRequest(*gin.Context, *relaycommon.RelayInfo, *dto.ClaudeRequest) (any, error) {
	return nil, errors.New("antigravity channel: /v1/messages endpoint not supported")
}

func (a *Adaptor) ConvertAudioRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.AudioRequest) (io.Reader, error) {
	return nil, errors.New("antigravity channel: endpoint not supported")
}

func (a *Adaptor) ConvertImageRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.ImageRequest) (any, error) {
	return nil, errors.New("antigravity channel: endpoint not supported")
}

func (a *Adaptor) Init(info *relaycommon.RelayInfo) {
}

func (a *Adaptor) ConvertOpenAIRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeneralOpenAIRequest) (any, error) {
	// Parse OAuth key from info.ApiKey
	key := strings.TrimSpace(info.ApiKey)
	if !strings.HasPrefix(key, "{") {
		return nil, errors.New("antigravity channel: key must be a JSON object")
	}

	oauthKey, err := ParseOAuthKey(key)
	if err != nil {
		return nil, err
	}

	if oauthKey.ProjectID == "" {
		return nil, errors.New("antigravity channel: project_id is required in oauth key")
	}

	// Store oauthKey for later use in GetRequestURL
	a.oauthKey = oauthKey

	// Resolve model alias and clean model name
	model := ResolveModelAlias(request.Model)

	// Workaround: gpt-oss models fail on non-streaming requests because the upstream
	// Antigravity proxy incorrectly adds stream_options when forwarding to the OpenAI backend.
	// Force streaming for gpt-oss models to avoid the 400 error.
	if strings.HasPrefix(model, "gpt-oss") && !lo.FromPtrOr(request.Stream, false) {
		a.forceStream = true
	}

	// Convert OpenAI messages to Antigravity contents
	contents := convertMessagesToContents(request.Messages)

	// Filter out thought/thoughtSignature parts from contents (upstream rejects them)
	// Also remove empty content entries after filtering (causes Gemini 3 Flash 400 errors)
	// Reference: OmniRoute antigravity.ts transformRequest
	contents = filterAntigravityContents(contents)

	// Convert OpenAI tools to Antigravity format
	var tools json.RawMessage
	if len(request.Tools) > 0 {
		toolsBytes, marshalErr := common.Marshal(convertToolsToAntigravityFormat(request.Tools))
		if marshalErr == nil {
			tools = toolsBytes
		}
	}

	// Build tool config if tools are present
	var toolConfig *AntigravityToolConfig
	if len(request.Tools) > 0 {
		toolConfig = &AntigravityToolConfig{
			FunctionCallingConfig: &AntigravityFunctionCallingConfig{
				Mode: "AUTO",
			},
		}
	}

	// Build the Antigravity request
	antigravityReq := AntigravityRequest{
		Project:     oauthKey.ProjectID,
		Model:       model,
		UserAgent:   "antigravity",
		RequestType: "agent",
		RequestID:   "agent-" + uuid.New().String(),
		Request: AntigravityInnerRequest{
			Contents:        contents,
			SessionID:       uuid.New().String(),
			Tools:           tools,
			ToolConfig:      toolConfig,
			Temperature:     request.Temperature,
			MaxOutputTokens: request.MaxCompletionTokens,
			TopP:            request.TopP,
		},
	}

	return antigravityReq, nil
}

// convertMessagesToContents converts OpenAI chat messages to Antigravity contents format
func convertMessagesToContents(messages []dto.Message) []AntigravityContent {
	contents := make([]AntigravityContent, 0, len(messages))

	for _, msg := range messages {
		role := msg.Role
		// Map OpenAI roles to Antigravity roles
		switch role {
		case "system":
			role = "user" // Antigravity doesn't have system role, use user
		case "assistant":
			role = "model" // Antigravity uses "model" for assistant
		case "tool":
			role = "user" // Tool responses go as user in Antigravity
		}

		content := AntigravityContent{
			Role: role,
		}

		// Handle string content
		if msg.Content != nil {
			if contentStr, ok := msg.Content.(string); ok && contentStr != "" {
				content.Parts = append(content.Parts, AntigravityPart{Text: contentStr})
			} else if contentArray, ok := msg.Content.([]interface{}); ok {
				// Handle array content (multimodal)
				for _, item := range contentArray {
					if itemMap, ok := item.(map[string]interface{}); ok {
						if text, ok := itemMap["text"].(string); ok {
							content.Parts = append(content.Parts, AntigravityPart{Text: text})
						}
					}
				}
			}
		}

		// Handle tool calls from assistant messages
		parsedToolCalls := msg.ParseToolCalls()
		if len(parsedToolCalls) > 0 {
			for _, tc := range parsedToolCalls {
				var args json.RawMessage
				if tc.Function.Arguments != "" {
					args = json.RawMessage(tc.Function.Arguments)
				}
				content.Parts = append(content.Parts, AntigravityPart{
					FunctionCall: &AntigravityFunctionCall{
						Name: tc.Function.Name,
						Args: args,
					},
				})
			}
		}

		// Handle tool response messages
		if msg.ToolCallId != "" && msg.Content != nil {
			if contentStr, ok := msg.Content.(string); ok {
				// Wrap tool response as function response
				responseJSON, _ := common.Marshal(map[string]string{"result": contentStr})
				funcName := ""
				if msg.Name != nil {
					funcName = *msg.Name
				}
				content.Parts = append(content.Parts, AntigravityPart{
					FunctionResponse: &AntigravityFunctionResponse{
						Name:     funcName,
						Response: responseJSON,
					},
				})
			}
		}

		if len(content.Parts) > 0 {
			contents = append(contents, content)
		}
	}

	return contents
}

// convertToolsToAntigravityFormat converts OpenAI tools to Antigravity function declarations format
func convertToolsToAntigravityFormat(tools []dto.ToolCallRequest) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(tools))

	for _, tool := range tools {
		if tool.Type == "function" {
			funcDecl := map[string]interface{}{
				"name":        tool.Function.Name,
				"description": tool.Function.Description,
			}
			if tool.Function.Parameters != nil {
				funcDecl["parameters"] = tool.Function.Parameters
			}
			result = append(result, funcDecl)
		}
	}

	return result
}

func (a *Adaptor) ConvertRerankRequest(c *gin.Context, relayMode int, request dto.RerankRequest) (any, error) {
	return nil, errors.New("antigravity channel: /v1/rerank endpoint not supported")
}

func (a *Adaptor) ConvertEmbeddingRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.EmbeddingRequest) (any, error) {
	return nil, errors.New("antigravity channel: /v1/embeddings endpoint not supported")
}

// AntigravityRequest represents the request format for Antigravity API
// Based on OmniRoute implementation: https://github.com/xuantungle/omni-route-proxy-hub
type AntigravityRequest struct {
	Project     string                   `json:"project"`
	Model       string                   `json:"model"`
	UserAgent   string                   `json:"userAgent"`
	RequestType string                   `json:"requestType"`
	RequestID   string                   `json:"requestId"`
	Request     AntigravityInnerRequest  `json:"request"`
}

type AntigravityInnerRequest struct {
	Contents      []AntigravityContent `json:"contents,omitempty"`
	SessionID     string               `json:"sessionId,omitempty"`
	Tools         json.RawMessage      `json:"tools,omitempty"`
	ToolConfig    *AntigravityToolConfig `json:"toolConfig,omitempty"`
	Temperature   *float64             `json:"temperature,omitempty"`
	MaxOutputTokens *uint              `json:"maxOutputTokens,omitempty"`
	TopP          *float64             `json:"topP,omitempty"`
}

type AntigravityContent struct {
	Role  string                `json:"role"`
	Parts []AntigravityPart     `json:"parts"`
}

type AntigravityPart struct {
	Text             string                    `json:"text,omitempty"`
	Thought          bool                      `json:"thought,omitempty"`
	ThoughtSignature json.RawMessage           `json:"thoughtSignature,omitempty"`
	FunctionCall     *AntigravityFunctionCall  `json:"functionCall,omitempty"`
	FunctionResponse *AntigravityFunctionResponse `json:"functionResponse,omitempty"`
}

type AntigravityFunctionCall struct {
	Name string `json:"name"`
	Args json.RawMessage `json:"args"`
}

type AntigravityFunctionResponse struct {
	Name   string `json:"name"`
	Response json.RawMessage `json:"response"`
}

type AntigravityToolConfig struct {
	FunctionCallingConfig *AntigravityFunctionCallingConfig `json:"functionCallingConfig,omitempty"`
}

type AntigravityFunctionCallingConfig struct {
	Mode string `json:"mode"` // "VALIDATED", "AUTO", "ANY"
}

func (a *Adaptor) ConvertOpenAIResponsesRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.OpenAIResponsesRequest) (any, error) {
	// Parse OAuth key from info.ApiKey (SetupRequestHeader hasn't been called yet)
	key := strings.TrimSpace(info.ApiKey)
	if !strings.HasPrefix(key, "{") {
		return nil, errors.New("antigravity channel: key must be a JSON object")
	}

	oauthKey, err := ParseOAuthKey(key)
	if err != nil {
		return nil, err
	}

	if oauthKey.ProjectID == "" {
		return nil, errors.New("antigravity channel: project_id is required in oauth key")
	}

	// Store oauthKey for later use in GetRequestURL
	a.oauthKey = oauthKey

	// Resolve model alias and clean model name
	model := ResolveModelAlias(request.Model)

	// Workaround: gpt-oss models fail on non-streaming requests because the upstream
	// Antigravity proxy incorrectly adds stream_options when forwarding to the OpenAI backend.
	// Force streaming for gpt-oss models to avoid the 400 error.
	if strings.HasPrefix(model, "gpt-oss") && !lo.FromPtrOr(request.Stream, false) {
		a.forceStream = true
	}

	// Convert input to contents
	var contents []AntigravityContent
	if request.Input != nil {
		contents = convertInputToContents(request.Input)
	}

	// Filter out thought/thoughtSignature parts from contents (upstream rejects them)
	// Also remove empty content entries after filtering (causes Gemini 3 Flash 400 errors)
	// Reference: OmniRoute antigravity.ts transformRequest
	contents = filterAntigravityContents(contents)

	// Build tool config if tools are present
	var toolConfig *AntigravityToolConfig
	if request.Tools != nil && len(request.Tools) > 0 {
		toolConfig = &AntigravityToolConfig{
			FunctionCallingConfig: &AntigravityFunctionCallingConfig{
				Mode: "VALIDATED",
			},
		}
	}

	// Build the Antigravity request
	antigravityReq := AntigravityRequest{
		Project:     oauthKey.ProjectID,
		Model:       model,
		UserAgent:   "antigravity",
		RequestType: "agent",
		RequestID:   "agent-" + uuid.New().String(),
		Request: AntigravityInnerRequest{
			Contents:        contents,
			SessionID:       uuid.New().String(),
			Tools:           request.Tools,
			ToolConfig:      toolConfig,
			Temperature:     request.Temperature,
			MaxOutputTokens: request.MaxOutputTokens,
			TopP:            request.TopP,
		},
	}

	return antigravityReq, nil
}

// convertInputToContents converts OpenAI Responses API input format to Antigravity contents format
func convertInputToContents(input json.RawMessage) []AntigravityContent {
	var contents []AntigravityContent

	// Try to parse as array first
	var inputArray []map[string]interface{}
	if err := common.Unmarshal(input, &inputArray); err == nil {
		for _, item := range inputArray {
			content := convertInputItemToContent(item)
			if content != nil {
				contents = append(contents, *content)
			}
		}
		return contents
	}

	// Try to parse as single string
	var inputStr string
	if err := common.Unmarshal(input, &inputStr); err == nil {
		contents = append(contents, AntigravityContent{
			Role: "user",
			Parts: []AntigravityPart{
				{Text: inputStr},
			},
		})
		return contents
	}

	// Try to parse as single object
	var inputObj map[string]interface{}
	if err := common.Unmarshal(input, &inputObj); err == nil {
		content := convertInputItemToContent(inputObj)
		if content != nil {
			contents = append(contents, *content)
		}
	}

	return contents
}

func convertInputItemToContent(item map[string]interface{}) *AntigravityContent {
	role, _ := item["role"].(string)
	if role == "" {
		role = "user"
	}
	// Map OpenAI roles to Antigravity/Gemini roles (same as chat path)
	switch role {
	case "system":
		role = "user"
	case "assistant":
		role = "model"
	case "tool":
		role = "user"
	}

	content := &AntigravityContent{
		Role: role,
	}

	// Handle content field
	if contentVal, ok := item["content"]; ok {
		switch v := contentVal.(type) {
		case string:
			content.Parts = append(content.Parts, AntigravityPart{Text: v})
		case []interface{}:
			for _, part := range v {
				if partMap, ok := part.(map[string]interface{}); ok {
					if text, ok := partMap["text"].(string); ok {
						content.Parts = append(content.Parts, AntigravityPart{Text: text})
					}
				}
			}
		}
	}

	// Handle function calls
	if toolCalls, ok := item["tool_calls"].([]interface{}); ok {
		for _, tc := range toolCalls {
			if tcMap, ok := tc.(map[string]interface{}); ok {
				function, ok := tcMap["function"].(map[string]interface{})
				if !ok {
					continue
				}
				name, _ := function["name"].(string)
				args, _ := common.Marshal(function["arguments"])
				content.Parts = append(content.Parts, AntigravityPart{
					FunctionCall: &AntigravityFunctionCall{
						Name: name,
						Args: args,
					},
				})
			}
		}
	}

	return content
}

func (a *Adaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (any, error) {
	return channel.DoApiRequest(a, c, info, requestBody)
}

func (a *Adaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (usage any, err *types.NewAPIError) {
	if info.RelayMode == relayconstant.RelayModeChatCompletions {
		// Antigravity returns Gemini-format responses wrapped in {"response": {...}}
		// We need to unwrap before using Gemini chat handlers
		//
		// Workaround for gpt-oss: forceStream is set when the original request is non-streaming
		// but we forced the upstream request to be streaming (to avoid upstream bug).
		// In this case, we need to collect the streaming response and merge it into a
		// non-streaming response for the client.
		// Note: info.IsStream may have been updated by the upstream Content-Type header,
		// so we check forceStream first.
		if a.forceStream {
			return AntigravityStreamToChatHandler(c, info, resp)
		}
		if info.IsStream {
			return AntigravityStreamHandler(c, info, resp)
		}
		return AntigravityChatHandler(c, info, resp)
	}

	if info.RelayMode != relayconstant.RelayModeResponses && info.RelayMode != relayconstant.RelayModeResponsesCompact {
		return nil, types.NewError(errors.New("antigravity channel: endpoint not supported"), types.ErrorCodeInvalidRequest)
	}

	// Antigravity returns Gemini-format responses, but Responses API expects OpenAI Responses format.
	// We need to unwrap the Antigravity wrapper, parse Gemini format, and convert to OpenAI Responses format.
	//
	// Workaround for gpt-oss non-streaming: forceStream was set to force the upstream
	// request to use streaming endpoint. Collect SSE stream and merge into a single Gemini response.
	if a.forceStream {
		return AntigravityStreamToResponsesHandler(c, info, resp)
	}

	if info.IsStream {
		return AntigravityResponsesStreamHandler(c, info, resp)
	}
	return AntigravityResponsesHandler(c, info, resp)
}

func (a *Adaptor) GetModelList() []string {
	return ModelList
}

func (a *Adaptor) GetChannelName() string {
	return ChannelName
}

func (a *Adaptor) GetRequestURL(info *relaycommon.RelayInfo) (string, error) {
	if info.RelayMode != relayconstant.RelayModeResponses && info.RelayMode != relayconstant.RelayModeResponsesCompact && info.RelayMode != relayconstant.RelayModeChatCompletions {
		return "", errors.New("antigravity channel: only /v1/responses, /v1/responses/compact and /v1/chat/completions are supported")
	}

	// Antigravity API endpoint (internal API)
	// Format based on OmniRoute implementation:
	// - Non-streaming: /v1internal:generateContent
	// - Streaming: /v1internal:streamGenerateContent?alt=sse
	// Reference: https://github.com/xuantungle/omni-route-proxy-hub/blob/main/open-sse/executors/antigravity.ts
	
	var path string
	if info.IsStream || a.forceStream {
		path = "/v1internal:streamGenerateContent?alt=sse"
	} else {
		path = "/v1internal:generateContent"
	}

	url := relaycommon.GetFullRequestURL(info.ChannelBaseUrl, path, info.ChannelType)
	return url, nil
}

func (a *Adaptor) SetupRequestHeader(c *gin.Context, req *http.Header, info *relaycommon.RelayInfo) error {
	channel.SetupApiRequestHeader(info, c, req)

	key := strings.TrimSpace(info.ApiKey)
	if !strings.HasPrefix(key, "{") {
		return errors.New("antigravity channel: key must be a JSON object")
	}

	oauthKey, err := ParseOAuthKey(key)
	if err != nil {
		return err
	}

	// Check if token needs refresh (lazy refresh)
	if oauthKey.IsExpired() && oauthKey.RefreshToken != "" {
		newAccessToken, newExpiredAt, refreshErr := oauthKey.RefreshAccessToken()
		if refreshErr != nil {
			// Log the error but don't fail - let the request go through and fail naturally
			common.SysError("antigravity channel: failed to refresh token: " + refreshErr.Error())
		} else {
			oauthKey.AccessToken = newAccessToken
			oauthKey.ExpiredAt = newExpiredAt
			common.SysLog("antigravity channel: token refreshed successfully, new expired_at: " + fmt.Sprintf("%d", newExpiredAt))
		}
	}

	accessToken := strings.TrimSpace(oauthKey.AccessToken)
	if accessToken == "" {
		return errors.New("antigravity channel: access_token is required")
	}

	// Store oauthKey for later use in GetRequestURL
	a.oauthKey = oauthKey

	// Set Authorization header
	req.Set("Authorization", "Bearer "+accessToken)

	// Antigravity-specific headers to mimic Antigravity client
	// These headers make the request appear to come from Antigravity VS Code extension
	if req.Get("User-Agent") == "" {
		req.Set("User-Agent", "antigravity/1.104.0 darwin/arm64")
	}
	if req.Get("X-Goog-Api-Client") == "" {
		req.Set("X-Goog-Api-Client", "gdcl/9.15.1 gl-node/20.18.2")
	}
	if req.Get("Client-Metadata") == "" {
		req.Set("Client-Metadata", "mode=proactive,source=cloudcode-vscode,extension_version=2.29.0,vscode_version=1.98.2,environment=vscode_cloudshelleditor")
	}
	if req.Get("x-request-source") == "" {
		req.Set("x-request-source", "local")
	}

	// Content-Type handling
	req.Set("Content-Type", "application/json")
	if info.IsStream || a.forceStream {
		req.Set("Accept", "text/event-stream")
	} else if req.Get("Accept") == "" {
		req.Set("Accept", "application/json")
	}

	return nil
}

// filterAntigravityContents filters out thought/thoughtSignature parts and removes empty content entries.
// Reference: OmniRoute antigravity.ts transformRequest - upstream rejects thought parts without valid signatures,
// and empty parts arrays cause Gemini 3 Flash to return 400 errors.
func filterAntigravityContents(contents []AntigravityContent) []AntigravityContent {
	filtered := make([]AntigravityContent, 0, len(contents))
	for _, c := range contents {
		role := c.Role
		// functionResponse entries must have role "user" (Claude models require this)
		for _, p := range c.Parts {
			if p.FunctionResponse != nil {
				role = "user"
				break
			}
		}

		// Filter out thought and thoughtSignature parts
		parts := make([]AntigravityPart, 0, len(c.Parts))
		for _, p := range c.Parts {
			if p.Thought || p.ThoughtSignature != nil {
				continue
			}
			parts = append(parts, p)
		}

		// Skip entries with empty parts (causes Gemini 3 Flash 400 errors)
		if len(parts) == 0 {
			continue
		}

		filtered = append(filtered, AntigravityContent{
			Role:  role,
			Parts: parts,
		})
	}
	return filtered
}
