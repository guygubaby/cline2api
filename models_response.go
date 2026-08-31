package main

import (
	"net/http"
	"time"
)

type modelLimits struct {
	input  int
	output int
}

var knownModelLimits = map[string]modelLimits{
	"deepseek/deepseek-v4-flash":   {input: 1_000_000, output: 384_000},
	"cline-pass/deepseek-v4-flash": {input: 1_000_000, output: 384_000},
	"z-ai/glm-5.3-flash":           {input: 1_000_000, output: 128_000},
	"cline-pass/glm-5.3-flash":     {input: 1_000_000, output: 128_000},
}

func resolvedModelLimits(model Model) (int, int) {
	input, output := model.Context, model.Output
	if known, ok := knownModelLimits[model.ID]; ok {
		if input == 0 {
			input = known.input
		}
		if output == 0 {
			output = known.output
		}
	}
	return input, output
}

func buildModelsResponse(models []Model, anthropic bool) map[string]any {
	data := make([]any, 0, len(models))
	for _, model := range models {
		if anthropic {
			maxInputTokens, maxTokens := resolvedModelLimits(model)
			var maxInputValue, maxTokensValue any
			if maxInputTokens > 0 {
				maxInputValue = maxInputTokens
			}
			if maxTokens > 0 {
				maxTokensValue = maxTokens
			}
			data = append(data, map[string]any{
				"id":               model.ID,
				"type":             "model",
				"display_name":     model.ID,
				"created_at":       time.Unix(0, 0).UTC().Format(time.RFC3339),
				"max_input_tokens": maxInputValue,
				"max_tokens":       maxTokensValue,
				"capabilities":     nil,
			})
			continue
		}

		ownedBy := "cline"
		if model.Source == "zen" || model.Provider == "opencode" {
			ownedBy = "opencode"
		}
		data = append(data, map[string]any{
			"id":       model.ID,
			"object":   "model",
			"created":  time.Now().Unix(),
			"owned_by": ownedBy,
		})
	}

	if !anthropic {
		return map[string]any{"object": "list", "data": data}
	}
	response := map[string]any{
		"data":     data,
		"has_more": false,
		"first_id": nil,
		"last_id":  nil,
	}
	if len(models) > 0 {
		response["first_id"] = models[0].ID
		response["last_id"] = models[len(models)-1].ID
	}
	return response
}

func isAnthropicModelsRequest(request *http.Request) bool {
	return request.Header.Get("anthropic-version") != ""
}
