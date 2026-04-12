package antigravity

import (
	"strings"
)

// Antigravity model list
// Based on CLIProxyAPI models.json: https://github.com/router-for-me/CLIProxyAPI/blob/main/internal/registry/models/models.json
// Only models that actually work on the upstream API are included.
// gemini-2.5 series removed: upstream 429/quota exhausted, not usable.
var baseModelList = []string{
	// Claude models (via Antigravity)
	"claude-opus-4-6-thinking",
	"claude-sonnet-4-6",
	// Gemini 3.1 series (pro split by thinking level)
	"gemini-3.1-pro-high",
	"gemini-3.1-pro-low",
	"gemini-3.1-flash-image",
	// Gemini 3 series
	"gemini-3-flash",
	// GPT OSS
	"gpt-oss-120b-medium",
}

// ModelList is the base model list (no compact variants - they have no practical value for Antigravity)
var ModelList = baseModelList

const ChannelName = "antigravity"

// ModelAliasMap maps user-friendly model names to internal Antigravity model names
// Only includes shorthand/convenience aliases, NOT legacy unsupported models
var ModelAliasMap = map[string]string{
	// Gemini 3.1 Pro aliases - map generic name to default high variant
	"gemini-3.1-pro":          "gemini-3.1-pro-high",
	// Claude shorthand aliases
	"claude-opus-4.6":         "claude-opus-4-6-thinking",
	"claude-opus-4-6":         "claude-opus-4-6-thinking",
	"claude-opus":             "claude-opus-4-6-thinking",
	"claude-sonnet-4.6":       "claude-sonnet-4-6",
	"claude-sonnet-4-6-thinking": "claude-sonnet-4-6",
	"claude-sonnet":           "claude-sonnet-4-6",
	// GPT OSS shorthand
	"gpt-oss":                 "gpt-oss-120b-medium",
}

// ResolveModelAlias resolves a model alias to the internal model name
// If the model is not an alias, returns the original name
func ResolveModelAlias(model string) string {
	// Clean model name - remove provider prefix
	if idx := strings.LastIndex(model, "/"); idx != -1 {
		model = model[idx+1:]
	}
	model = strings.TrimPrefix(model, "antigravity-")

	// Check alias map
	if aliased, ok := ModelAliasMap[model]; ok {
		return aliased
	}
	return model
}
