package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestBuildUpstreamBodyDisablesThinkingForSmallAuxiliaryCompletion(t *testing.T) {
	body := buildUpstreamBody(map[string]any{
		"model":      "deepseek/deepseek-v4-flash",
		"max_tokens": float64(64),
		"stream":     false,
		"messages": []any{
			map[string]any{"role": "user", "content": "Return one short title."},
		},
	}, true)

	thinking, _ := body["thinking"].(map[string]any)
	if thinking["type"] != "disabled" {
		t.Fatalf("small auxiliary completion should disable thinking: %#v", body)
	}
	if _, ok := body["reasoning_effort"]; ok {
		t.Fatalf("unspecified reasoning_effort should not be injected: %#v", body)
	}
}

func TestBuildUpstreamBodyPreservesConvertedIntegerTokenLimits(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		field string
		value any
		want  int
	}{
		{name: "Anthropic max_tokens", field: "max_tokens", value: 32, want: 32},
		{name: "Responses max_completion_tokens", field: "max_completion_tokens", value: int64(48), want: 48},
		{name: "JSON number", field: "max_tokens", value: json.Number("64"), want: 64},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			params := map[string]any{
				"model":    "deepseek/deepseek-v4-flash",
				"stream":   false,
				"messages": []any{map[string]any{"role": "user", "content": "hello"}},
			}
			params[testCase.field] = testCase.value
			body := buildUpstreamBody(params, true)

			if body["max_tokens"] != testCase.want {
				t.Fatalf("converted token limit = %#v, want %d", body["max_tokens"], testCase.want)
			}
			thinking, _ := body["thinking"].(map[string]any)
			if thinking["type"] != "disabled" {
				t.Fatalf("integer auxiliary limit did not activate the narrow thinking fallback: %#v", body)
			}
		})
	}
}

func TestShouldForceClineStreamOnlyForNonStreamingDeepSeek(t *testing.T) {
	if !shouldForceClineStream(map[string]any{"model": "deepseek/deepseek-v4-flash"}, false) {
		t.Fatal("non-streaming DeepSeek request should use the upstream stream fallback")
	}
	if shouldForceClineStream(map[string]any{"model": "deepseek/deepseek-v4-flash"}, true) {
		t.Fatal("client streaming request must use the normal streaming path")
	}
	if shouldForceClineStream(map[string]any{"model": "poolside/laguna-s-2.1:free"}, false) {
		t.Fatal("non-DeepSeek request must keep the existing non-streaming path")
	}
}

func TestAggregateChatCompletionStreamPreservesToolsUsageAndReasoning(t *testing.T) {
	fixture := strings.Join([]string{
		`data: {"data":{"id":"chat_1","model":"deepseek/deepseek-v4-flash","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"checked ","content":"done ","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"first","arguments":"{\"x\":"}},{"index":1,"id":"call_2","type":"function","function":{"name":"second","arguments":"{\"y\":"}}]}}]}}`,
		`data: {"data":{"choices":[{"index":0,"delta":{"content":"now","tool_calls":[{"index":0,"function":{"arguments":"1}"}},{"index":1,"function":{"arguments":"2}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}}`,
		`data: [DONE]`,
		``,
	}, "\n")

	out, err := aggregateChatCompletionStream(strings.NewReader(fixture))
	if err != nil {
		t.Fatalf("aggregate stream: %v", err)
	}
	message := getNested(out, "choices", 0, "message").(map[string]any)
	if message["content"] != "done now" || message["reasoning_content"] != "checked " {
		t.Fatalf("text or reasoning was lost: %#v", message)
	}
	calls := message["tool_calls"].([]any)
	if len(calls) != 2 {
		t.Fatalf("parallel tool calls = %d, want 2: %#v", len(calls), calls)
	}
	first := calls[0].(map[string]any)["function"].(map[string]any)
	second := calls[1].(map[string]any)["function"].(map[string]any)
	if first["arguments"] != `{"x":1}` || second["arguments"] != `{"y":2}` {
		t.Fatalf("fragmented tool arguments were lost: %#v", calls)
	}
	usage := parseTokenUsage(out["usage"])
	if !usage.Valid || usage.Prompt != 10 || usage.Completion != 5 || usage.Total != 15 {
		t.Fatalf("usage mapping = %#v", out["usage"])
	}
}

