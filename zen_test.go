package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain 把池文件与请求日志重定向到临时目录：
// 单元测试绝不读写用户真实的 .cline-accounts.json。
func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "cline-proxy-test-*")
	if err != nil {
		panic(err)
	}
	oldPoolPath, oldLogsPath := poolPath, requestLogsPath
	poolPath = filepath.Join(tmp, ".cline-accounts.json")
	requestLogsPath = filepath.Join(tmp, ".cline-request-logs.json")

	poolMu.Lock()
	pool = nil // 强制从临时路径重新加载
	poolMu.Unlock()

	code := m.Run()

	poolPath, requestLogsPath = oldPoolPath, oldLogsPath
	os.RemoveAll(tmp)
	os.Exit(code)
}

// resetZenTestState 恢复与 zen 相关的全局可变状态，避免测试间相互污染。
func resetZenTestState(t *testing.T) {
	t.Helper()
	remoteZenEnabledMu.Lock()
	oldZenEnabled := remoteZenEnabled
	remoteZenEnabled = false
	remoteZenEnabledMu.Unlock()

	zenConfigMu.Lock()
	oldCfg := zenConfig
	zenConfig = nil
	zenConfigMu.Unlock()

	zenStateMu.Lock()
	oldCount, oldUntil := zenFailCount, zenFailUntil
	zenFailCount, zenFailUntil = 0, time.Time{}
	zenStateMu.Unlock()

	compactStatesMu.Lock()
	oldStates := compactStates
	compactStates = make(map[string]*compactState)
	compactStatesMu.Unlock()

	t.Cleanup(func() {
		remoteZenEnabledMu.Lock()
		remoteZenEnabled = oldZenEnabled
		remoteZenEnabledMu.Unlock()
		zenConfigMu.Lock()
		zenConfig = oldCfg
		zenConfigMu.Unlock()
		zenStateMu.Lock()
		zenFailCount, zenFailUntil = oldCount, oldUntil
		zenStateMu.Unlock()
		compactStatesMu.Lock()
		compactStates = oldStates
		compactStatesMu.Unlock()
	})
}

// ============ routeModel / resolveZenInfo 测试 ============

// currentZenModels / resolveZenInfo 把 "zen" 与 "seed" 都视为 zen 来源
func withZenPool(t *testing.T, models []Model) {
	t.Helper()
	resetZenTestState(t)
	p := loadPool()
	oldModels := p.Models
	p.Models = models
	savePool()
	t.Cleanup(func() {
		q := loadPool()
		q.Models = oldModels
		savePool()
	})
}

func TestRouteModelZenFree(t *testing.T) {
	withZenPool(t, append([]Model{}, builtinZenModels()...))
	// 未同步过 → 种子表生效
	if got := routeModel("deepseek-v4-flash-free"); got != "zen" {
		t.Errorf("routeModel(free seed model) = %q, want zen", got)
	}
	if got := routeModel("opencode/mimo-v2.5-free"); got != "zen" {
		t.Errorf("routeModel(opencode/ prefix) = %q, want zen", got)
	}
	// 别名
	if got := routeModel("deepseek-v4"); got != "zen" {
		t.Errorf("routeModel(alias) = %q, want zen", got)
	}
}

func TestRouteModelRejectPaid(t *testing.T) {
	withZenPool(t, []Model{
		{ID: "gpt-5-turbo", Provider: "opencode", Cost: "pass", Status: "active", Source: "zen"},
	})
	if got := routeModel("gpt-5-turbo"); got != "reject" {
		t.Errorf("routeModel(paid zen model) = %q, want reject", got)
	}
	if got := routeModel("opencode/gpt-5-turbo"); got != "reject" {
		t.Errorf("routeModel(opencode/paid) = %q, want reject", got)
	}
}

func TestRouteModelClinePassthrough(t *testing.T) {
	withZenPool(t, append([]Model{}, builtinZenModels()...))
	if got := routeModel("cline-free/glm-5.2"); got != "cline" {
		t.Errorf("routeModel(cline model) = %q, want cline", got)
	}
	if got := routeModel(""); got != "cline" {
		t.Errorf("routeModel(empty) = %q, want cline", got)
	}
}

func TestDegradedZenOnlyModelDoesNotLeakIntoCline(t *testing.T) {
	withZenPool(t, []Model{
		{ID: "big-pickle", Provider: "opencode", Cost: "free", Status: "active", Source: "zen"},
	})
	zenStateMu.Lock()
	zenFailUntil = time.Now().Add(5 * time.Minute)
	zenStateMu.Unlock()

	if got := routeModel("big-pickle"); got != "zen_unavailable" {
		t.Fatalf("routeModel(degraded zen-only model) = %q, want zen_unavailable", got)
	}
}

