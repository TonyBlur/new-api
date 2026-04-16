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
	"github.com/samber/lo"
)

type Adaptor struct {
	oauthKey    *OAuthKey
	forceStream bool // set to true for gpt-oss non-streaming requests (upstream bug workaround)
}

func (a *Adaptor) ConvertGeminiRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeminiChatRequest) (any, error) {
	return nil, errors.New("antigravity channel: Gemini native endpoint not supported, use /v1/chat/completions, /v1/messages or /v1/responses")
}

func (a *Adaptor) ConvertClaudeRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.ClaudeRequest) (any, error) {
	common.SysLog(fmt.Sprintf("antigravity: ConvertClaudeRequest called, model=%s, RelayMode=%d, path=%s", request.Model, info.RelayMode, info.RequestURLPath))
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

	// Force streaming for gpt-oss models
	if strings.HasPrefix(model, "gpt-oss") && !lo.FromPtrOr(request.Stream, false) {
		a.forceStream = true
	}

	// Convert Claude messages to Antigravity contents
	var systemInstruction *AntigravityContent
	contents := convertClaudeMessagesToContents(request.Messages, request.System, &systemInstruction)

	// Filter out thought/thoughtSignature parts
	contents = filterAntigravityContents(contents)

	// Convert Claude tools to Antigravity format
	var antigravityTools []AntigravityTools
	if request.Tools != nil {
		funcDecls := convertClaudeToolsToAntigravityFormat(request.Tools)
		if len(funcDecls) > 0 {
			antigravityTools = []AntigravityTools{{FunctionDeclarations: funcDecls}}
		}
	}

	// Build tool config if tools are present
	var toolConfig *AntigravityToolConfig
	if len(antigravityTools) > 0 {
		toolConfig = &AntigravityToolConfig{
			FunctionCallingConfig: &AntigravityFunctionCallingConfig{
				Mode: "AUTO",
			},
		}
	}

	// Build generation config - only include if at least one field is set
	var genConfig *AntigravityGenerationConfig
	if request.Temperature != nil || request.MaxTokens != nil || request.TopP != nil {
		genConfig = &AntigravityGenerationConfig{
			Temperature:     request.Temperature,
			MaxOutputTokens: request.MaxTokens,
			TopP:            request.TopP,
		}
	}

	// Build the Antigravity request
	antigravityReq := AntigravityRequest{
		Model:   model,
		Project: oauthKey.ProjectID,
		Request: AntigravityInnerRequest{
			Contents:          contents,
			SystemInstruction: systemInstruction,
			Tools:             antigravityTools,
			ToolConfig:        toolConfig,
			GenerationConfig:  genConfig,
			SafetySettings:    defaultSafetySettings(),
		},
	}

	return antigravityReq, nil
}

