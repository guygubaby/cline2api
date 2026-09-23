package main

import (
	"encoding/json"
	"testing"
)

func decodeTestMessages(t *testing.T, input string) []any {
	t.Helper()
	var messages []any
	if err := json.Unmarshal([]byte(input), &messages); err != nil {
		t.Fatal(err)
	}
	return messages
}

func TestSanitizeMessagesDropsMalformedCallsAndOrphanResults(t *testing.T) {
	messages := decodeTestMessages(t, `[
		{"role":"user","content":"hi"},
		{"role":"assistant","tool_calls":[
			{"id":"valid","type":"function","function":{"name":"read","arguments":"{}"}},
			{"id":"empty-name","type":"function","function":{"name":"","arguments":"{}"}},
			{"id":"","type":"function","function":{"name":"bash","arguments":"{}"}}
		]},
		{"role":"tool","tool_call_id":"valid","content":"ok"},
		{"role":"tool","tool_call_id":"empty-name","content":"bad"},
		{"role":"tool","tool_call_id":"missing","content":"orphan"}
	]`)
	cleaned := sanitizeMessages(messages)
	if len(cleaned) != 3 {
		t.Fatalf("messages = %d, want 3: %#v", len(cleaned), cleaned)
	}
	toolCalls, _ := cleaned[1].(map[string]any)["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(toolCalls))
	}
	if getNested(toolCalls[0].(map[string]any), "function", "name") != "read" {
		t.Fatalf("wrong tool call survived: %#v", toolCalls)
	}
}

func TestSanitizeMessagesRemovesEmptyToolCallsField(t *testing.T) {
	messages := decodeTestMessages(t, `[
		{"role":"assistant","content":"fallback","tool_calls":[
			{"id":"bad","type":"function","function":{"name":" ","arguments":"{}"}}
		]}
	]`)
	cleaned := sanitizeMessages(messages)
	message := cleaned[0].(map[string]any)
	if _, exists := message["tool_calls"]; exists {
		t.Fatalf("tool_calls was not removed: %#v", message)
	}
}

func TestOpenAIToAnthropicSkipsUnnamedToolCall(t *testing.T) {
	response := openAIToAnthropic(map[string]any{
		"model": "m1",
		"choices": []any{map[string]any{
			"finish_reason": "tool_calls",
			"message": map[string]any{
				"content": "fallback",
				"tool_calls": []any{map[string]any{
					"id": "bad", "function": map[string]any{"name": "", "arguments": "{}"},
				}},
			},
		}},
	})
	if response["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %v", response["stop_reason"])
	}
	if hasToolUseBlock(response["content"]) {
		t.Fatalf("malformed tool call leaked: %#v", response["content"])
	}
}

func TestChatToResponsesSkipsUnnamedToolCall(t *testing.T) {
	response := chatToResponses(map[string]any{
		"model": "m1",
		"choices": []any{map[string]any{
			"finish_reason": "tool_calls",
			"message": map[string]any{
				"content": "fallback",
				"tool_calls": []any{map[string]any{
					"id": "bad", "function": map[string]any{"name": "", "arguments": "{}"},
				}},
			},
		}},
	})
	outputs, _ := response["output"].([]any)
	for _, rawOutput := range outputs {
		output, _ := rawOutput.(map[string]any)
		if output["type"] == "function_call" {
			t.Fatalf("malformed function call leaked: %#v", output)
		}
	}
}

func TestNormalizeOpenAIChatResponseSkipsUnnamedToolCall(t *testing.T) {
	response := normalizeOpenAIChatResponse(map[string]any{
		"model": "m1",
		"choices": []any{map[string]any{
			"finish_reason": "tool_calls",
			"message": map[string]any{
				"role":    "assistant",
				"content": "fallback",
				"tool_calls": []any{map[string]any{
					"id": "bad", "type": "function",
					"function": map[string]any{"name": "", "arguments": "{}"},
				}},
			},
		}},
	}, "m1", false)
	message, _ := getNested(response, "choices", 0, "message").(map[string]any)
	if message["tool_calls"] != nil {
		t.Fatalf("malformed tool call leaked: %#v", message)
	}
	if getNested(response, "choices", 0, "finish_reason") != "stop" {
		t.Fatalf("finish_reason = %v", getNested(response, "choices", 0, "finish_reason"))
	}
}
