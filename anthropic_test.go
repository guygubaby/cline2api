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

func TestHandleAnthropicStreamPreservesFragmentedToolInput(t *testing.T) {
	upstreamBody := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"{\"command\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"pwd\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	upstream := &http.Response{Body: io.NopCloser(strings.NewReader(upstreamBody))}
	recorder := httptest.NewRecorder()
	startedAt := time.Now()
	reqLog := &RequestLog{StartedAt: startedAt, Protocol: "anthropic", Stream: true}

	handleAnthropicStream(recorder, upstream, nil, reqLog, 0)

	body := recorder.Body.String()
	if !strings.Contains(body, `"content_block":{"id":"call_1","input":{},"name":"Bash","type":"tool_use"}`) {
		t.Fatalf("missing Anthropic tool_use block start; SSE body:\n%s", body)
	}
	fragments := []string{}
	for _, event := range decodeSSEEvents(t, body) {
		delta, _ := event["delta"].(map[string]any)
		if delta["type"] == "input_json_delta" {
			fragment, _ := delta["partial_json"].(string)
			fragments = append(fragments, fragment)
		}
	}
	if strings.Join(fragments, "") != `{"command":"pwd"}` {
		t.Fatalf("fragmented tool arguments were not forwarded incrementally: %#v", fragments)
	}
}