// convertClaudeMessagesToContents converts Claude messages to Antigravity contents format
func convertClaudeMessagesToContents(messages []dto.ClaudeMessage, system any, systemInstruction **AntigravityContent) []AntigravityContent {
	contents := make([]AntigravityContent, 0, len(messages))

	// Handle system prompt
	var systemTexts []string
	if system != nil {
		switch s := system.(type) {
		case string:
			if s != "" {
				systemTexts = append(systemTexts, s)
			}
		case []any:
			for _, item := range s {
				if m, ok := item.(map[string]any); ok {
					if t, ok := m["text"].(string); ok && t != "" {
						systemTexts = append(systemTexts, t)
					}
				}
			}
		}
	}

	if len(systemTexts) > 0 {
		parts := make([]AntigravityPart, 0, len(systemTexts))
		for _, t := range systemTexts {
			parts = append(parts, AntigravityPart{Text: t})
		}
		*systemInstruction = &AntigravityContent{
			Role:  "user",
			Parts: parts,
		}
	}

	for _, msg := range messages {
		role := msg.Role
		// Map Claude roles to Antigravity/Gemini roles
		switch role {
		case "assistant":
			role = "model"
		}

		content := AntigravityContent{
			Role: role,
		}

		// Handle content
		if msg.Content == nil {
			continue
		}

		// String content
		if str, ok := msg.Content.(string); ok {
			if str == "" {
				continue
			}
			content.Parts = []AntigravityPart{{Text: str}}
		} else if arr, ok := msg.Content.([]any); ok {
			// Array content blocks
			parts := make([]AntigravityPart, 0, len(arr))
			for _, item := range arr {
				block, ok := item.(map[string]any)
				if !ok {
					continue
				}
				switch block["type"] {
				case "text":
					if text, ok := block["text"].(string); ok && text != "" {
						parts = append(parts, AntigravityPart{Text: text})
					}
				case "tool_use":
					// Convert Claude tool_use to Gemini functionCall
					funcCall := &AntigravityFunctionCall{}
					if name, ok := block["name"].(string); ok {
						funcCall.Name = name
					}
					if input, ok := block["input"]; ok {
						argsJSON, _ := json.Marshal(input)
						funcCall.Args = json.RawMessage(argsJSON)
					}
					parts = append(parts, AntigravityPart{
						FunctionCall: funcCall,
					})
				case "tool_result":
					// Tool results go as functionResponse
					funcResp := &AntigravityFunctionResponse{}
					if id, ok := block["tool_use_id"].(string); ok {
						funcResp.Name = id // Use tool_use_id as name lookup
					}
					// Extract response content
					if resultContent, ok := block["content"]; ok {
						switch rc := resultContent.(type) {
						case string:
							respJSON, _ := json.Marshal(map[string]any{"content": rc})
							funcResp.Response = json.RawMessage(respJSON)
						case []any:
							// Extract text from content blocks
							var texts []string
							for _, rcItem := range rc {
								if rcBlock, ok := rcItem.(map[string]any); ok {
									if rcText, ok := rcBlock["text"].(string); ok {
										texts = append(texts, rcText)
									}
								}
							}
							if len(texts) > 0 {
								respJSON, _ := json.Marshal(map[string]any{"content": strings.Join(texts, "\n")})
								funcResp.Response = json.RawMessage(respJSON)
							}
						}
					}
					parts = append(parts, AntigravityPart{
						FunctionResponse: funcResp,
					})
				case "thinking":
					// Skip thinking blocks — upstream rejects them without valid signature
					continue
				}
			}
			if len(parts) > 0 {
				content.Parts = parts
			} else {
				continue
			}
		} else {
			continue
		}

		contents = append(contents, content)
	}

	return contents
}

// convertClaudeToolsToAntigravityFormat converts Claude tools to Antigravity functionDeclarations
func convertClaudeToolsToAntigravityFormat(tools any) []map[string]interface{} {
	result := make([]map[string]interface{}, 0)

	switch t := tools.(type) {
	case []any:
		for _, tool := range t {
			toolMap, ok := tool.(map[string]any)
			if !ok {
				continue
			}
			if toolMap["type"] != "function" && toolMap["type"] != "custom" {
				continue
			}
			// For type "custom", use the tool name directly; for "function", extract from function field
			var name, description string
			var parameters map[string]any

			if fn, ok := toolMap["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
				description, _ = fn["description"].(string)
				parameters, _ = fn["parameters"].(map[string]any)
			} else {
				// Claude custom tool format
				name, _ = toolMap["name"].(string)
				description, _ = toolMap["description"].(string)
				parameters, _ = toolMap["input_schema"].(map[string]any)
			}

			if name == "" {
				continue
			}

			funcDecl := map[string]interface{}{
				"name":        name,
				"description": description,
			}
			if parameters != nil {
				funcDecl["parametersJsonSchema"] = parameters
			}
			result = append(result, funcDecl)
		}
	}

	return result
}

func (a *Adaptor) ConvertAudioRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.AudioRequest) (io.Reader, error) {
	return nil, errors.New("antigravity channel: audio endpoint not supported")
}

func (a *Adaptor) ConvertImageRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.ImageRequest) (any, error) {
	return nil, errors.New("antigravity channel: image endpoint not supported")
}

func (a *Adaptor) Init(info *relaycommon.RelayInfo) {
}

// ---- Request structs ----
// Aligned with CLIProxyAPI format:
// Template: {"model":"","request":{},"project":""}
// Reference: https://github.com/router-for-me/CLIProxyAPI/blob/main/internal/translator/antigravity/gemini/antigravity_gemini_request.go

// AntigravityRequest is the top-level request structure for Antigravity API
type AntigravityRequest struct {
	Model   string                  `json:"model"`
	Request AntigravityInnerRequest `json:"request"`
	Project string                  `json:"project"` // must be empty string, not omitted
}

