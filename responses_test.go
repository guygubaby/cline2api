package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

func TestResponsesNamespaceToolsFlattenToChatFunctions(t *testing.T) {
	request := map[string]any{
		"model": "m1",
		"input": "hello",
		"tools": []any{map[string]any{
			"type": "namespace", "name": "multi_agent_v1", "description": "Manage agents",
			"tools": []any{
				map[string]any{
					"type": "function", "name": "spawn_agent", "description": "Spawn an agent",
					"parameters": map[string]any{"type": "object"},
				},
				map[string]any{
					"type": "function", "name": "wait_agent", "description": "Wait for an agent",
					"parameters": map[string]any{"type": "object"},
				},
			},
		}, map[string]any{"type": "web_search"}},
	}
	if err := validateResponsesCompatibility(request); err != nil {
		t.Fatalf("Codex namespace tool was rejected: %v", err)
	}
	out := responsesToChat(request)
	tools := out["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("flattened namespace tools = %#v", tools)
	}
	first := tools[0].(map[string]any)["function"].(map[string]any)
	if first["name"] != "spawn_agent" || first["parameters"] == nil {
		t.Fatalf("flattened namespace function = %#v", first)
	}
}

func TestResponsesCustomToolMapsToChatFunction(t *testing.T) {
	request := map[string]any{
		"model": "m1",
		"input": "update the file",
		"tools": []any{map[string]any{
			"type": "custom", "name": "apply_patch", "description": "Apply a patch",
			"format": map[string]any{"type": "text"},
		}},
	}
	if err := validateResponsesCompatibility(request); err != nil {
		t.Fatalf("Codex custom tool was rejected: %v", err)
	}
	out := responsesToChat(request)
	tools := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("custom tool mapping = %#v", tools)
	}
	function := tools[0].(map[string]any)["function"].(map[string]any)
	parameters := function["parameters"].(map[string]any)
	properties := parameters["properties"].(map[string]any)
	if function["name"] != "apply_patch" || properties["input"] == nil {
		t.Fatalf("custom tool function mapping = %#v", function)
	}
}

func TestResponsesCustomToolHistoryRoundTripsToChat(t *testing.T) {
	out := responsesToChat(map[string]any{
		"model": "m1",
		"input": []any{
			map[string]any{
				"type": "custom_tool_call", "call_id": "call_patch", "name": "apply_patch",
				"input": "*** Begin Patch\n*** End Patch",
			},
			map[string]any{
				"type": "custom_tool_call_output", "call_id": "call_patch", "output": "Done!",
			},
		},
	})
	messages := out["messages"].([]any)
	assistant := messages[0].(map[string]any)
	call := assistant["tool_calls"].([]any)[0].(map[string]any)
	arguments := call["function"].(map[string]any)["arguments"]
	if arguments != `{"input":"*** Begin Patch\n*** End Patch"}` {
		t.Fatalf("custom tool input mapping = %#v", arguments)
	}
	if messages[1].(map[string]any)["tool_call_id"] != "call_patch" {
		t.Fatalf("custom tool output mapping = %#v", messages)
	}
}