func TestAggregateChatCompletionStreamRejectsReasoningOnly(t *testing.T) {
	fixture := strings.Join([]string{
		`data: {"model":"deepseek/deepseek-v4-flash","choices":[{"index":0,"delta":{"reasoning_content":"hidden only"}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")

	_, err := aggregateChatCompletionStream(strings.NewReader(fixture))
	if !errors.Is(err, errEmptyResponseContent) {
		t.Fatalf("reasoning-only stream error = %v, want errEmptyResponseContent", err)
	}
}

func TestAggregateChatCompletionStreamReturnsSSEError(t *testing.T) {
	fixture := "data: {\"error\":{\"type\":\"api_error\",\"message\":\"upstream failed\"}}\n\n"
	_, err := aggregateChatCompletionStream(strings.NewReader(fixture))
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("SSE error was not surfaced: %v", err)
	}
}

func TestCallClineNonStreamRetriesOneAlternativeAfterEmptyStream(t *testing.T) {
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
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if body["stream"] != true {
			t.Fatalf("fallback request is not streamed: %#v", body)
		}
		thinking, _ := body["thinking"].(map[string]any)
		if thinking["type"] != "disabled" {
			t.Fatalf("small request lost thinking=disabled: %#v", body)
		}

		content := `data: {"model":"deepseek/deepseek-v4-flash","choices":[{"index":0,"delta":{"reasoning_content":"reasoning only"},"finish_reason":"length"}]}` + "\n\ndata: [DONE]\n\n"
		if requestCount == 2 {
			content = `data: {"model":"deepseek/deepseek-v4-flash","choices":[{"index":0,"delta":{"content":"visible answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}` + "\n\ndata: [DONE]\n\n"
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(content)),
			Request:    request,
		}, nil
	})}
	t.Cleanup(func() { httpClient = oldHTTPClient })

	out, account, err := callClineNonStream(map[string]any{
		"model":      "deepseek/deepseek-v4-flash",
		"max_tokens": float64(64),
		"stream":     false,
		"messages":   []any{map[string]any{"role": "user", "content": "Return a short title."}},
	})
	if err != nil {
		t.Fatalf("call Cline non-stream fallback: %v", err)
	}
	if requestCount != 2 {
		t.Fatalf("upstream requests = %d, want one initial request and one retry", requestCount)
	}
	if account != secondAccount {
		t.Fatalf("retry account = %#v, want the alternative account", account)
	}
	if content := getNested(out, "choices", 0, "message", "content"); content != "visible answer" {
		t.Fatalf("aggregated content = %#v", content)
	}
}

func TestBuildUpstreamBodyPreservesExplicitReasoning(t *testing.T) {
	body := buildUpstreamBody(map[string]any{
		"model":            "deepseek/deepseek-v4-flash",
		"max_tokens":       float64(64),
		"reasoning_effort": "high",
		"messages":         []any{map[string]any{"role": "user", "content": "Think carefully."}},
	}, false)

	if body["reasoning_effort"] != "high" {
		t.Fatalf("explicit reasoning_effort was not preserved: %#v", body)
	}
	if _, ok := body["thinking"]; ok {
		t.Fatalf("explicit reasoning request should not be disabled: %#v", body)
	}
}

func TestLoadOverrideContentKeepsEmptyFileSilent(t *testing.T) {
	tempDir := t.TempDir()
	if err := os.WriteFile(tempDir+"/override.md", nil, 0600); err != nil {
		t.Fatalf("write override: %v", err)
	}
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("change working directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(originalDir) })

	var logs bytes.Buffer
	originalWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(originalWriter) })

	if content := loadOverrideContent(); content != "" {
		t.Fatalf("empty override content = %q", content)
	}
	if strings.Contains(logs.String(), "override.md is empty") {
		t.Fatalf("empty optional override should be silent: %s", logs.String())
	}
}