func TestDegradedZenModelUsesExplicitClineFallback(t *testing.T) {
	withZenPool(t, []Model{
		{ID: "deepseek-v4-flash-free", Provider: "opencode", Cost: "free", Status: "active", Source: "zen"},
		{ID: "deepseek/deepseek-v4-flash", Provider: "deepseek", Cost: "free", Status: "active", Source: "remote"},
	})
	zenStateMu.Lock()
	zenFailUntil = time.Now().Add(5 * time.Minute)
	zenStateMu.Unlock()

	decision := resolveModelRoute("deepseek-v4-flash-free")
	if decision.Route != modelRouteCline || decision.Model != "deepseek/deepseek-v4-flash" {
		t.Fatalf("route decision = %#v, want explicit cline fallback", decision)
	}
}

func TestZenBadRequestDoesNotArmGlobalFailover(t *testing.T) {
	withZenPool(t, []Model{
		{ID: "big-pickle", Provider: "opencode", Cost: "free", Status: "active", Source: "zen"},
	})
	cfg := getZenConfig()
	cfg.BaseURL = "https://zen.test/v1"
	cfg.Retries = 1
	zenConfigMu.Lock()
	zenConfig = cloneZenConfig(cfg)
	zenConfigMu.Unlock()

	zenTransportMu.Lock()
	oldClient := zenHTTPClient
	zenHTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":"bad request"}`)),
			Request:    request,
		}, nil
	})}
	zenTransportMu.Unlock()
	t.Cleanup(func() {
		zenTransportMu.Lock()
		zenHTTPClient = oldClient
		zenTransportMu.Unlock()
	})

	params := map[string]any{
		"model": "big-pickle",
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
	}
	for range 3 {
		if _, err := callZenAPI(params, false); err == nil {
			t.Fatal("callZenAPI unexpectedly succeeded")
		}
	}
	if zenFailedNow() {
		t.Fatal("three request-specific 400 responses must not arm global failover")
	}
}

// 僵尸模型回归：同步成功后（remoteZenEnabled=true），官方已下架的种子模型必须休眠。
func TestDelistedSeedModelGoesDormant(t *testing.T) {
	remoteZenEnabledMu.Lock()
	oldEnabled := remoteZenEnabled
	remoteZenEnabled = true
	remoteZenEnabledMu.Unlock()
	t.Cleanup(func() { restoreRemoteZen(oldEnabled) })

	// 池中只有官方还存在的模型，longcat-2.0-free 已被下架（不在池里）
	withZenPool(t, []Model{
		{ID: "deepseek-v4-flash-free", Provider: "opencode", Cost: "free", Source: "zen", Context: 200000},
	})
	if _, ok := resolveZenInfo("longcat-2.0-free"); ok {
		t.Error("delisted seed model should be dormant after sync (no zombie)")
	}
	if _, ok := resolveZenInfo("big-pickle"); ok {
		t.Error("delisted seed model big-pickle should be dormant")
	}
	m, ok := resolveZenInfo("deepseek-v4-flash-free")
	if !ok || m.ID != "deepseek-v4-flash-free" {
		t.Errorf("surviving model should resolve, got %+v ok=%v", m, ok)
	}
}

func restoreRemoteZen(v bool) {
	remoteZenEnabledMu.Lock()
	remoteZenEnabled = v
	remoteZenEnabledMu.Unlock()
}

func TestAliasNotShadowedBySyncedPaidModel(t *testing.T) {
	restoreRemoteZen(false)
	withZenPool(t, append([]Model{}, builtinZenModels()...))
	// 同步来一个付费模型 ID 恰好等于 free 别名（参考项目的隐患场景）
	withZenPool(t, append(builtinZenModels(), Model{
		ID: "deepseek-v4-flash", Provider: "opencode", Cost: "pass", Source: "zen",
	}))
	// 别名 deepseek-v4-flash 应解析到免费正式 ID 而非付费条目
	m, ok := resolveZenInfo("deepseek-v4-flash")
	if !ok {
		t.Fatal("alias should still resolve")
	}
	if m.Cost != "free" || !strings.HasSuffix(m.ID, "-free") {
		t.Errorf("alias resolved to paid model %+v, want free -free model", m)
	}
}

// ============ 故障转移状态机测试 ============

func TestFailoverStateMachine(t *testing.T) {
	// 隔离全局状态
	zenStateMu.Lock()
	oldCount, oldUntil := zenFailCount, zenFailUntil
	zenStateMu.Unlock()
	t.Cleanup(func() {
		zenStateMu.Lock()
		zenFailCount, zenFailUntil = oldCount, oldUntil
		zenStateMu.Unlock()
	})

	zenStateMu.Lock()
	zenFailCount, zenFailUntil = 0, time.Time{}
	zenStateMu.Unlock()

	if zenFailedNow() {
		t.Fatal("should not be in failover initially")
	}
	for i := 0; i < 3; i++ {
		markZenFail()
	}
	if !zenFailedNow() {
		t.Error("3 consecutive failures should arm failover")
	}
	markZenSuccess()
	if zenFailedNow() {
		t.Error("success should reset failover state")
	}
}