// AntigravityInnerRequest is the inner request body (Gemini format)
type AntigravityInnerRequest struct {
	Contents          []AntigravityContent         `json:"contents,omitempty"`
	SystemInstruction *AntigravityContent          `json:"systemInstruction,omitempty"`
	Tools             []AntigravityTools           `json:"tools,omitempty"`
	ToolConfig        *AntigravityToolConfig       `json:"toolConfig,omitempty"`
	GenerationConfig  *AntigravityGenerationConfig `json:"generationConfig,omitempty"`
	SafetySettings    []AntigravitySafetySetting   `json:"safetySettings"`
}

// AntigravityTools wraps function declarations
// CLIProxyAPI format: [{"functionDeclarations": [...]}]
type AntigravityTools struct {
	FunctionDeclarations []map[string]interface{} `json:"functionDeclarations"`
}

// AntigravityGenerationConfig holds generation parameters
// CLIProxyAPI puts all generation params inside generationConfig sub-object
type AntigravityGenerationConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	MaxOutputTokens *uint    `json:"maxOutputTokens,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
}

// AntigravitySafetySetting represents a Gemini safety setting
// CLIProxyAPI AttachDefaultSafetySettings: all filters OFF/BLOCK_NONE
type AntigravitySafetySetting struct {
	Category  string `json:"category"`
	Threshold string `json:"threshold"`
}

// defaultSafetySettings returns the default safety settings that disable all content filtering
// Reference: CLIProxyAPI internal/translator/gemini/common/safety.go
func defaultSafetySettings() []AntigravitySafetySetting {
	return []AntigravitySafetySetting{
		{Category: "HARM_CATEGORY_HARASSMENT", Threshold: "OFF"},
		{Category: "HARM_CATEGORY_HATE_SPEECH", Threshold: "OFF"},
		{Category: "HARM_CATEGORY_SEXUALLY_EXPLICIT", Threshold: "OFF"},
		{Category: "HARM_CATEGORY_DANGEROUS_CONTENT", Threshold: "OFF"},
		{Category: "HARM_CATEGORY_CIVIC_INTEGRITY", Threshold: "BLOCK_NONE"},
	}
}

type AntigravityContent struct {
	Role  string            `json:"role"`
	Parts []AntigravityPart `json:"parts"`
}

type AntigravityPart struct {
	Text             string                      `json:"text,omitempty"`
	Thought          bool                        `json:"thought,omitempty"`
	ThoughtSignature string                      `json:"thoughtSignature,omitempty"`
	FunctionCall     *AntigravityFunctionCall    `json:"functionCall,omitempty"`
	FunctionResponse *AntigravityFunctionResponse `json:"functionResponse,omitempty"`
}

type AntigravityFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type AntigravityFunctionResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type AntigravityToolConfig struct {
	FunctionCallingConfig *AntigravityFunctionCallingConfig `json:"functionCallingConfig,omitempty"`
}

type AntigravityFunctionCallingConfig struct {
	Mode string `json:"mode"` // "VALIDATED", "AUTO", "ANY", "NONE"
}

// ---- OpenAI Chat Completions conversion ----

func (a *Adaptor) ConvertOpenAIRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeneralOpenAIRequest) (any, error) {
	common.SysLog(fmt.Sprintf("antigravity: ConvertOpenAIRequest called, model=%s, RelayMode=%d, path=%s", request.Model, info.RelayMode, info.RequestURLPath))
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
	// CLIProxyAPI: system/developer messages → systemInstruction, others → contents
	var systemInstruction *AntigravityContent
	contents := convertMessagesToContents(request.Messages, &systemInstruction)

	// Filter out thought/thoughtSignature parts from contents (upstream rejects them)
	// Also remove empty content entries after filtering (causes Gemini 3 Flash 400 errors)
	// Reference: OmniRoute antigravity.ts transformRequest
	contents = filterAntigravityContents(contents)

	// Convert OpenAI tools to Antigravity format
	// CLIProxyAPI format: tools is [{"functionDeclarations": [...]}]
	var antigravityTools []AntigravityTools
	if len(request.Tools) > 0 {
		funcDecls := convertToolsToAntigravityFormat(request.Tools)
		if len(funcDecls) > 0 {
			antigravityTools = []AntigravityTools{{FunctionDeclarations: funcDecls}}
		}
	}

	// Build tool config if tools are present
	var toolConfig *AntigravityToolConfig
	if len(antigravityTools) > 0 {
		toolConfig = &AntigravityToolConfig{
			FunctionCallingConfig: &AntigravityFunctionCallingConfig{
				Mode: "AUTO",
			},
		}
	}

	// Build generation config - only include if at least one field is set
	var genConfig *AntigravityGenerationConfig
	if request.Temperature != nil || request.MaxCompletionTokens != nil || request.TopP != nil {
		genConfig = &AntigravityGenerationConfig{
			Temperature:     request.Temperature,
			MaxOutputTokens: request.MaxCompletionTokens,
			TopP:            request.TopP,
		}
	}

	// Build the Antigravity request
	antigravityReq := AntigravityRequest{
		Model:   model,
		Project: oauthKey.ProjectID, // OmniRoute uses projectId from OAuth credentials
		Request: AntigravityInnerRequest{
			Contents:          contents,
			SystemInstruction: systemInstruction,
			Tools:             antigravityTools,
			ToolConfig:        toolConfig,
			GenerationConfig:  genConfig,
			SafetySettings:    defaultSafetySettings(),
		},
	}

	return antigravityReq, nil
}

// convertMessagesToContents converts OpenAI chat messages to Antigravity contents format
// system/developer messages are extracted into systemInstruction (per CLIProxyAPI convention)
func convertMessagesToContents(messages []dto.Message, systemInstruction **AntigravityContent) []AntigravityContent {
	contents := make([]AntigravityContent, 0, len(messages))
	var systemTexts []string

	for _, msg := range messages {
		role := msg.Role
		// Map OpenAI roles to Antigravity/Gemini roles
		switch role {
		case "system", "developer":
			// CLIProxyAPI: system/developer messages → systemInstruction
			if msg.Content != nil {
				if contentStr, ok := msg.Content.(string); ok && contentStr != "" {
					systemTexts = append(systemTexts, contentStr)
				}
			}
			continue
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
					// CLIProxyAPI adds thoughtSignature for function calls
					ThoughtSignature: "skip_thought_signature_validator",
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

	// Build systemInstruction from collected system messages
	if len(systemTexts) > 0 {
		*systemInstruction = &AntigravityContent{
			Role:  "user",
			Parts: []AntigravityPart{{Text: strings.Join(systemTexts, "\n")}},
		}
	}

	return contents
}

// convertToolsToAntigravityFormat converts OpenAI tools to Antigravity function declarations format
// CLIProxyAPI renames "parameters" to "parametersJsonSchema" for Gemini API compatibility
func convertToolsToAntigravityFormat(tools []dto.ToolCallRequest) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(tools))

	for _, tool := range tools {
		if tool.Type == "function" {
			funcDecl := map[string]interface{}{
				"name":        tool.Function.Name,
				"description": tool.Function.Description,
			}
			if tool.Function.Parameters != nil {
				// Rename "parameters" to "parametersJsonSchema" as required by Antigravity API
				funcDecl["parametersJsonSchema"] = tool.Function.Parameters
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

// ---- OpenAI Responses API conversion ----

func (a *Adaptor) ConvertOpenAIResponsesRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.OpenAIResponsesRequest) (any, error) {
	common.SysLog(fmt.Sprintf("antigravity: ConvertOpenAIResponsesRequest called, model=%s, RelayMode=%d, path=%s", request.Model, info.RelayMode, info.RequestURLPath))
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

	// Handle the "instructions" field from Responses API as systemInstruction
	var systemInstruction *AntigravityContent
	if request.Instructions != nil {
		var instrText string
		if err := common.Unmarshal(request.Instructions, &instrText); err == nil && instrText != "" {
			systemInstruction = &AntigravityContent{
				Role:  "user",
				Parts: []AntigravityPart{{Text: instrText}},
			}
		}
	}

	// Filter out thought/thoughtSignature parts from contents (upstream rejects them)
	// Also remove empty content entries after filtering (causes Gemini 3 Flash 400 errors)
	// Reference: OmniRoute antigravity.ts transformRequest
	contents = filterAntigravityContents(contents)

	// Convert tools to Antigravity format
	// CLIProxyAPI format: tools is [{"functionDeclarations": [...]}]
	var antigravityTools []AntigravityTools
	var toolConfig *AntigravityToolConfig
	if request.Tools != nil && len(request.Tools) > 0 {
		funcDecls := convertResponsesToolsToAntigravityFormat(request.Tools)
		if len(funcDecls) > 0 {
			antigravityTools = []AntigravityTools{{FunctionDeclarations: funcDecls}}
			toolConfig = &AntigravityToolConfig{
				FunctionCallingConfig: &AntigravityFunctionCallingConfig{
					Mode: "AUTO",
				},
			}
		}
	}

	// Build generation config - only include if at least one field is set
	var genConfig *AntigravityGenerationConfig
	if request.Temperature != nil || request.MaxOutputTokens != nil || request.TopP != nil {
		genConfig = &AntigravityGenerationConfig{
			Temperature:     request.Temperature,
			MaxOutputTokens: request.MaxOutputTokens,
			TopP:            request.TopP,
		}
	}

	// Build the Antigravity request
	antigravityReq := AntigravityRequest{
		Model:   model,
		Project: oauthKey.ProjectID, // OmniRoute uses projectId from OAuth credentials
		Request: AntigravityInnerRequest{
			Contents:          contents,
			SystemInstruction: systemInstruction,
			Tools:             antigravityTools,
			ToolConfig:        toolConfig,
			GenerationConfig:  genConfig,
			SafetySettings:    defaultSafetySettings(),
		},
	}

	return antigravityReq, nil
}

// convertResponsesToolsToAntigravityFormat converts OpenAI Responses API tools (json.RawMessage)
// to Antigravity function declarations format
func convertResponsesToolsToAntigravityFormat(tools json.RawMessage) []map[string]interface{} {
	var toolsArray []map[string]interface{}
	if err := common.Unmarshal(tools, &toolsArray); err != nil {
		return nil
	}

	result := make([]map[string]interface{}, 0, len(toolsArray))
	for _, tool := range toolsArray {
		toolType, _ := tool["type"].(string)
		switch toolType {
		case "function":
			if function, ok := tool["function"].(map[string]interface{}); ok {
				funcDecl := map[string]interface{}{
					"name":        function["name"],
					"description": function["description"],
				}
				if params, ok := function["parameters"]; ok {
					funcDecl["parametersJsonSchema"] = params
				}
				result = append(result, funcDecl)
			}
		case "google_search":
			result = append(result, map[string]interface{}{"googleSearch": tool})
		case "code_execution", "code_interpreter":
			result = append(result, map[string]interface{}{"codeExecution": tool})
		}
	}

	return result
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
	case "system", "developer":
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
					ThoughtSignature: "skip_thought_signature_validator",
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
	// Claude /v1/messages format
	if info.RelayFormat == types.RelayFormatClaude {
		if a.forceStream {
			return AntigravityClaudeStreamToChatHandler(c, info, resp)
		}
		if info.IsStream {
			return AntigravityClaudeStreamHandler(c, info, resp)
		}
		return AntigravityClaudeHandler(c, info, resp)
	}

	if info.RelayMode == relayconstant.RelayModeChatCompletions {
		if a.forceStream {
			return AntigravityStreamToChatHandler(c, info, resp)
		}
		if info.IsStream {
			return AntigravityStreamHandler(c, info, resp)
		}
		return AntigravityChatHandler(c, info, resp)
	}

	if info.RelayMode != relayconstant.RelayModeResponses && info.RelayMode != relayconstant.RelayModeResponsesCompact && info.RelayFormat != types.RelayFormatClaude {
		return nil, types.NewError(errors.New("antigravity channel: endpoint not supported"), types.ErrorCodeInvalidRequest)
	}

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
	// Support Claude format (RelayModeUnknown + RelayFormatClaude) in addition to Responses and ChatCompletions
	if info.RelayMode != relayconstant.RelayModeResponses && info.RelayMode != relayconstant.RelayModeResponsesCompact && info.RelayMode != relayconstant.RelayModeChatCompletions {
		if info.RelayFormat != types.RelayFormatClaude {
			return "", errors.New("antigravity channel: only /v1/responses, /v1/chat/completions and /v1/messages are supported")
		}
	}

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

	// Antigravity-specific headers
	// User-Agent MUST be "antigravity/X.X.X ..." — using "google-api-nodejs-client" causes 404
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
		// NOTE: We keep parts with ThoughtSignature == "skip_thought_signature_validator"
		// since those are intentionally added by our converter
		parts := make([]AntigravityPart, 0, len(c.Parts))
		for _, p := range c.Parts {
			// Skip thought parts (but keep thoughtSignature "skip_thought_signature_validator")
			if p.Thought {
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
