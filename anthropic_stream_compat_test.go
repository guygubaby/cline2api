package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHandleAnthropicStreamPreservesReasoningBeforeToolUse(t *testing.T) {
	upstreamBody := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"I should inspect the project."}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Read","arguments":"{\"file_path\":\"README.md\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":12,"total_tokens":32,"completion_tokens_details":{"reasoning_tokens":7}}}`,
		`data: [DONE]`,
		``,
	}, "\n")
	upstream := &http.Response{Body: io.NopCloser(strings.NewReader(upstreamBody))}
	recorder := httptest.NewRecorder()
	reqLog := &RequestLog{StartedAt: time.Now(), Protocol: "anthropic", Model: "m1", Stream: true}

	handleAnthropicStream(recorder, upstream, nil, reqLog, 20)

	events := decodeSSEEvents(t, recorder.Body.String())
	thinkingStart := firstContentBlockStartOfType(t, events, "thinking")
	thinkingIndex := thinkingStart["index"]
	if thinkingStart["content_block"].(map[string]any)["signature"] != "" {
		t.Fatalf("thinking block must start with an empty signature: %#v", thinkingStart)
	}
	thinkingDelta := firstEventOfType(t, events, "content_block_delta")
	if thinkingDelta["index"] != thinkingIndex || thinkingDelta["delta"].(map[string]any)["type"] != "thinking_delta" {
		t.Fatalf("thinking delta = %#v", thinkingDelta)
	}
	signatureDelta := firstDeltaOfType(t, events, "signature_delta")
	if signature, _ := signatureDelta["delta"].(map[string]any)["signature"].(string); signature == "" {
		t.Fatalf("thinking signature was not finalized: %#v", signatureDelta)
	}
	toolStart := firstContentBlockStartOfType(t, events, "tool_use")
	if toolStart["index"].(float64) <= thinkingIndex.(float64) {
		t.Fatalf("tool block must follow the thinking block: thinking=%#v tool=%#v", thinkingStart, toolStart)
	}
}

func TestAnthropicThinkingToolHistoryRoundTripsToReasoningContent(t *testing.T) {
	raw := `{
		"model":"m1",
		"max_tokens":1024,
		"messages":[{"role":"assistant","content":[
			{"type":"thinking","thinking":"inspect first","signature":"proxy_signature"},
			{"type":"tool_use","id":"call_1","name":"Read","input":{"file_path":"README.md"}}
		]}]
	}`
	var request anthropicReq
	if err := json.Unmarshal([]byte(raw), &request); err != nil {
		t.Fatalf("decode request: %v", err)
	}

	converted := anthropicToOpenAI(request)
	messages := converted["messages"].([]any)
	assistant := messages[0].(map[string]any)
	if assistant["reasoning_content"] != "inspect first" {
		t.Fatalf("reasoning history was not restored: %#v", assistant)
	}
	if calls := assistant["tool_calls"].([]any); len(calls) != 1 {
		t.Fatalf("tool history was not restored: %#v", assistant)
	}
}

func TestOpenAIToAnthropicIncludesThinkingBeforeToolUse(t *testing.T) {
	response := openAIToAnthropic(map[string]any{
		"model": "m1",
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role":              "assistant",
				"content":           "",
				"reasoning_content": "inspect first",
				"tool_calls": []any{map[string]any{
					"id": "call_1", "type": "function",
					"function": map[string]any{"name": "Read", "arguments": `{"file_path":"README.md"}`},
				}},
			},
			"finish_reason": "tool_calls",
		}},
	})

	content := response["content"].([]any)
	if len(content) != 2 || content[0].(map[string]any)["type"] != "thinking" || content[1].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("thinking/tool block order = %#v", content)
	}
	if signature, _ := content[0].(map[string]any)["signature"].(string); signature == "" {
		t.Fatalf("thinking signature missing: %#v", content[0])
	}
}

func TestPrepareSemanticChatStreamRejectsReasoningOnlyOutput(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"thinking only"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":1,"total_tokens":101,"completion_tokens_details":{"reasoning_tokens":0}}}`,
		`data: [DONE]`,
		``,
	}, "\n")
	response := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}

	_, diagnostic, err := prepareSemanticChatStream(response)
	if !errors.Is(err, errEmptyResponseContent) {
		t.Fatalf("reasoning-only stream error = %v", err)
	}
	if diagnostic.ReasoningChars != len("thinking only") || diagnostic.FinishReason != "stop" || diagnostic.Usage.Completion != 1 {
		t.Fatalf("empty-stream diagnostic = %#v", diagnostic)
	}
}

