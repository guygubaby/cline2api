package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
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
	}, true, "test-session")

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
			body := buildUpstreamBody(params, true, "test-session")

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
	if !shouldForceClineStream(map[string]any{"model": virtualFreeModel}, false) {
		t.Fatal("virtual free requests should aggregate a reliable upstream stream")
	}
	if !shouldForceClineStream(map[string]any{"model": "z-ai/glm-5.3-flash"}, false) {
		t.Fatal("GLM 5.3 requests should aggregate a reliable upstream stream")
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
	}, false, "test-session")

	if body["reasoning_effort"] != "high" {
		t.Fatalf("explicit reasoning_effort was not preserved: %#v", body)
	}
	if _, ok := body["thinking"]; ok {
		t.Fatalf("explicit reasoning request should not be disabled: %#v", body)
	}
}

func TestHandleChatStreamMarksEarlyEOFFailed(t *testing.T) {
	upstream := &http.Response{Body: io.NopCloser(strings.NewReader(
		"data: {\"model\":\"m1\",\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n",
	))}
	recorder := httptest.NewRecorder()
	reqLog := &RequestLog{StartedAt: time.Now(), Protocol: "openai", Model: "m1", Stream: true}

	handleStreamResponse(recorder, upstream, nil, reqLog, false)

	if reqLog.Completed || reqLog.ErrorCode != "stream_early_eof" || reqLog.SawDone {
		t.Fatalf("early EOF request log = %#v", reqLog)
	}
	if !strings.Contains(recorder.Body.String(), `"code":"stream_early_eof"`) {
		t.Fatalf("client did not receive early EOF error: %s", recorder.Body.String())
	}
}

func TestHandleChatStreamRecordsTerminalReason(t *testing.T) {
	upstreamBody := strings.Join([]string{
		`data: {"model":"m1","choices":[{"delta":{"content":"done"}}]}`,
		`data: {"model":"m1","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`,
		`data: [DONE]`,
		``,
	}, "\n")
	upstream := &http.Response{Body: io.NopCloser(strings.NewReader(upstreamBody))}
	recorder := httptest.NewRecorder()
	reqLog := &RequestLog{StartedAt: time.Now().Add(-time.Second), Protocol: "openai", Model: "m1", Stream: true}

	handleStreamResponse(recorder, upstream, nil, reqLog, false)

	if !reqLog.Completed || reqLog.FinishReason != "stop" || !reqLog.SawDone || reqLog.ErrorCode != "" {
		t.Fatalf("terminal request log = %#v", reqLog)
	}
	if reqLog.UpstreamTTFTMs <= 0 || reqLog.VisibleTTFTMs <= 0 || reqLog.ThinkingTTFTMs != 0 {
		t.Fatalf("chat latency phases = %#v", reqLog)
	}
}

func TestHandleChatStreamStopsRunawayRepeatedText(t *testing.T) {
	upstream := &http.Response{Body: io.NopCloser(strings.NewReader(repetitiveChatSSE(t, 64)))}
	recorder := httptest.NewRecorder()
	reqLog := &RequestLog{StartedAt: time.Now(), Protocol: "openai", Model: "m1", Stream: true}

	handleStreamResponse(recorder, upstream, nil, reqLog, false)

	if reqLog.Completed || reqLog.ErrorCode != repetitiveOutputErrorCode {
		t.Fatalf("runaway Chat stream log = %#v", reqLog)
	}
	if !strings.Contains(recorder.Body.String(), `"code":"repetitive_output"`) || strings.Contains(recorder.Body.String(), "data: [DONE]") {
		t.Fatalf("runaway Chat stream did not fail safely: %s", recorder.Body.String())
	}
}

func TestNormalizeOpenAIChatResponseUsesStandardFields(t *testing.T) {
	response := normalizeOpenAIChatResponse(map[string]any{
		"id":                "chatcmpl_1",
		"object":            "vendor.chunk",
		"created":           float64(123),
		"model":             "m1",
		"provider_metadata": map[string]any{"secret": true},
		"choices": []any{map[string]any{
			"index": float64(0), "finish_reason": nil,
			"delta": map[string]any{
				"role": "assistant", "content": "hello", "reasoning_content": "hidden", "vendor": "drop",
			},
		}},
		"usage": map[string]any{
			"prompt_tokens": float64(10), "completion_tokens": float64(5), "total_tokens": float64(15),
			"cost": float64(0.1), "is_byok": false,
			"completion_tokens_details": map[string]any{"reasoning_tokens": float64(3), "vendor": float64(1)},
		},
	}, "m1", true)

	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("encode normalized response: %v", err)
	}
	for _, forbidden := range []string{"reasoning_content", "provider_metadata", `"vendor"`, `"cost"`, "is_byok"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("non-standard field %q leaked: %s", forbidden, encoded)
		}
	}
	if response["object"] != "chat.completion.chunk" {
		t.Fatalf("stream object = %#v", response["object"])
	}
	if reasoning := getNested(response, "usage", "completion_tokens_details", "reasoning_tokens"); reasoning != float64(3) {
		t.Fatalf("standard reasoning token usage was lost: %#v", response["usage"])
	}

	nonStream := normalizeOpenAIChatResponse(map[string]any{
		"choices": []any{map[string]any{
			"index": float64(0), "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": "answer", "reasoning_content": "hidden"},
		}},
	}, "m1", false)
	encoded, _ = json.Marshal(nonStream)
	if nonStream["object"] != "chat.completion" || strings.Contains(string(encoded), "reasoning_content") {
		t.Fatalf("non-stream response is not standard Chat Completions: %s", encoded)
	}
}

