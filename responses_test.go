package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponsesToChatMapsCurrentCoreRequest(t *testing.T) {
	out := responsesToChat(map[string]any{
		"model": "m1",
		"input": []any{map[string]any{
			"type": "message", "role": "user",
			"content": []any{
				map[string]any{"type": "input_text", "text": "describe"},
				map[string]any{"type": "input_image", "image_url": "https://example.com/a.png", "detail": "high"},
			},
		}},
		"reasoning": map[string]any{"effort": "high"},
		"text": map[string]any{"format": map[string]any{
			"type": "json_schema", "name": "result", "schema": map[string]any{"type": "object"}, "strict": true,
		}},
		"tool_choice": map[string]any{"type": "function", "name": "inspect"},
		"tools": []any{map[string]any{
			"type": "function", "name": "inspect", "description": "Inspect an image",
			"parameters": map[string]any{"type": "object"}, "strict": true,
		}},
	})

	messages := out["messages"].([]any)
	content, ok := messages[0].(map[string]any)["content"].([]any)
	if !ok || len(content) != 2 || content[1].(map[string]any)["type"] != "image_url" {
		t.Fatalf("Responses multimodal content mapping = %#v", messages)
	}
	if out["reasoning_effort"] != "high" {
		t.Fatalf("reasoning.effort mapping = %#v", out["reasoning_effort"])
	}
	format := out["response_format"].(map[string]any)
	if format["type"] != "json_schema" {
		t.Fatalf("text.format mapping = %#v", format)
	}
	choice := out["tool_choice"].(map[string]any)
	if choice["type"] != "function" || choice["function"].(map[string]any)["name"] != "inspect" {
		t.Fatalf("tool_choice mapping = %#v", choice)
	}
	function := out["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if function["strict"] != true {
		t.Fatalf("strict tool flag was lost: %#v", function)
	}
}

func TestResponsesToChatGroupsParallelFunctionCalls(t *testing.T) {
	out := responsesToChat(map[string]any{
		"model": "m1",
		"input": []any{
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "a", "arguments": `{}`},
			map[string]any{"type": "function_call", "call_id": "call_2", "name": "b", "arguments": `{}`},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "one"},
			map[string]any{"type": "function_call_output", "call_id": "call_2", "output": "two"},
		},
	})
	messages := out["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("want one assistant message and two tool results, got %#v", messages)
	}
	if calls := messages[0].(map[string]any)["tool_calls"].([]any); len(calls) != 2 {
		t.Fatalf("parallel function calls were not grouped: %#v", messages[0])
	}
}

func TestChatToResponsesToolOnlyHasNoEmptyMessage(t *testing.T) {
	response := chatToResponses(map[string]any{
		"model": "m1",
		"choices": []any{map[string]any{
			"message": map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "inspect", "arguments": `{}`}},
			}},
			"finish_reason": "tool_calls",
		}},
	})
	outputs := response["output"].([]any)
	if len(outputs) != 1 || outputs[0].(map[string]any)["type"] != "function_call" {
		t.Fatalf("tool-only Responses output contains a synthetic message: %#v", outputs)
	}
	for _, key := range []string{"error", "incomplete_details", "parallel_tool_calls", "tool_choice", "tools"} {
		if _, ok := response[key]; !ok {
			t.Fatalf("standard response field %q missing: %#v", key, response)
		}
	}
}

func TestChatStreamToResponsesEmitsStandardLifecycleAndParallelTools(t *testing.T) {
	upstreamBody := strings.Join([]string{
		`data: {"model":"m1","choices":[{"delta":{"content":"Checking "}}]}`,
		`data: {"model":"m1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"first","arguments":"{\"x\":"}},{"index":1,"id":"call_2","function":{"name":"second","arguments":"{\"y\":"}}]}}]}`,
		`data: {"model":"m1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}},{"index":1,"function":{"arguments":"2}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		`data: [DONE]`,
		``,
	}, "\n")
	upstream := &http.Response{Body: io.NopCloser(strings.NewReader(upstreamBody))}
	recorder := httptest.NewRecorder()

	chatStreamToResponses(recorder, upstream, nil, nil)

	events := decodeSSEEvents(t, recorder.Body.String())
	created := firstEventOfType(t, events, "response.created")
	if _, ok := created["response"].(map[string]any); !ok {
		t.Fatalf("response.created has no response snapshot: %#v", created)
	}
	for _, event := range events {
		if _, ok := event["sequence_number"]; !ok {
			t.Fatalf("sequence_number missing from event: %#v", event)
		}
	}
	deltas := eventsOfType(events, "response.function_call_arguments.delta")
	if len(deltas) != 4 {
		t.Fatalf("want four function argument fragments, got %d: %#v", len(deltas), deltas)
	}
	completed := firstEventOfType(t, events, "response.completed")
	response := completed["response"].(map[string]any)
	outputs := response["output"].([]any)
	if len(outputs) != 3 {
		t.Fatalf("completed response should contain text + two calls: %#v", outputs)
	}
}

func decodeSSEEvents(t *testing.T, body string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, block := range strings.Split(strings.TrimSpace(body), "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var event map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
				t.Fatalf("decode SSE event: %v\n%s", err, line)
			}
			events = append(events, event)
		}
	}
	return events
}

func firstEventOfType(t *testing.T, events []map[string]any, eventType string) map[string]any {
	t.Helper()
	for _, event := range events {
		if event["type"] == eventType {
			return event
		}
	}
	t.Fatalf("event %q not found in %#v", eventType, events)
	return nil
}

func eventsOfType(events []map[string]any, eventType string) []map[string]any {
	var matched []map[string]any
	for _, event := range events {
		if event["type"] == eventType {
			matched = append(matched, event)
		}
	}
	return matched
}