func TestPrepareSemanticChatStreamReplaysBufferedReasoningAndText(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"think"}}]}`,
		`data: {"choices":[{"delta":{"content":"answer"}}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	response := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}

	prepared, _, err := prepareSemanticChatStream(response)
	if err != nil {
		t.Fatalf("prepare semantic stream: %v", err)
	}
	replayed, err := io.ReadAll(prepared.Body)
	if err != nil {
		t.Fatalf("read replayed stream: %v", err)
	}
	if string(replayed) != body {
		t.Fatalf("replayed stream differs:\nwant: %q\n got: %q", body, replayed)
	}
}

func TestCallClineAnthropicStreamRetriesOneAlternativeAfterEmptyOutput(t *testing.T) {
	firstAccount := &Account{
		AccountID: "first", Email: "first@example.com", AccessToken: "workos:first",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active", ModelCooldowns: map[string]time.Time{},
	}
	secondAccount := &Account{
		AccountID: "second", Email: "second@example.com", AccessToken: "workos:second",
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
		body := strings.Join([]string{
			`data: {"choices":[{"delta":{"reasoning_content":"thinking only"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":1,"total_tokens":101}}`,
			`data: [DONE]`,
			``,
		}, "\n")
		if requestCount == 2 {
			body = strings.Join([]string{
				`data: {"choices":[{"delta":{"reasoning_content":"inspect first"}}]}`,
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Read","arguments":"{\"file_path\":\"README.md\"}"}}]},"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
				``,
			}, "\n")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})}
	t.Cleanup(func() { httpClient = oldHTTPClient })

	prepared, account, diagnostic, err := callClineAnthropicStream(map[string]any{
		"model":      "deepseek/deepseek-v4-flash",
		"max_tokens": float64(128000),
		"stream":     true,
		"messages":   []any{map[string]any{"role": "user", "content": "Inspect the project."}},
	})
	if err != nil {
		t.Fatalf("call Anthropic stream: %v", err)
	}
	defer prepared.Body.Close()
	if requestCount != 2 {
		t.Fatalf("upstream requests = %d, want 2", requestCount)
	}
	if account != secondAccount {
		t.Fatalf("retry account = %#v, want second account", account)
	}
	if diagnostic.ReasoningChars != len("inspect first") {
		t.Fatalf("retry diagnostic = %#v", diagnostic)
	}
	replayed, err := io.ReadAll(prepared.Body)
	if err != nil {
		t.Fatalf("read prepared stream: %v", err)
	}
	if !strings.Contains(string(replayed), `"name":"Read"`) {
		t.Fatalf("prepared retry stream lost the tool call: %s", replayed)
	}
}

func firstContentBlockStartOfType(t *testing.T, events []map[string]any, blockType string) map[string]any {
	t.Helper()
	for _, event := range events {
		if event["type"] != "content_block_start" {
			continue
		}
		block, _ := event["content_block"].(map[string]any)
		if block["type"] == blockType {
			return event
		}
	}
	t.Fatalf("content_block_start %q not found: %#v", blockType, events)
	return nil
}

func firstDeltaOfType(t *testing.T, events []map[string]any, deltaType string) map[string]any {
	t.Helper()
	for _, event := range events {
		if event["type"] != "content_block_delta" {
			continue
		}
		delta, _ := event["delta"].(map[string]any)
		if delta["type"] == deltaType {
			return event
		}
	}
	t.Fatalf("content_block_delta %q not found: %#v", deltaType, events)
	return nil
}