func TestChatStreamToResponsesEmitsCustomToolCall(t *testing.T) {
	upstreamBody := strings.Join([]string{
		`data: {"model":"m1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_patch","function":{"name":"apply_patch","arguments":"{\"input\":\"*** Begin"}}]}}]}`,
		`data: {"model":"m1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":" Patch\\n*** End Patch\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		`data: [DONE]`,
		``,
	}, "\n")
	upstream := &http.Response{Body: io.NopCloser(strings.NewReader(upstreamBody))}
	recorder := httptest.NewRecorder()
	request := map[string]any{
		"tools": []any{map[string]any{"type": "custom", "name": "apply_patch"}},
	}

	chatStreamToResponses(recorder, upstream, nil, nil, request)
	events := decodeSSEEvents(t, recorder.Body.String())
	done := firstEventOfType(t, events, "response.custom_tool_call_input.done")
	if done["input"] != "*** Begin Patch\n*** End Patch" {
		t.Fatalf("custom tool input done = %#v", done)
	}
	completed := firstEventOfType(t, events, "response.completed")
	outputs := completed["response"].(map[string]any)["output"].([]any)
	if len(outputs) != 1 || outputs[0].(map[string]any)["type"] != "custom_tool_call" {
		t.Fatalf("custom tool output item = %#v", outputs)
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

func TestChatToResponsesIncludesReasoningBeforeMessage(t *testing.T) {
	response := chatToResponses(map[string]any{
		"model": "m1",
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role": "assistant", "reasoning_content": "reason first", "content": "answer",
			},
			"finish_reason": "stop",
		}},
	})
	outputs := response["output"].([]any)
	if len(outputs) != 2 || outputs[0].(map[string]any)["type"] != "reasoning" || outputs[1].(map[string]any)["type"] != "message" {
		t.Fatalf("non-stream reasoning/message output = %#v", outputs)
	}
}

func TestFinalizeResponsesNonStreamLogMarksIncomplete(t *testing.T) {
	chat := map[string]any{
		"model": "m1",
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": "truncated"},
			"finish_reason": "length",
		}},
	}
	response := chatToResponses(chat)
	reqLog := &RequestLog{StartedAt: time.Now(), Protocol: "responses", Model: "m1"}

	finalizeResponsesNonStreamLog(reqLog, chat, response, tokenUsage{})

	if reqLog.Completed || reqLog.FinishReason != "length" || reqLog.ErrorCode != "max_output_tokens" {
		t.Fatalf("non-stream incomplete request log = %#v", reqLog)
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
	reqLog := &RequestLog{StartedAt: time.Now(), Protocol: "responses", Model: "m1", Stream: true}

	chatStreamToResponses(recorder, upstream, reqLog, nil)

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

func TestChatStreamToResponsesFailsOnEarlyEOF(t *testing.T) {
	upstream := &http.Response{Body: io.NopCloser(strings.NewReader(
		"data: {\"model\":\"m1\",\"choices\":[{\"delta\":{\"content\":\"partial output\"}}]}\n\n",
	))}
	recorder := httptest.NewRecorder()
	reqLog := &RequestLog{StartedAt: time.Now(), Protocol: "responses", Model: "m1", Stream: true}

	chatStreamToResponses(recorder, upstream, reqLog, nil)
	events := decodeSSEEvents(t, recorder.Body.String())
	if len(eventsOfType(events, "response.completed")) != 0 {
		t.Fatalf("premature EOF was incorrectly reported as completed: %s", recorder.Body.String())
	}
	failed := firstEventOfType(t, events, "response.failed")
	response := failed["response"].(map[string]any)
	errorBody := response["error"].(map[string]any)
	if errorBody["code"] != "stream_early_eof" {
		t.Fatalf("early EOF error = %#v", errorBody)
	}
	if reqLog.Completed || reqLog.ErrorCode != "stream_early_eof" || reqLog.SawDone {
		t.Fatalf("early EOF request log = %#v", reqLog)
	}
}

func TestChatStreamToResponsesForwardsReasoningSummary(t *testing.T) {
	upstreamBody := strings.Join([]string{
		`data: {"model":"m1","choices":[{"delta":{"reasoning_content":"reasoning now"}}]}`,
		`data: {"model":"m1","choices":[{"delta":{"content":"answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"completion_tokens_details":{"reasoning_tokens":3}}}`,
		`data: [DONE]`,
		``,
	}, "\n")
	upstream := &http.Response{Body: io.NopCloser(strings.NewReader(upstreamBody))}
	recorder := httptest.NewRecorder()
	reqLog := &RequestLog{StartedAt: time.Now(), Protocol: "responses", Model: "m1", Stream: true}

	chatStreamToResponses(recorder, upstream, reqLog, nil)
	events := decodeSSEEvents(t, recorder.Body.String())
	deltas := eventsOfType(events, "response.reasoning_summary_text.delta")
	if len(deltas) != 1 || deltas[0]["delta"] != "reasoning now" {
		t.Fatalf("reasoning progress missing: %#v", deltas)
	}
	completed := firstEventOfType(t, events, "response.completed")
	response := completed["response"].(map[string]any)
	outputs := response["output"].([]any)
	if len(outputs) != 2 || outputs[0].(map[string]any)["type"] != "reasoning" || outputs[1].(map[string]any)["type"] != "message" {
		t.Fatalf("reasoning/message output order = %#v", outputs)
	}
}

func TestChatStreamToResponsesMarksLengthIncomplete(t *testing.T) {
	upstreamBody := strings.Join([]string{
		`data: {"model":"m1","choices":[{"delta":{"content":"truncated"}}]}`,
		`data: {"model":"m1","choices":[{"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		`data: [DONE]`,
		``,
	}, "\n")
	upstream := &http.Response{Body: io.NopCloser(strings.NewReader(upstreamBody))}
	recorder := httptest.NewRecorder()
	reqLog := &RequestLog{StartedAt: time.Now(), Protocol: "responses", Model: "m1", Stream: true}

	chatStreamToResponses(recorder, upstream, reqLog, nil)
	events := decodeSSEEvents(t, recorder.Body.String())
	if len(eventsOfType(events, "response.completed")) != 0 {
		t.Fatalf("length-limited stream was incorrectly completed: %s", recorder.Body.String())
	}
	incomplete := firstEventOfType(t, events, "response.incomplete")
	response := incomplete["response"].(map[string]any)
	details := response["incomplete_details"].(map[string]any)
	if details["reason"] != "max_output_tokens" {
		t.Fatalf("incomplete details = %#v", details)
	}
	if reqLog.Completed || reqLog.ErrorCode != "max_output_tokens" || reqLog.FinishReason != "length" || !reqLog.SawDone {
		t.Fatalf("incomplete request log = %#v", reqLog)
	}
}

func TestResponsesReasoningHistoryRoundTripsToChat(t *testing.T) {
	out := responsesToChat(map[string]any{
		"model": "deepseek/deepseek-v4-flash",
		"input": []any{
			map[string]any{
				"type": "reasoning",
				"id":   "rs_1",
				"summary": []any{
					map[string]any{"type": "summary_text", "text": "inspect before calling the tool"},
				},
			},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "Read", "arguments": `{"path":"README.md"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "contents"},
		},
	})
	messages := out["messages"].([]any)
	assistant := messages[0].(map[string]any)
	if assistant["reasoning_content"] != "inspect before calling the tool" {
		t.Fatalf("Responses reasoning history was dropped: %#v", assistant)
	}
}

func TestResponsesStreamRetriesInitializationFailureBeforeCommitting(t *testing.T) {
	firstAccount := &Account{
		AccountID: "responses-first", Email: "first@example.com", AccessToken: "workos:first",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active", ModelCooldowns: map[string]time.Time{},
	}
	secondAccount := &Account{
		AccountID: "responses-second", Email: "second@example.com", AccessToken: "workos:second",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active", ModelCooldowns: map[string]time.Time{},
	}
	currentPool := loadPool()
	poolMu.Lock()
	oldAccounts, oldIndex := currentPool.Accounts, currentPool.CurrentIdx
	currentPool.Accounts = []*Account{firstAccount, secondAccount}
	currentPool.CurrentIdx = 0
	poolMu.Unlock()
	t.Cleanup(func() {
		poolMu.Lock()
		currentPool.Accounts = oldAccounts
		currentPool.CurrentIdx = oldIndex
		poolMu.Unlock()
		savePool()
	})

	oldHTTPClient := httpClient
	requestCount := 0
	httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestCount++
		statusCode := http.StatusTooManyRequests
		body := `{"error":{"code":"INFERENCE_CAP_ERROR","message":"provider rate limited"}}`
		contentType := "application/json"
		if requestCount == 2 {
			statusCode = http.StatusOK
			contentType = "text/event-stream"
			body = strings.Join([]string{
				`data: {"model":"deepseek/deepseek-v4-flash","choices":[{"delta":{"reasoning_content":"retry reasoning"}}]}`,
				`data: {"model":"deepseek/deepseek-v4-flash","choices":[{"delta":{"content":"recovered"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
				`data: [DONE]`,
				``,
			}, "\n")
		}
		return &http.Response{
			StatusCode: statusCode,
			Header:     http.Header{"Content-Type": []string{contentType}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})}
	t.Cleanup(func() { httpClient = oldHTTPClient })

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"deepseek/deepseek-v4-flash",
		"input":"hello",
		"stream":true,
		"reasoning":{"effort":"high","summary":"auto"}
	}`))
	handleResponses(recorder, request)

	if requestCount != 2 {
		t.Fatalf("initialization retry requests = %d, want 2", requestCount)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	events := decodeSSEEvents(t, recorder.Body.String())
	if len(eventsOfType(events, "response.failed")) != 0 {
		t.Fatalf("recoverable initialization failure leaked to client: %s", recorder.Body.String())
	}
	firstEventOfType(t, events, "response.completed")
	if len(eventsOfType(events, "response.reasoning_summary_text.delta")) == 0 {
		t.Fatalf("retry reasoning was not forwarded: %s", recorder.Body.String())
	}
	page, err := listRequestLogs(1, "")
	if err != nil || len(page.Items) != 1 || page.Items[0].RetryCount != 1 {
		t.Fatalf("Responses retry metadata missing: page=%#v err=%v", page, err)
	}
}

func TestResponsesStreamMapsRepeatedInitializationRateLimit(t *testing.T) {
	firstAccount := &Account{
		AccountID: "responses-rate-first", Email: "first@example.com", AccessToken: "workos:first",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active", ModelCooldowns: map[string]time.Time{},
	}
	secondAccount := &Account{
		AccountID: "responses-rate-second", Email: "second@example.com", AccessToken: "workos:second",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active", ModelCooldowns: map[string]time.Time{},
	}
	currentPool := loadPool()
	poolMu.Lock()
	oldAccounts, oldIndex := currentPool.Accounts, currentPool.CurrentIdx
	currentPool.Accounts = []*Account{firstAccount, secondAccount}
	currentPool.CurrentIdx = 0
	poolMu.Unlock()
	t.Cleanup(func() {
		poolMu.Lock()
		currentPool.Accounts = oldAccounts
		currentPool.CurrentIdx = oldIndex
		poolMu.Unlock()
		savePool()
	})

	oldHTTPClient := httpClient
	requestCount := 0
	httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestCount++
		body := "data: {\"error\":{\"code\":\"stream_initialization_failed\",\"message\":\"provider failed with status 429\"}}\n\ndata: [DONE]\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})}
	t.Cleanup(func() { httpClient = oldHTTPClient })

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"deepseek/deepseek-v4-flash","input":"hello","stream":true
	}`))
	handleResponses(recorder, request)

	if requestCount != 2 {
		t.Fatalf("rate-limit attempts = %d, want 2", requestCount)
	}
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body=%s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode rate-limit response: %v", err)
	}
	errorBody := response["error"].(map[string]any)
	if errorBody["type"] != "rate_limit_error" {
		t.Fatalf("rate-limit error = %#v", errorBody)
	}
}

func TestUsageToResponsesPreservesCacheWriteAndReasoningDetails(t *testing.T) {
	parsed := parseTokenUsage(map[string]any{
		"prompt_tokens":     float64(100),
		"completion_tokens": float64(20),
		"total_tokens":      float64(120),
		"prompt_tokens_details": map[string]any{
			"cached_tokens":      float64(30),
			"cache_write_tokens": float64(5),
		},
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": float64(12),
		},
	})

	usage := usageToResponses(parsed)
	inputDetails := usage["input_tokens_details"].(map[string]any)
	outputDetails := usage["output_tokens_details"].(map[string]any)
	if inputDetails["cached_tokens"] != int64(30) || inputDetails["cache_write_tokens"] != int64(5) {
		t.Fatalf("input details = %#v", inputDetails)
	}
	if outputDetails["reasoning_tokens"] != int64(12) {
		t.Fatalf("output details = %#v", outputDetails)
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