func TestIsRateLimited(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{429, `{"error":"x"}`, true},
		{503, "", true},
		{403, `"ResourceExhausted"`, true},
		{502, "rate limit reached", true},
		{400, "rate limit in body only", false}, // 400 不算限流信号
		{500, "", false},
	}
	for _, c := range cases {
		if got := isRateLimited(c.status, c.body); got != c.want {
			t.Errorf("isRateLimited(%d,%q)=%v want %v", c.status, c.body, got, c.want)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d := parseRetryAfter("120"); d != 120*time.Second {
		t.Errorf("seconds parse: got %v", d)
	}
	if d := parseRetryAfter(""); d != 0 {
		t.Errorf("empty: got %v", d)
	}
	future := time.Now().Add(2 * time.Minute).UTC().Format(http.TimeFormat)
	d := parseRetryAfter(future)
	if d <= 0 || d > 3*time.Minute {
		t.Errorf("http-date parse: got %v", d)
	}
}

// ============ 配置校验测试 ============

func TestValidateProxyList(t *testing.T) {
	if err := validateProxyList([]string{"socks5://127.0.0.1:1080", "http://user:pass@p.com:8080"}); err != nil {
		t.Errorf("valid list rejected: %v", err)
	}
	for _, bad := range []string{"ftp://x:1", "http://noport", "not-a-url"} {
		if err := validateProxyList([]string{bad}); err == nil {
			t.Errorf("bad proxy %q accepted", bad)
		}
	}
	if err := validateProxyList([]string{"", "  "}); err != nil {
		t.Errorf("blank lines should pass: %v", err)
	}
}

// ============ 压缩核心逻辑测试 ============

func TestSelectRecentBudgetSplit(t *testing.T) {
	msgs := []string{
		strings.Repeat("a", 800), // ~200 tokens
		strings.Repeat("b", 800), // ~200 tokens
		strings.Repeat("c", 80),  // ~20 tokens
	}
	sel := selectRecent(msgs, 220)
	if sel == nil {
		t.Fatal("expected selection")
	}
	if sel.split != 1 {
		t.Errorf("split=%d want 1 (last two messages fit)", sel.split)
	}
	recentText := strings.Join(sel.recent, "\n")
	if !strings.Contains(recentText, strings.Repeat("b", 10)) || !strings.Contains(recentText, "cccc") {
		t.Error("recent should contain tail messages/suffix")
	}
	if len(sel.head) == 0 || !strings.HasPrefix(sel.head[0], "aaaa") {
		t.Error("head should contain overflowed prefix")
	}
}

func TestSerializeMsgRoles(t *testing.T) {
	user := map[string]any{"role": "user", "content": "hello"}
	if s := serializeMsg(user); !strings.HasPrefix(s, "[User]: hello") {
		t.Errorf("user serialize: %q", s)
	}
	tool := map[string]any{"role": "tool", "content": strings.Repeat("x", 3000)}
	s := serializeMsg(tool)
	if len(s) > toolOutputMaxChars+50 {
		t.Errorf("tool output not truncated: %d", len(s))
	}
	asst := map[string]any{
		"role":    "assistant",
		"content": "doing it",
		"tool_calls": []any{map[string]any{
			"id": "c1", "type": "function",
			"function": map[string]any{"name": "edit_file", "arguments": `{"path":"a.go"}`},
		}},
	}
	s = serializeMsg(asst)
	if !strings.Contains(s, `[Assistant]: doing it`) || !strings.Contains(s, "[Assistant tool call]: edit_file") {
		t.Errorf("assistant serialize: %q", s)
	}
}

func TestEstimateJSONAndCompactDisabled(t *testing.T) {
	cfg := getZenConfig()
	if cfg.Compaction.Buffer <= 0 {
		t.Error("default compaction buffer should be positive")
	}
	params := map[string]any{
		"model":    "m",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	bts, _ := json.Marshal(params)
	if estimateJSON(params) != len(bts)/4 {
		t.Error("estimateJSON should be len/4")
	}
}

// ============ buildZenBody 测试 ============

func TestBuildZenBodyRewritesModelAndStripsReasoning(t *testing.T) {
	params := map[string]any{
		"model":      "deepseek-v4-flash",
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		"max_tokens": 100.0,
		"stream":     true,
		"tools":      []any{},
	}
	body := buildZenBody(params, true)
	if body["model"] != "deepseek-v4-flash-free" {
		t.Errorf("model alias rewrite failed: %v", body["model"])
	}
	if body["stream"] != true {
		t.Error("stream flag lost")
	}
	for _, k := range []string{"reasoning_effort", "reasoningEffort"} {
		if _, ok := body[k]; ok {
			t.Errorf("%s should be stripped", k)
		}
	}
	if body["session_id"] != nil {
		t.Error("zen body must not carry cline session_id")
	}
}

// ============ Responses API 转换测试 ============

func TestResponsesToChatStringInput(t *testing.T) {
	out := responsesToChat(map[string]any{
		"model":             "m1",
		"input":             "hello",
		"instructions":      "be brief",
		"max_output_tokens": 500.0,
	})
	msgs := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("want system+user, got %d", len(msgs))
	}
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Error("instructions should become first system message")
	}
	if out["max_tokens"] != 500 {
		t.Errorf("max_output_tokens mapping: %v", out["max_tokens"])
	}
}

