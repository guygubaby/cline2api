package main

import (
	"crypto/sha256"
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

func TestPrepareSemanticChatStreamReleasesOnFirstReasoningDelta(t *testing.T) {
	reader, writer := io.Pipe()
	response := &http.Response{StatusCode: http.StatusOK, Body: reader}
	type prepareResult struct {
		response   *http.Response
		diagnostic semanticStreamDiagnostic
		err        error
	}
	resultCh := make(chan prepareResult, 1)
	go func() {
		prepared, diagnostic, err := prepareSemanticChatStream(response)
		resultCh <- prepareResult{response: prepared, diagnostic: diagnostic, err: err}
	}()

	reasoningLine := `data: {"choices":[{"delta":{"reasoning_content":"thinking now"}}]}` + "\n"
	if _, err := writer.Write([]byte(reasoningLine)); err != nil {
		t.Fatalf("write reasoning delta: %v", err)
	}

	var result prepareResult
	select {
	case result = <-resultCh:
	case <-time.After(250 * time.Millisecond):
		_ = writer.CloseWithError(errors.New("test timeout"))
		<-resultCh
		t.Fatal("reasoning delta was buffered instead of being released immediately")
	}
	_ = writer.Close()
	if result.err != nil {
		t.Fatalf("prepare reasoning stream: %v", result.err)
	}
	if result.diagnostic.ReasoningChars != len("thinking now") {
		t.Fatalf("reasoning diagnostic = %#v", result.diagnostic)
	}
	replayed, err := io.ReadAll(result.response.Body)
	if err != nil {
		t.Fatalf("read reasoning stream: %v", err)
	}
	if string(replayed) != reasoningLine {
		t.Fatalf("reasoning replay = %q, want %q", replayed, reasoningLine)
	}
}

func TestPrepareSemanticChatStreamTimesOutWithoutSemanticOutput(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	response := &http.Response{StatusCode: http.StatusOK, Body: reader}

	_, diagnostic, err := prepareSemanticChatStreamWithTimeout(response, 20*time.Millisecond)
	if !errors.Is(err, errAnthropicFirstEventTimeout) {
		t.Fatalf("first-event timeout error = %v", err)
	}
	if diagnostic.ReasoningChars != 0 {
		t.Fatalf("timeout diagnostic = %#v", diagnostic)
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
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":1,"total_tokens":101}}`,
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
	if diagnostic.ReasoningChars != len("inspect first") || diagnostic.RetryCount != 1 {
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

func TestCallClineAnthropicStreamCoolsDownAccountAfterFirstEventTimeout(t *testing.T) {
	firstAccount := &Account{
		AccountID: "slow", Email: "slow@example.com", AccessToken: "workos:slow",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active", ModelCooldowns: map[string]time.Time{},
	}
	secondAccount := &Account{
		AccountID: "fast", Email: "fast@example.com", AccessToken: "workos:fast",
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
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       reader,
				Request:    request,
			}, nil
		}
		body := strings.Join([]string{
			`data: {"choices":[{"delta":{"reasoning_content":"ready"}}]}`,
			`data: [DONE]`,
			``,
		}, "\n")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})}
	t.Cleanup(func() {
		httpClient = oldHTTPClient
		if blockedWriter != nil {
			_ = blockedWriter.Close()
		}
	})

	model := "deepseek/deepseek-v4-flash"
	prepared, account, diagnostic, err := callClineAnthropicStreamWithTimeout(map[string]any{
		"model":      model,
		"max_tokens": float64(128000),
		"stream":     true,
		"messages":   []any{map[string]any{"role": "user", "content": "Inspect the project."}},
	}, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("retry after first-event timeout: %v", err)
	}
	defer prepared.Body.Close()
	if requestCount != 2 || account != secondAccount || diagnostic.RetryCount != 1 {
		t.Fatalf("timeout retry result: requests=%d account=%#v diagnostic=%#v", requestCount, account, diagnostic)
	}
	if until := firstAccount.ModelCooldowns[model]; !until.After(time.Now()) {
		t.Fatalf("slow account was not cooled down: %#v", firstAccount.ModelCooldowns)
	}
}