func TestHandleChatStreamEmitsStandardUsageChunk(t *testing.T) {
	upstreamBody := strings.Join([]string{
		`data: {"id":"chatcmpl_1","object":"vendor.chunk","created":123,"model":"m1","choices":[{"index":0,"delta":{"reasoning_content":"hidden reasoning"}}]}`,
		`data: {"id":"chatcmpl_1","created":123,"model":"m1","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"}}]}`,
		`data: {"id":"chatcmpl_1","created":123,"model":"m1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"cost":0.1,"completion_tokens_details":{"reasoning_tokens":3}}}`,
		`data: [DONE]`,
		``,
	}, "\n")
	upstream := &http.Response{Body: io.NopCloser(strings.NewReader(upstreamBody))}
	recorder := httptest.NewRecorder()
	reqLog := &RequestLog{StartedAt: time.Now().Add(-time.Second), Protocol: "openai", Model: "m1", Stream: true}

	handleStreamResponse(recorder, upstream, nil, reqLog, true)

	if strings.Contains(recorder.Body.String(), "reasoning_content") || strings.Contains(recorder.Body.String(), `"cost"`) {
		t.Fatalf("non-standard Chat fields leaked: %s", recorder.Body.String())
	}
	events := decodeSSEEvents(t, recorder.Body.String())
	if len(events) != 3 {
		t.Fatalf("standard stream chunks = %d, want text + finish + usage: %#v", len(events), events)
	}
	for index, event := range events {
		if event["object"] != "chat.completion.chunk" {
			t.Fatalf("chunk %d object = %#v", index, event["object"])
		}
		if index < len(events)-1 && event["usage"] != nil {
			t.Fatalf("ordinary chunk %d usage = %#v, want null", index, event["usage"])
		}
	}
	usageChunk := events[len(events)-1]
	if choices, _ := usageChunk["choices"].([]any); len(choices) != 0 {
		t.Fatalf("usage chunk choices = %#v", usageChunk["choices"])
	}
	if getNested(usageChunk, "usage", "total_tokens") != float64(15) {
		t.Fatalf("usage chunk = %#v", usageChunk)
	}
	if !strings.HasSuffix(strings.TrimSpace(recorder.Body.String()), "data: [DONE]") {
		t.Fatalf("stream does not end with [DONE]: %s", recorder.Body.String())
	}
	if reqLog.TTFTMs <= 0 || !reqLog.Completed {
		t.Fatalf("request log = %#v", reqLog)
	}
}

func TestValidateChatCompletionRequest(t *testing.T) {
	valid := map[string]any{
		"model": "m1", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
		"stream": true, "stream_options": map[string]any{"include_usage": true},
	}
	if err := validateChatCompletionRequest(valid); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	for name, request := range map[string]map[string]any{
		"missing model":    {"messages": []any{map[string]any{"role": "user", "content": "hello"}}},
		"missing messages": {"model": "m1"},
		"stream options without streaming": {
			"model": "m1", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
			"stream_options": map[string]any{"include_usage": true},
		},
		"unsupported persistence": {
			"model": "m1", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
			"store": true,
		},
		"unsupported audio": {
			"model": "m1", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
			"modalities": []any{"text", "audio"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateChatCompletionRequest(request); err == nil {
				t.Fatalf("invalid request accepted: %#v", request)
			}
		})
	}
}

func TestChatStreamStopsAfterFirstEventTimeoutAndCoolsDownSlowAccount(t *testing.T) {
	firstAccount := &Account{
		AccountID: "chat-slow", Email: "slow@example.com", AccessToken: "workos:slow",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active", ModelCooldowns: map[string]time.Time{},
	}
	secondAccount := &Account{
		AccountID: "chat-fast", Email: "fast@example.com", AccessToken: "workos:fast",
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
	var blockedWriter *io.PipeWriter
	httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestCount++
		if requestCount == 1 {
			reader, writer := io.Pipe()
			blockedWriter = writer
			return &http.Response{StatusCode: http.StatusOK, Body: reader, Request: request}, nil
		}
		body := "data: {\"model\":\"m1\",\"choices\":[{\"delta\":{\"content\":\"ready\"}}]}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})}
	t.Cleanup(func() {
		httpClient = oldHTTPClient
		if blockedWriter != nil {
			_ = blockedWriter.Close()
		}
	})

	model := "deepseek/deepseek-v4-flash"
	response, account, retryCount, err := callClineChatStreamWithTimeout(map[string]any{
		"model": model, "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, 20*time.Millisecond)
	if !errors.Is(err, errUpstreamFirstEventTimeout) {
		t.Fatalf("Chat timeout = %v", err)
	}
	if response != nil {
		response.Body.Close()
	}
	if requestCount != 1 || account != firstAccount || retryCount != 0 {
		t.Fatalf("retry result: requests=%d account=%#v retries=%d", requestCount, account, retryCount)
	}
	if until := firstAccount.ModelCooldowns[model]; !until.After(time.Now()) {
		t.Fatalf("slow account was not cooled down: %#v", firstAccount.ModelCooldowns)
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
