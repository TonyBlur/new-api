package antigravity

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/relay/channel/gemini"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

// Antigravity API wraps Gemini responses in {"response": {...}, "traceId": "...", "metadata": {}}
// We need to unwrap the "response" field before passing to Gemini handlers.

// antigravityResponse represents the top-level Antigravity response wrapper
type antigravityResponse struct {
	Response json.RawMessage `json:"response"`
	TraceID  string          `json:"traceId,omitempty"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

// AntigravityChatHandler handles non-streaming Antigravity responses
func AntigravityChatHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	service.CloseResponseBodyGracefully(resp)

	// Unwrap Antigravity response wrapper
	unwrapped := unwrapAntigravityResponse(responseBody)

	// Create a new response with unwrapped body for Gemini handler
	newResp := &http.Response{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       io.NopCloser(strings.NewReader(string(unwrapped))),
	}

	return gemini.GeminiChatHandler(c, info, newResp)
}

// AntigravityStreamHandler handles streaming Antigravity responses
// It uses a pipe to intercept and unwrap SSE data on the fly
func AntigravityStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	// Create a pipe to intercept the stream
	pr, pw := io.Pipe()

	// Start a goroutine to read from original response, unwrap, and write to pipe
	go func() {
		defer pw.Close()
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()

			// Check if this is a data line that needs unwrapping
			if strings.HasPrefix(line, "data: ") {
				data := strings.TrimPrefix(line, "data: ")
				unwrapped := unwrapSSEData(data)
				line = "data: " + unwrapped
			}

			pw.Write([]byte(line + "\n"))
		}
		resp.Body.Close()
	}()

	// Create a new response with the piped reader
	newResp := &http.Response{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       pr,
	}

	return gemini.GeminiChatStreamHandler(c, info, newResp)
}

// AntigravityStreamToChatHandler handles the case where we forced a streaming request to the upstream
// (e.g., for gpt-oss models that fail on non-streaming requests) but the client expects a non-streaming response.
// It collects all SSE chunks, merges them into a single Gemini response, and then processes as non-streaming.
func AntigravityStreamToChatHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	// Read the entire streaming response and collect all Gemini chunks
	var allChunks []json.RawMessage
	scanner := bufio.NewScanner(resp.Body)
	// Increase scanner buffer for large responses
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}

		// Unwrap Antigravity wrapper if present
		unwrapped := unwrapSSEData(data)

		var chunk json.RawMessage = json.RawMessage(unwrapped)
		allChunks = append(allChunks, chunk)
	}
	resp.Body.Close()

	// Merge all Gemini stream chunks into a single Gemini response
	mergedResponse := mergeGeminiStreamChunks(allChunks)
	mergedBody, err := common.Marshal(mergedResponse)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}

	// Create a new response with the merged body for Gemini non-streaming handler
	newResp := &http.Response{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       io.NopCloser(strings.NewReader(string(mergedBody))),
	}

	return gemini.GeminiChatHandler(c, info, newResp)
}

// mergeGeminiStreamChunks merges multiple Gemini stream chunks into a single response
// This simulates what the non-streaming response would look like
func mergeGeminiStreamChunks(chunks []json.RawMessage) map[string]interface{} {
	result := map[string]interface{}{
		"candidates": []interface{}{},
	}

	var allCandidates []interface{}
	var usageMetadata map[string]interface{}

	for _, chunk := range chunks {
		var parsed map[string]interface{}
		if err := common.Unmarshal(chunk, &parsed); err != nil {
			continue
		}

		// Collect candidates
		if candidates, ok := parsed["candidates"].([]interface{}); ok {
			allCandidates = mergeCandidates(allCandidates, candidates)
		}

		// Use the last usageMetadata
		if um, ok := parsed["usageMetadata"].(map[string]interface{}); ok {
			usageMetadata = um
		}
	}

	result["candidates"] = allCandidates
	if usageMetadata != nil {
		result["usageMetadata"] = usageMetadata
	}

	return result
}

// mergeCandidates merges streaming candidates into a single candidate
func mergeCandidates(existing []interface{}, newCandidates []interface{}) []interface{} {
	for _, nc := range newCandidates {
		candidate, ok := nc.(map[string]interface{})
		if !ok {
			continue
		}

		// Find matching candidate by index (default to 0 if not present)
		idx := 0
		if index, ok := candidate["index"].(float64); ok {
			idx = int(index)
		}

		// Extend existing candidates list if needed
		for len(existing) <= idx {
			existing = append(existing, map[string]interface{}{
				"content": map[string]interface{}{
					"parts": []interface{}{},
					"role":  "model",
				},
			})
		}

		existingCandidate, ok := existing[idx].(map[string]interface{})
		if !ok {
			existing[idx] = candidate
			continue
		}

		// Merge content parts
		if content, ok := candidate["content"].(map[string]interface{}); ok {
			existingContent, ok := existingCandidate["content"].(map[string]interface{})
			if !ok {
				existingContent = map[string]interface{}{
					"parts": []interface{}{},
					"role":  "model",
				}
			}

			if parts, ok := content["parts"].([]interface{}); ok {
				existingParts, _ := existingContent["parts"].([]interface{})
				for _, part := range parts {
					partMap, ok := part.(map[string]interface{})
					if !ok {
						continue
					}
					// Separate text vs thought text: thought parts go into reasoning,
					// regular text parts get concatenated
					isThought := false
					if thought, ok := partMap["thought"].(bool); ok && thought {
						isThought = true
					}

					if text, hasText := partMap["text"].(string); hasText {
						if isThought {
							// For thought parts, use a separate "thought" text accumulation
							// We'll keep them as-is in parts for Gemini handler to process
							existingParts = append(existingParts, partMap)
						} else {
							// Regular text: find last text part and concatenate
							if len(existingParts) > 0 {
								lastPart, ok := existingParts[len(existingParts)-1].(map[string]interface{})
								if ok {
									if lastText, hasLastText := lastPart["text"].(string); hasLastText {
										if lastThought, isThought := lastPart["thought"].(bool); !isThought || !lastThought {
											lastPart["text"] = lastText + text
											continue
										}
									}
								}
							}
							existingParts = append(existingParts, partMap)
						}
					} else {
						// Non-text parts (e.g., functionCall) just append
						existingParts = append(existingParts, partMap)
					}
				}
				existingContent["parts"] = existingParts
			}

			if role, ok := content["role"].(string); ok {
				existingContent["role"] = role
			}
			existingCandidate["content"] = existingContent
		}

		// Use the last finishReason
		if fr, ok := candidate["finishReason"]; ok {
			existingCandidate["finishReason"] = fr
		}
	}

	return existing
}

// unwrapAntigravityResponse extracts the inner Gemini response from Antigravity wrapper
func unwrapAntigravityResponse(body []byte) []byte {
	var wrapper antigravityResponse
	if err := common.Unmarshal(body, &wrapper); err != nil {
		// If we can't parse as wrapper, return as-is
		return body
	}

	if wrapper.Response != nil {
		return wrapper.Response
	}

	// No "response" field, return as-is
	return body
}

// unwrapSSEData unwraps a single SSE data line if it's wrapped in Antigravity format
func unwrapSSEData(data string) string {
	data = strings.TrimSpace(data)
	if !strings.HasPrefix(data, "{") {
		return data
	}

	var wrapper antigravityResponse
	if err := common.Unmarshal([]byte(data), &wrapper); err != nil {
		return data
	}

	if wrapper.Response != nil {
		return string(wrapper.Response)
	}

	return data
}

// AntigravityGeminiHandler handles non-streaming Gemini native format responses
// Unwraps Antigravity wrapper and returns Gemini native format
func AntigravityGeminiHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	service.CloseResponseBodyGracefully(resp)

	// Unwrap Antigravity response wrapper
	unwrapped := unwrapAntigravityResponse(responseBody)

	// Create a new response with unwrapped body for Gemini native handler
	newResp := &http.Response{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       io.NopCloser(strings.NewReader(string(unwrapped))),
	}

	return gemini.GeminiTextGenerationHandler(c, info, newResp)
}

// AntigravityGeminiStreamHandler handles streaming Gemini native format responses
// Unwraps Antigravity SSE wrapper and returns Gemini native SSE stream
func AntigravityGeminiStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	// Create a pipe to intercept the stream
	pr, pw := io.Pipe()

	// Start a goroutine to read from original response, unwrap, and write to pipe
	go func() {
		defer pw.Close()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				data := strings.TrimPrefix(line, "data: ")
				unwrapped := unwrapSSEData(data)
				if _, err := fmt.Fprintf(pw, "data: %s\n\n", unwrapped); err != nil {
					return
				}
			} else if line != "" {
				if _, err := fmt.Fprintf(pw, "%s\n", line); err != nil {
					return
				}
			}
		}
	}()

	// Create a new response with the pipe reader
	newResp := &http.Response{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       pr,
	}

	return gemini.GeminiTextGenerationStreamHandler(c, info, newResp)
}

// ---- Responses API handlers ----
// Antigravity returns Gemini-format responses, but the Responses API expects OpenAI Responses format.
// These handlers unwrap the Antigravity wrapper, parse the Gemini response, and convert to OpenAI Responses format.

// geminiTextAndReasoning extracts text and reasoning content from a Gemini response
func geminiTextAndReasoning(geminiResp map[string]interface{}) (text string, reasoning string) {
	candidates, _ := geminiResp["candidates"].([]interface{})
	if len(candidates) == 0 {
		return
	}
	candidate, _ := candidates[0].(map[string]interface{})
	content, _ := candidate["content"].(map[string]interface{})
	parts, _ := content["parts"].([]interface{})

	var textParts []string
	var thoughtParts []string
	for _, p := range parts {
		partMap, _ := p.(map[string]interface{})
		t, _ := partMap["text"].(string)
		if t == "" {
			continue
		}
		if thought, _ := partMap["thought"].(bool); thought {
			thoughtParts = append(thoughtParts, t)
		} else {
			textParts = append(textParts, t)
		}
	}
	text = strings.Join(textParts, "")
	reasoning = strings.Join(thoughtParts, "")
	return
}

// geminiUsage extracts usage metadata from a Gemini response
func geminiUsage(geminiResp map[string]interface{}) (promptTokens, completionTokens, totalTokens int) {
	um, _ := geminiResp["usageMetadata"].(map[string]interface{})
	if um == nil {
		return
	}
	if v, ok := um["promptTokenCount"].(float64); ok {
		promptTokens = int(v)
	}
	if v, ok := um["candidatesTokenCount"].(float64); ok {
		completionTokens = int(v)
	}
	if v, ok := um["totalTokenCount"].(float64); ok {
		totalTokens = int(v)
	}
	return
}

// geminiToResponsesOutput converts Gemini response to OpenAI Responses output items
func geminiToResponsesOutput(geminiResp map[string]interface{}) []dto.ResponsesOutput {
	var outputs []dto.ResponsesOutput

	candidates, _ := geminiResp["candidates"].([]interface{})
	if len(candidates) == 0 {
		return outputs
	}
	candidate, _ := candidates[0].(map[string]interface{})
	content, _ := candidate["content"].(map[string]interface{})
	parts, _ := content["parts"].([]interface{})

	// Collect reasoning (thought) parts separately
	var thoughtTexts []string
	var regularTexts []string
	var functionCalls []dto.ResponsesOutput

	for _, p := range parts {
		partMap, _ := p.(map[string]interface{})
		// Thought/reasoning parts
		if thought, _ := partMap["thought"].(bool); thought {
			if t, _ := partMap["text"].(string); t != "" {
				thoughtTexts = append(thoughtTexts, t)
			}
			continue
		}
		// Function calls
		if fc, _ := partMap["functionCall"].(map[string]interface{}); fc != nil {
			name, _ := fc["name"].(string)
			args, _ := common.Marshal(fc["args"])
			functionCalls = append(functionCalls, dto.ResponsesOutput{
				Type:      "function_call",
				ID:        "fc_" + common.GetRandomString(12),
				CallId:    "call_" + common.GetRandomString(12),
				Name:      name,
				Arguments: json.RawMessage(args),
				Status:    "completed",
			})
			continue
		}
		// Regular text parts
		if t, _ := partMap["text"].(string); t != "" {
			regularTexts = append(regularTexts, t)
		}
	}

	// Add reasoning output if present
	if len(thoughtTexts) > 0 {
		reasoningContent := []dto.ResponsesOutputContent{
			{
				Type: "summary_text",
				Text: strings.Join(thoughtTexts, ""),
			},
		}
		outputs = append(outputs, dto.ResponsesOutput{
			Type:    "reasoning",
			ID:      "rs_" + common.GetRandomString(12),
			Content: reasoningContent,
			Status:  "completed",
		})
	}

	// Add message output
	if len(regularTexts) > 0 || len(functionCalls) == 0 {
		msgContent := []dto.ResponsesOutputContent{
			{
				Type: "output_text",
				Text: strings.Join(regularTexts, ""),
			},
		}
		outputs = append(outputs, dto.ResponsesOutput{
			Type:    "message",
			ID:      "msg_" + common.GetRandomString(12),
			Role:    "assistant",
			Content: msgContent,
			Status:  "completed",
		})
	}

	// Add function calls
	outputs = append(outputs, functionCalls...)

	return outputs
}

// buildResponsesJSON constructs the OpenAI Responses format JSON
func buildResponsesJSON(geminiResp map[string]interface{}, model string) map[string]interface{} {
	text, _ := geminiTextAndReasoning(geminiResp)
	promptTokens, completionTokens, totalTokens := geminiUsage(geminiResp)

	// If no total tokens, calculate
	if totalTokens == 0 {
		totalTokens = promptTokens + completionTokens
	}

	outputs := geminiToResponsesOutput(geminiResp)

	// If no outputs were generated, create a minimal message output
	if len(outputs) == 0 {
		outputs = append(outputs, dto.ResponsesOutput{
			Type:    "message",
			ID:      "msg_" + common.GetRandomString(12),
			Role:    "assistant",
			Content: []dto.ResponsesOutputContent{{Type: "output_text", Text: text}},
			Status:  "completed",
		})
	}

	return map[string]interface{}{
		"id":         "resp_" + common.GetRandomString(24),
		"object":     "response",
		"created_at": 0,
		"status":     "completed",
		"model":      model,
		"output":     outputs,
		"usage": map[string]interface{}{
			"input_tokens":  promptTokens,
			"output_tokens": completionTokens,
			"total_tokens":  totalTokens,
		},
	}
}

// AntigravityResponsesHandler handles non-streaming Antigravity responses for Responses API
func AntigravityResponsesHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	service.CloseResponseBodyGracefully(resp)

	// Check for error status
	if resp.StatusCode != http.StatusOK {
		return nil, types.NewOpenAIError(fmt.Errorf("upstream returned status %d: %s", resp.StatusCode, string(responseBody)), types.ErrorCodeBadResponse, resp.StatusCode)
	}

	// Unwrap Antigravity response wrapper
	unwrapped := unwrapAntigravityResponse(responseBody)

	// Parse Gemini response
	var geminiResp map[string]interface{}
	if err := common.Unmarshal(unwrapped, &geminiResp); err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}

	// Convert to OpenAI Responses format
	responsesJSON := buildResponsesJSON(geminiResp, info.UpstreamModelName)
	responseBytes, err := common.Marshal(responsesJSON)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}

	// Write response
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(http.StatusOK)
	c.Writer.Write(responseBytes)

	// Extract usage
	promptTokens, completionTokens, totalTokens := geminiUsage(geminiResp)
	usage := &dto.Usage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      totalTokens,
	}

	return usage, nil
}

// AntigravityResponsesStreamHandler handles streaming Antigravity responses for Responses API
// It collects the Gemini stream, unwraps it, and converts to OpenAI Responses streaming format
func AntigravityResponsesStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	// Collect all SSE chunks from the Gemini stream
	var allChunks []json.RawMessage
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		// Unwrap Antigravity wrapper
		unwrapped := unwrapSSEData(data)
		allChunks = append(allChunks, json.RawMessage(unwrapped))
	}
	resp.Body.Close()

	// Merge chunks into a single Gemini response
	mergedResponse := mergeGeminiStreamChunks(allChunks)

	// Convert to OpenAI Responses streaming format
	// For streaming, we send the complete response as a single response.completed event
	// This is simpler than trying to convert each Gemini SSE chunk individually
	responseID := "resp_" + common.GetRandomString(24)
	outputs := geminiToResponsesOutput(mergedResponse)
	promptTokens, completionTokens, totalTokens := geminiUsage(mergedResponse)
	if totalTokens == 0 {
		totalTokens = promptTokens + completionTokens
	}

	// Set up SSE
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")

	// Send response.created event
	createdEvent := map[string]interface{}{
		"type": "response.created",
		"response": map[string]interface{}{
			"id":         responseID,
			"object":     "response",
			"created_at": 0,
			"status":     "in_progress",
			"model":      info.UpstreamModelName,
			"output":     []interface{}{},
		},
	}
	writeSSE(c, createdEvent)

	// Send output item events for each output
	for i, output := range outputs {
		// output_item.added
		addedEvent := map[string]interface{}{
			"type":         "response.output_item.added",
			"output_index": i,
			"item":         output,
		}
		writeSSE(c, addedEvent)

		// content_part events for message items
		if output.Type == "message" && len(output.Content) > 0 {
			for j, contentItem := range output.Content {
				partAddedEvent := map[string]interface{}{
					"type":          "response.content_part.added",
					"output_index":  i,
					"content_index": j,
					"part":          contentItem,
				}
				writeSSE(c, partAddedEvent)

				// text delta
				if contentItem.Text != "" {
					deltaEvent := map[string]interface{}{
						"type":          "response.output_text.delta",
						"output_index":  i,
						"content_index": j,
						"delta":         contentItem.Text,
					}
					writeSSE(c, deltaEvent)

					// text done
					textDoneEvent := map[string]interface{}{
						"type":          "response.output_text.done",
						"output_index":  i,
						"content_index": j,
						"text":          contentItem.Text,
					}
					writeSSE(c, textDoneEvent)
				}
			}
		}

		// output_item.done
		doneEvent := map[string]interface{}{
			"type":         "response.output_item.done",
			"output_index": i,
			"item":         output,
		}
		writeSSE(c, doneEvent)
	}

	// Send response.completed event
	completedEvent := map[string]interface{}{
		"type": "response.completed",
		"response": map[string]interface{}{
			"id":         responseID,
			"object":     "response",
			"created_at": 0,
			"status":     "completed",
			"model":      info.UpstreamModelName,
			"output":     outputs,
			"usage": map[string]interface{}{
				"input_tokens":  promptTokens,
				"output_tokens": completionTokens,
				"total_tokens":  totalTokens,
			},
		},
	}
	writeSSE(c, completedEvent)

	// Send [DONE]
	c.Writer.Write([]byte("data: [DONE]\n\n"))
	c.Writer.Flush()

	usage := &dto.Usage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      totalTokens,
	}

	return usage, nil
}

// AntigravityStreamToResponsesHandler handles the case where we forced a streaming request to the upstream
// (e.g., for gpt-oss models) but the client expects a non-streaming Responses API response.
func AntigravityStreamToResponsesHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	// Collect all SSE chunks from the Gemini stream
	var allChunks []json.RawMessage
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		// Unwrap Antigravity wrapper
		unwrapped := unwrapSSEData(data)
		allChunks = append(allChunks, json.RawMessage(unwrapped))
	}
	resp.Body.Close()

	// Merge chunks into a single Gemini response
	mergedResponse := mergeGeminiStreamChunks(allChunks)

	// Convert to OpenAI Responses format
	responsesJSON := buildResponsesJSON(mergedResponse, info.UpstreamModelName)
	responseBytes, err := common.Marshal(responsesJSON)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}

	// Write response
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(http.StatusOK)
	c.Writer.Write(responseBytes)

	// Extract usage
	promptTokens, completionTokens, totalTokens := geminiUsage(mergedResponse)
	usage := &dto.Usage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      totalTokens,
	}

	return usage, nil
}

// writeSSE writes a server-sent event to the response writer
func writeSSE(c *gin.Context, data interface{}) {
	jsonData, err := common.Marshal(data)
	if err != nil {
		return
	}
	c.Writer.Write([]byte("data: " + string(jsonData) + "\n\n"))
	c.Writer.Flush()
}

// ---- Claude /v1/messages format handlers ----

// geminiToClaudeContent converts Gemini response parts to Claude content blocks
func geminiToClaudeContent(geminiResp map[string]interface{}) ([]dto.ClaudeMediaMessage, string) {
	var contentBlocks []dto.ClaudeMediaMessage
	var stopReason string

	candidates, _ := geminiResp["candidates"].([]interface{})
	if len(candidates) == 0 {
		return contentBlocks, "end_turn"
	}
	candidate, _ := candidates[0].(map[string]interface{})

	// Map finishReason to Claude stop_reason
	if fr, ok := candidate["finishReason"].(string); ok {
		switch fr {
		case "STOP", "stop":
			stopReason = "end_turn"
		case "MAX_TOKENS", "max_tokens":
			stopReason = "max_tokens"
		case "SAFETY", "safety":
			stopReason = "end_turn"
		case "RECITATION", "recitation":
			stopReason = "end_turn"
		case "LANGUAGE", "language":
			stopReason = "end_turn"
		default:
			stopReason = "end_turn"
		}
	} else {
		stopReason = "end_turn"
	}

	c, _ := candidate["content"].(map[string]interface{})
	if c == nil {
		return contentBlocks, stopReason
	}
	parts, _ := c["parts"].([]interface{})

	toolUseIndex := 0
	for _, p := range parts {
		partMap, _ := p.(map[string]interface{})

		// Thought/reasoning parts → Claude thinking block
		if thought, _ := partMap["thought"].(bool); thought {
			if t, _ := partMap["text"].(string); t != "" {
				contentBlocks = append(contentBlocks, dto.ClaudeMediaMessage{
					Type:     "thinking",
					Thinking: &t,
				})
			}
			continue
		}

		// Function call → Claude tool_use block
		if fc, _ := partMap["functionCall"].(map[string]interface{}); fc != nil {
			name, _ := fc["name"].(string)
			args := fc["args"]
			toolID := fmt.Sprintf("toolu_%s", common.GetRandomString(12))
			contentBlocks = append(contentBlocks, dto.ClaudeMediaMessage{
				Type:  "tool_use",
				Id:    toolID,
				Name:  name,
				Input: args,
			})
			toolUseIndex++
			stopReason = "tool_use"
			continue
		}

		// Regular text → Claude text block
		if t, _ := partMap["text"].(string); t != "" {
			contentBlocks = append(contentBlocks, dto.ClaudeMediaMessage{
				Type: "text",
				Text: &t,
			})
		}
	}

	return contentBlocks, stopReason
}

// AntigravityClaudeHandler handles non-streaming Antigravity responses for /v1/messages
func AntigravityClaudeHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	service.CloseResponseBodyGracefully(resp)

	// Check for error status
	if resp.StatusCode != http.StatusOK {
		return nil, types.NewOpenAIError(fmt.Errorf("upstream returned status %d: %s", resp.StatusCode, string(responseBody)), types.ErrorCodeBadResponse, resp.StatusCode)
	}

	// Unwrap Antigravity response wrapper
	unwrapped := unwrapAntigravityResponse(responseBody)

	// Parse Gemini response
	var geminiResp map[string]interface{}
	if err := common.Unmarshal(unwrapped, &geminiResp); err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}

	// Convert to Claude format
	contentBlocks, stopReason := geminiToClaudeContent(geminiResp)
	promptTokens, completionTokens, _ := geminiUsage(geminiResp)

	claudeResp := dto.ClaudeResponse{
		Id:         "msg_" + common.GetRandomString(24),
		Type:       "message",
		Role:       "assistant",
		Content:    contentBlocks,
		Model:      info.UpstreamModelName,
		StopReason: stopReason,
		Usage: &dto.ClaudeUsage{
			InputTokens:  promptTokens,
			OutputTokens: completionTokens,
		},
	}

	respBytes, err := common.Marshal(claudeResp)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}

	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(http.StatusOK)
	c.Writer.Write(respBytes)

	usage := &dto.Usage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
	}

	return usage, nil
}

// AntigravityClaudeStreamHandler handles streaming Antigravity responses for /v1/messages
func AntigravityClaudeStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")

	msgID := "msg_" + common.GetRandomString(24)

	// Send message_start event
	startEvent := dto.ClaudeResponse{
		Type: "message_start",
		Message: &dto.ClaudeMediaMessage{
			Id:    msgID,
			Type:  "message",
			Role:  "assistant",
			Model: info.UpstreamModelName,
			Usage: &dto.ClaudeUsage{InputTokens: 0, OutputTokens: 0},
		},
	}
	writeClaudeSSE(c, startEvent)

	// Send content_block_start for text
	writeClaudeSSE(c, dto.ClaudeResponse{
		Type:  "content_block_start",
		Index: intPtr(0),
		ContentBlock: &dto.ClaudeMediaMessage{
			Type: "text",
			Text: strPtr(""),
		},
	})

	// Read and process the Antigravity SSE stream
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var totalText string
	var stopReason string = "end_turn"
	var promptTokens, completionTokens int
	contentBlockIndex := 0

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}

		// Unwrap Antigravity wrapper
		unwrapped := unwrapSSEData(data)

		var chunk map[string]interface{}
		if err := common.Unmarshal(json.RawMessage(unwrapped), &chunk); err != nil {
			continue
		}

		// Extract usage metadata
		if um, ok := chunk["usageMetadata"].(map[string]interface{}); ok {
			if v, ok := um["promptTokenCount"].(float64); ok {
				promptTokens = int(v)
			}
			if v, ok := um["candidatesTokenCount"].(float64); ok {
				completionTokens = int(v)
			}
		}

		// Extract candidates
		candidates, _ := chunk["candidates"].([]interface{})
		if len(candidates) == 0 {
			continue
		}
		candidate, _ := candidates[0].(map[string]interface{})

		// Check finishReason
		if fr, ok := candidate["finishReason"].(string); ok {
			switch fr {
			case "STOP":
				stopReason = "end_turn"
			case "MAX_TOKENS":
				stopReason = "max_tokens"
			case "SAFETY":
				stopReason = "end_turn"
			default:
				stopReason = "end_turn"
			}
		}

		content, _ := candidate["content"].(map[string]interface{})
		if content == nil {
			continue
		}
		parts, _ := content["parts"].([]interface{})

		for _, p := range parts {
			partMap, _ := p.(map[string]interface{})

			// Skip thought parts in streaming
			if thought, _ := partMap["thought"].(bool); thought {
				continue
			}

			// Function call
			if fc, _ := partMap["functionCall"].(map[string]interface{}); fc != nil {
				// Close the current text block first
				if totalText != "" || contentBlockIndex == 0 {
					writeClaudeSSE(c, dto.ClaudeResponse{
						Type:  "content_block_stop",
						Index: intPtr(contentBlockIndex),
					})
					contentBlockIndex++
				}

				name, _ := fc["name"].(string)
				args := fc["args"]
				toolID := fmt.Sprintf("toolu_%s", common.GetRandomString(12))

				// Send tool_use content_block_start
				writeClaudeSSE(c, dto.ClaudeResponse{
					Type:  "content_block_start",
					Index: intPtr(contentBlockIndex),
					ContentBlock: &dto.ClaudeMediaMessage{
						Type:  "tool_use",
						Id:    toolID,
						Name:  name,
						Input: map[string]interface{}{},
					},
				})

				// Send tool input as delta
				argsJSON, _ := common.Marshal(args)
				writeClaudeSSE(c, dto.ClaudeResponse{
					Type:  "content_block_delta",
					Index: intPtr(contentBlockIndex),
					Delta: &dto.ClaudeMediaMessage{
						Type:        "input_json_delta",
						PartialJson: strPtr(string(argsJSON)),
					},
				})

				// Close tool block
				writeClaudeSSE(c, dto.ClaudeResponse{
					Type:  "content_block_stop",
					Index: intPtr(contentBlockIndex),
				})
				contentBlockIndex++
				stopReason = "tool_use"
				continue
			}

			// Text delta
			if t, _ := partMap["text"].(string); t != "" {
				totalText += t
				writeClaudeSSE(c, dto.ClaudeResponse{
					Type:  "content_block_delta",
					Index: intPtr(contentBlockIndex),
					Delta: &dto.ClaudeMediaMessage{
						Type:  "text_delta",
						Delta: t,
					},
				})
			}
		}
	}
	resp.Body.Close()

	// Close the last text content block
	writeClaudeSSE(c, dto.ClaudeResponse{
		Type:  "content_block_stop",
		Index: intPtr(contentBlockIndex),
	})

	// Send message_delta with stop reason
	writeClaudeSSE(c, dto.ClaudeResponse{
		Type: "message_delta",
		Delta: &dto.ClaudeMediaMessage{
			StopReason: &stopReason,
		},
		Usage: &dto.ClaudeUsage{
			OutputTokens: completionTokens,
		},
	})

	// Send message_stop
	writeClaudeSSE(c, dto.ClaudeResponse{
		Type: "message_stop",
	})

	c.Writer.Write([]byte("data: [DONE]\n\n"))
	c.Writer.Flush()

	usage := &dto.Usage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
	}

	return usage, nil
}

// AntigravityClaudeStreamToChatHandler handles forceStream for /v1/messages
// Collects streaming response and returns as non-streaming Claude JSON
func AntigravityClaudeStreamToChatHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	// Collect all SSE chunks from the Gemini stream
	var allChunks []json.RawMessage
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		// Unwrap Antigravity wrapper
		unwrapped := unwrapSSEData(data)
		allChunks = append(allChunks, json.RawMessage(unwrapped))
	}
	resp.Body.Close()

	// Merge chunks into a single Gemini response
	mergedResponse := mergeGeminiStreamChunks(allChunks)

	// Convert to Claude format
	contentBlocks, stopReason := geminiToClaudeContent(mergedResponse)
	promptTokens, completionTokens, _ := geminiUsage(mergedResponse)

	claudeResp := dto.ClaudeResponse{
		Id:         "msg_" + common.GetRandomString(24),
		Type:       "message",
		Role:       "assistant",
		Content:    contentBlocks,
		Model:      info.UpstreamModelName,
		StopReason: stopReason,
		Usage: &dto.ClaudeUsage{
			InputTokens:  promptTokens,
			OutputTokens: completionTokens,
		},
	}

	respBytes, err := common.Marshal(claudeResp)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}

	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(http.StatusOK)
	c.Writer.Write(respBytes)

	usage := &dto.Usage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
	}

	return usage, nil
}

// writeClaudeSSE writes a Claude SSE event
func writeClaudeSSE(c *gin.Context, data interface{}) {
	jsonData, err := common.Marshal(data)
	if err != nil {
		return
	}
	c.Writer.Write([]byte("event: \ndata: " + string(jsonData) + "\n\n"))
	c.Writer.Flush()
}

// intPtr returns a pointer to the given int
func intPtr(i int) *int {
	return &i
}

// strPtr returns a pointer to the given string
func strPtr(s string) *string {
	return &s
}
