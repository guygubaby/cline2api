package main

import (
	"crypto/sha256"
	"fmt"
	"unicode/utf8"
)

const (
	maxUpstreamToolNameLength = 64
	proxyToolNamesParamKey    = "_cline2api_tool_names"
)

func shortenToolName(name string) string {
	sum := sha256.Sum256([]byte(name))
	prefix := name[:55]
	for !utf8.ValidString(prefix) {
		prefix = prefix[:len(prefix)-1]
	}
	return fmt.Sprintf("%s_%x", prefix, sum[:4])
}

// clampParamsToolNames keeps Cline/OpenRouter requests within Meta's 64-byte
// function-name limit while preserving a request-local map for the response.
func clampParamsToolNames(params map[string]any) map[string]string {
	if existing, ok := params[proxyToolNamesParamKey].(map[string]string); ok {
		return existing
	}

	names := map[string]string{}
	clamp := func(function map[string]any) {
		name, _ := function["name"].(string)
		if len(name) <= maxUpstreamToolNameLength {
			return
		}
		short := shortenToolName(name)
		names[short] = name
		function["name"] = short
	}

	if tools, ok := params["tools"].([]any); ok {
		for _, rawTool := range tools {
			tool, _ := rawTool.(map[string]any)
			function, _ := tool["function"].(map[string]any)
			if function != nil {
				clamp(function)
			}
		}
	}
	if messages, ok := params["messages"].([]any); ok {
		for _, rawMessage := range messages {
			message, _ := rawMessage.(map[string]any)
			calls, _ := message["tool_calls"].([]any)
			for _, rawCall := range calls {
				call, _ := rawCall.(map[string]any)
				function, _ := call["function"].(map[string]any)
				if function != nil {
					clamp(function)
				}
			}
		}
	}
	if choice, ok := params["tool_choice"].(map[string]any); ok {
		if function, _ := choice["function"].(map[string]any); function != nil {
			clamp(function)
		}
	}

	if len(names) == 0 {
		return nil
	}
	params[proxyToolNamesParamKey] = names
	return names
}

func restoreToolCallsInResponse(response map[string]any, names map[string]string) {
	if len(names) == 0 {
		return
	}
	choices, _ := response["choices"].([]any)
	for _, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		for _, field := range []string{"delta", "message"} {
			output, _ := choice[field].(map[string]any)
			calls, _ := output["tool_calls"].([]any)
			for _, rawCall := range calls {
				call, _ := rawCall.(map[string]any)
				function, _ := call["function"].(map[string]any)
				name, _ := function["name"].(string)
				if original := names[name]; original != "" {
					function["name"] = original
				}
			}
		}
	}
}