func TestHandleAnthropicMessagesKeepsNonStreamingToolUse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl_1",
			"model":"deepseek-v4-flash-free",
			"choices":[{
				"index":0,
				"message":{"role":"assistant","content":null,"tool_calls":[{
					"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"pwd\"}"}
				}]},
				"finish_reason":"tool_calls"
			}],
			"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}
		}`))
	}))
	t.Cleanup(upstream.Close)

	withZenPool(t, append([]Model{}, builtinZenModels()...))
	zenConfigMu.Lock()
	zenConfig = &zenConfigData{
		Enabled:        true,
		Key:            "test",
		BaseURL:        upstream.URL,
		MaxConcurrency: 1,
		Retries:        1,
	}
	zenConfigMu.Unlock()
	rebuildZenSem()

	body := `{
		"model":"deepseek-v4-flash-free",
		"max_tokens":1024,
		"stream":false,
		"messages":[{"role":"user","content":"看看项目是啥"}],
		"tools":[{"name":"Bash","description":"Run a shell command","input_schema":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	recorder := httptest.NewRecorder()

	handleAnthropicMessages(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Content []struct {
			Type  string         `json:"type"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(response.Content) != 1 || response.Content[0].Type != "tool_use" {
		t.Fatalf("tool_use content was lost: %s", recorder.Body.String())
	}
	if response.Content[0].Name != "Bash" || response.Content[0].Input["command"] != "pwd" {
		t.Fatalf("unexpected tool_use content: %s", recorder.Body.String())
	}
	if response.StopReason != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", response.StopReason)
	}
}

func TestAnthropicToOpenAIMapsCurrentMessagesRequest(t *testing.T) {
	raw := `{
		"model":"m1",
		"max_tokens":2048,
		"temperature":0,
		"top_p":0,
		"top_k":20,
		"stop_sequences":["END"],
		"tool_choice":{"type":"any","disable_parallel_tool_use":true},
		"output_config":{"effort":"high","format":{"type":"json_schema","schema":{"type":"object"}}},
		"tools":[{"name":"Bash","description":"Run a command","input_schema":{"type":"object"},"strict":true}],
		"messages":[
			{"role":"assistant","content":[
				{"type":"text","text":"I'll run both."},
				{"type":"tool_use","id":"call_1","name":"Bash","input":{"command":"pwd"}},
				{"type":"tool_use","id":"call_2","name":"Bash","input":{"command":"ls"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"call_1","content":"/tmp"},
				{"type":"tool_result","tool_use_id":"call_2","content":"a.txt"},
				{"type":"text","text":"Continue"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}
			]}
		]
	}`
	var req anthropicReq
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}

	out := anthropicToOpenAI(req)
	if out["temperature"] != float64(0) || out["top_p"] != float64(0) || out["top_k"] != 20 {
		t.Fatalf("sampling fields were not preserved: %#v", out)
	}
	if stop, ok := out["stop"].([]any); !ok || len(stop) != 1 || stop[0] != "END" {
		t.Fatalf("stop_sequences mapping = %#v", out["stop"])
	}
	if out["tool_choice"] != "required" || out["parallel_tool_calls"] != false {
		t.Fatalf("tool_choice mapping = %#v, parallel = %#v", out["tool_choice"], out["parallel_tool_calls"])
	}
	if out["reasoning_effort"] != "high" {
		t.Fatalf("output_config.effort mapping = %#v", out["reasoning_effort"])
	}
	format, _ := out["response_format"].(map[string]any)
	if format["type"] != "json_schema" {
		t.Fatalf("output_config.format mapping = %#v", format)
	}
	tools := out["tools"].([]any)
	function := tools[0].(map[string]any)["function"].(map[string]any)
	if function["strict"] != true {
		t.Fatalf("strict tool flag was lost: %#v", function)
	}

	messages := out["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("want assistant + 2 tool results + user, got %d: %#v", len(messages), messages)
	}
	if calls := messages[0].(map[string]any)["tool_calls"].([]any); len(calls) != 2 {
		t.Fatalf("parallel tool calls were lost: %#v", messages[0])
	}
	if messages[1].(map[string]any)["tool_call_id"] != "call_1" || messages[2].(map[string]any)["tool_call_id"] != "call_2" {
		t.Fatalf("tool result order was not preserved: %#v", messages)
	}
	content, ok := messages[3].(map[string]any)["content"].([]any)
	if !ok || len(content) != 2 || content[1].(map[string]any)["type"] != "image_url" {
		t.Fatalf("multimodal user content was not preserved: %#v", messages[3])
	}
}

func TestOpenAIToAnthropicUsesCurrentMessageEnvelope(t *testing.T) {
	response := openAIToAnthropic(map[string]any{
		"model": "m1",
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": "done"},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens": 12.0, "completion_tokens": 3.0,
			"prompt_tokens_details": map[string]any{"cached_tokens": 4.0},
		},
	})
	if _, ok := response["stop_sequence"]; !ok {
		t.Fatalf("stop_sequence is required in a standard Messages response: %#v", response)
	}
	usage := response["usage"].(map[string]any)
	if usage["input_tokens"] != int64(8) || usage["output_tokens"] != int64(3) || usage["cache_read_input_tokens"] != int64(4) {
		t.Fatalf("usage mapping = %#v", usage)
	}
}

func TestAnthropicParseErrorUsesStandardEnvelope(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":`))
	recorder := httptest.NewRecorder()
	handleAnthropicMessages(recorder, req)

	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if response["type"] != "error" {
		t.Fatalf("standard Anthropic error envelope missing: %s", recorder.Body.String())
	}
}

func TestAnthropicCountTokensUsesStandardEnvelope(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{
		"model":"m1",
		"messages":[{"role":"user","content":"hello"}]
	}`))
	recorder := httptest.NewRecorder()
	handleAnthropicCountTokens(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if tokens, _ := response["input_tokens"].(float64); tokens < 1 {
		t.Fatalf("input_tokens = %#v", response["input_tokens"])
	}
}

func TestAnthropicStreamStartsWithCurrentMessageShape(t *testing.T) {
	upstreamBody := strings.Join([]string{
		`data: {"model":"m1","choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`,
		`data: [DONE]`,
		``,
	}, "\n")
	upstream := &http.Response{Body: io.NopCloser(strings.NewReader(upstreamBody))}
	recorder := httptest.NewRecorder()
	reqLog := &RequestLog{StartedAt: time.Now(), Protocol: "anthropic", Model: "m1", Stream: true}

	handleAnthropicStream(recorder, upstream, nil, reqLog, 42)

	events := decodeSSEEvents(t, recorder.Body.String())
	start := firstEventOfType(t, events, "message_start")
	message := start["message"].(map[string]any)
	if message["model"] != "m1" {
		t.Fatalf("model = %#v", message["model"])
	}
	if _, ok := message["stop_sequence"]; !ok {
		t.Fatalf("stop_sequence missing: %#v", message)
	}
	if _, ok := message["usage"].(map[string]any); !ok {
		t.Fatalf("usage missing: %#v", message)
	}
	if message["usage"].(map[string]any)["input_tokens"] != float64(42) {
		t.Fatalf("estimated input usage missing from message_start: %#v", message["usage"])
	}
}

func TestAnthropicStreamReportsFinalInputCacheAndReasoningUsage(t *testing.T) {
	upstreamBody := strings.Join([]string{
		`data: {"model":"m1","choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"model":"m1","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":120,"completion_tokens":15,"total_tokens":135,"prompt_tokens_details":{"cached_tokens":40,"cache_write_tokens":7},"completion_tokens_details":{"reasoning_tokens":9}}}`,
		`data: [DONE]`,
		``,
	}, "\n")
	upstream := &http.Response{Body: io.NopCloser(strings.NewReader(upstreamBody))}
	recorder := httptest.NewRecorder()
	reqLog := &RequestLog{StartedAt: time.Now(), Protocol: "anthropic", Model: "m1", Stream: true}

	handleAnthropicStream(recorder, upstream, nil, reqLog, 0)

	events := decodeSSEEvents(t, recorder.Body.String())
	delta := firstEventOfType(t, events, "message_delta")
	usage := delta["usage"].(map[string]any)
	if usage["input_tokens"] != float64(73) || usage["output_tokens"] != float64(15) {
		t.Fatalf("input/output usage = %#v", usage)
	}
	if usage["cache_read_input_tokens"] != float64(40) || usage["cache_creation_input_tokens"] != float64(7) {
		t.Fatalf("cache usage = %#v", usage)
	}
	details, _ := usage["output_tokens_details"].(map[string]any)
	if details["thinking_tokens"] != float64(9) {
		t.Fatalf("thinking usage = %#v", usage)
	}
}