func TestResponsesToChatToolRoundTrip(t *testing.T) {
	body := map[string]any{
		"model": "m1",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "run it"},
			map[string]any{"type": "function_call", "call_id": "fc1", "name": "shell", "arguments": map[string]any{"cmd": "ls"}},
			map[string]any{"type": "function_call_output", "call_id": "fc1", "output": "file.txt"},
		},
		"tools": []any{map[string]any{
			"type": "function", "name": "shell", "description": "run command",
			"parameters": map[string]any{"type": "object"},
		}},
	}
	out := responsesToChat(body)
	msgs := out["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("want user+assistant(tool_call)+tool, got %d msgs", len(msgs))
	}
	tc := msgs[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if tc["id"] != "fc1" {
		t.Errorf("call_id propagation: %v", tc["id"])
	}
	var argsObj map[string]any
	json.Unmarshal([]byte(tc["function"].(map[string]any)["arguments"].(string)), &argsObj)
	if argsObj["cmd"] != "ls" {
		t.Errorf("object arguments marshaled: %v", argsObj)
	}
	if msgs[2].(map[string]any)["role"] != "tool" {
		t.Error("function_call_output -> role tool")
	}
	tools := out["tools"].([]any)[0].(map[string]any)
	fn := tools["function"].(map[string]any)
	if tools["type"] != "function" || fn["name"] != "shell" {
		t.Errorf("flat tool conversion broken: %v", tools)
	}
}

func TestChatToResponsesUsageMapping(t *testing.T) {
	chat := map[string]any{
		"model": "mm",
		"choices": []any{map[string]any{
			"message": map[string]any{"content": "answer text", "tool_calls": []any{
				map[string]any{"id": "c9", "type": "function",
					"function": map[string]any{"name": "f1", "arguments": "{}"}},
			}},
		}},
		"usage": map[string]any{
			"prompt_tokens": float64(11), "completion_tokens": float64(7), "total_tokens": float64(18),
			"prompt_tokens_details": map[string]any{"cached_tokens": float64(3)},
		},
	}
	resp := chatToResponses(chat)
	if resp["object"] != "response" || resp["status"] != "completed" {
		t.Errorf("response envelope: %v/%v", resp["object"], resp["status"])
	}
	outputs := resp["output"].([]any)
	if outputs[0].(map[string]any)["type"] != "message" {
		t.Errorf("first item type: %v", outputs[0])
	}
	if outputs[1].(map[string]any)["call_id"] != "c9" {
		t.Errorf("function_call call_id: %v", outputs[1])
	}
	u := resp["usage"].(map[string]any)
	if u["input_tokens"] != float64(11) || u["output_tokens"] != float64(7) {
		t.Errorf("usage mapping: %v", u)
	}
	if u["input_tokens_details"].(map[string]any)["cached_tokens"] != float64(3) {
		t.Errorf("cached tokens mapping: %v", u)
	}
	if resp["output_text"] != "answer text" {
		t.Errorf("output_text: %v", resp["output_text"])
	}
}

func TestUnwrapDataEnvelope(t *testing.T) {
	in := map[string]any{"data": map[string]any{"choices": []any{}, "id": "abc"}}
	if unwrapDataEnvelope(in)["id"] != "abc" {
		t.Error("envelope should be unwrapped when data has choices/id")
	}
	if unwrapDataEnvelope(map[string]any{"other": 1}) == nil {
		t.Error("non-envelope passthrough")
	}
}

func TestUsageToResponses(t *testing.T) {
	u := usageToResponses(tokenUsage{Prompt: 10, Completion: 5, Cached: 2})
	if u["input_tokens"] != int64(10) || u["output_tokens"] != int64(5) || u["total_tokens"] != int64(15) {
		t.Errorf("aggregate usage: %v", u)
	}
	if u["input_tokens_details"].(map[string]any)["cached_tokens"] != int64(2) {
		t.Errorf("cached detail: %v", u)
	}
}
