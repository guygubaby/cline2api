package main

import "testing"

func TestBuildAnthropicModelsResponseIncludesStandardContextLimits(t *testing.T) {
	response := buildModelsResponse([]Model{{
		ID: "deepseek/deepseek-v4-flash", Provider: "deepseek", Status: "active",
	}}, true)

	data := response["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("models = %d, want 1", len(data))
	}
	model := data[0].(map[string]any)
	if model["type"] != "model" || model["display_name"] != "deepseek/deepseek-v4-flash" {
		t.Fatalf("Anthropic model shape = %#v", model)
	}
	if model["max_input_tokens"] != 1_000_000 || model["max_tokens"] != 384_000 {
		t.Fatalf("context limits = %#v", model)
	}
	if response["has_more"] != false || response["first_id"] != model["id"] || response["last_id"] != model["id"] {
		t.Fatalf("Anthropic pagination envelope = %#v", response)
	}
}

func TestBuildOpenAIModelsResponseKeepsStandardBasicShape(t *testing.T) {
	response := buildModelsResponse([]Model{{
		ID: "deepseek/deepseek-v4-flash", Provider: "deepseek", Status: "active",
	}}, false)

	if response["object"] != "list" {
		t.Fatalf("OpenAI models envelope = %#v", response)
	}
	data := response["data"].([]any)
	model := data[0].(map[string]any)
	if _, exists := model["max_input_tokens"]; exists {
		t.Fatalf("non-standard max_input_tokens leaked into OpenAI model: %#v", model)
	}
	if _, exists := model["max_tokens"]; exists {
		t.Fatalf("non-standard max_tokens leaked into OpenAI model: %#v", model)
	}
}