func TestAnthropicRepeatedSemanticEmptyIsNonRetryableAndCircuitBroken(t *testing.T) {
	semanticEmptyCircuitsMu.Lock()
	oldCircuits := semanticEmptyCircuits
	semanticEmptyCircuits = map[string]semanticEmptyCircuitEntry{}
	semanticEmptyCircuitsMu.Unlock()
	t.Cleanup(func() {
		semanticEmptyCircuitsMu.Lock()
		semanticEmptyCircuits = oldCircuits
		semanticEmptyCircuitsMu.Unlock()
	})

	firstAccount := &Account{
		AccountID: "semantic-empty-first", Email: "first@example.com", AccessToken: "workos:first",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active", ModelCooldowns: map[string]time.Time{},
	}
	secondAccount := &Account{
		AccountID: "semantic-empty-second", Email: "second@example.com", AccessToken: "workos:second",
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
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":23000,"completion_tokens":0,"total_tokens":23000,"completion_tokens_details":{"reasoning_tokens":0}}}`,
			`data: [DONE]`,
			``,
		}, "\n")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})}
	t.Cleanup(func() { httpClient = oldHTTPClient })

	requestBody := `{
		"model":"deepseek/deepseek-v4-flash",
		"max_tokens":128000,
		"stream":true,
		"messages":[{"role":"user","content":"semantic-empty-circuit-regression"}]
	}`
	call := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(requestBody))
		handleAnthropicMessages(recorder, request)
		return recorder
	}

	first := call()
	if first.Code != http.StatusBadRequest {
		t.Fatalf("first status = %d, want 400; body=%s", first.Code, first.Body.String())
	}
	var errorResponse map[string]any
	if err := json.Unmarshal(first.Body.Bytes(), &errorResponse); err != nil {
		t.Fatalf("decode first error: %v", err)
	}
	errorBody, _ := errorResponse["error"].(map[string]any)
	if errorBody["type"] != "invalid_request_error" || !strings.Contains(errorBody["message"].(string), "/compact") {
		t.Fatalf("first error is not actionable/non-retryable: %#v", errorResponse)
	}
	if requestCount != 2 {
		t.Fatalf("first call upstream requests = %d, want primary + one alternate", requestCount)
	}

	second := call()
	if second.Code != http.StatusBadRequest {
		t.Fatalf("second status = %d, want 400; body=%s", second.Code, second.Body.String())
	}
	if requestCount != 2 {
		t.Fatalf("identical retry reached upstream: requests=%d, want 2", requestCount)
	}
	page, err := listRequestLogs(1, "")
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("read semantic-empty request log: page=%#v err=%v", page, err)
	}
	entry := page.Items[0]
	if entry.ErrorCode != semanticEmptyErrorCode || entry.FinishReason != "stop" || entry.ReasoningChars != 0 {
		t.Fatalf("semantic-empty diagnostics missing: %#v", entry)
	}
	if entry.ThinkingTokens != 0 || !entry.RetrySuppressed || entry.Upstream != upstreamCline {
		t.Fatalf("semantic-empty suppression metadata missing: %#v", entry)
	}
}

func TestHandleAnthropicStreamTreatsReasoningAsFirstOutput(t *testing.T) {
	upstreamBody := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"thinking only"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":8,"total_tokens":108,"completion_tokens_details":{"reasoning_tokens":8}}}`,
		`data: [DONE]`,
		``,
	}, "\n")
	upstream := &http.Response{Body: io.NopCloser(strings.NewReader(upstreamBody))}
	recorder := httptest.NewRecorder()
	reqLog := &RequestLog{StartedAt: time.Now().Add(-time.Second), Protocol: "anthropic", Model: "m1", Stream: true}

	handleAnthropicStream(recorder, upstream, nil, reqLog, 100)

	events := decodeSSEEvents(t, recorder.Body.String())
	firstContentBlockStartOfType(t, events, "thinking")
	firstEventOfType(t, events, "message_stop")
	for _, event := range events {
		if event["type"] == "error" {
			t.Fatalf("reasoning-only stream ended with an error: %#v", event)
		}
	}
	if reqLog.UpstreamTTFTMs <= 0 || reqLog.ThinkingTTFTMs <= 0 || reqLog.VisibleTTFTMs != 0 {
		t.Fatalf("reasoning latency metrics = %#v", reqLog)
	}
}

func TestSemanticEmptyCircuitFingerprintIsPrivateSpecificAndExpires(t *testing.T) {
	first := map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "private conversation content"}},
	}
	second := map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "different conversation content"}},
	}
	firstFingerprint := semanticRequestFingerprint(first)
	secondFingerprint := semanticRequestFingerprint(second)
	if len(firstFingerprint) != sha256.Size*2 || strings.Contains(firstFingerprint, "private") {
		t.Fatalf("fingerprint is not an opaque SHA-256 digest: %q", firstFingerprint)
	}
	if firstFingerprint == secondFingerprint {
		t.Fatal("different requests produced the same semantic-empty fingerprint")
	}

	semanticEmptyCircuitsMu.Lock()
	oldCircuits := semanticEmptyCircuits
	semanticEmptyCircuits = map[string]semanticEmptyCircuitEntry{}
	semanticEmptyCircuitsMu.Unlock()
	t.Cleanup(func() {
		semanticEmptyCircuitsMu.Lock()
		semanticEmptyCircuits = oldCircuits
		semanticEmptyCircuitsMu.Unlock()
	})

	now := time.Now()
	diagnostic := semanticStreamDiagnostic{FinishReason: "stop", ReasoningChars: 5}
	rememberSemanticEmptyCircuit(firstFingerprint, diagnostic, now)
	if got, active := activeSemanticEmptyCircuit(firstFingerprint, now.Add(time.Minute)); !active || got.FinishReason != "stop" {
		t.Fatalf("circuit was not active during TTL: diagnostic=%#v active=%v", got, active)
	}
	if _, active := activeSemanticEmptyCircuit(firstFingerprint, now.Add(semanticEmptyCircuitTTL)); active {
		t.Fatal("semantic-empty circuit did not expire at TTL")
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
