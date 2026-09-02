package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultMaxTokens          = 128000
	smallAuxiliaryMaxTokens   = 256
	fallbackDefaultModel      = "z-ai/glm-5.3-flash"
	virtualFreeModel          = "free"
	freeModelPrimary          = "z-ai/glm-5.3-flash"
	freeModelFallback         = "deepseek/deepseek-v4-flash"
	freeModelLastResort       = "cline-free/longcat-2.0"
	freeModelAttemptsPerModel = 2
)

// freeModelChain powers the virtual "free" model. Each model gets a small,
// fixed account budget so an exhausted pool cannot multiply into unbounded retries.
var freeModelChain = []string{freeModelPrimary, freeModelFallback, freeModelLastResort}

// builtinModels 是内置默认模型列表（不可删除），仅作为离线 / 未同步时的 fallback。
// 同步 Cline 官方推荐模型成功后，getAllModels 以远程模型为主。
var builtinModels = []Model{
	{ID: "z-ai/glm-5.3-flash", Provider: "z-ai", Cost: "free", Status: "active", Custom: false},
	{ID: "cline-free/longcat-2.0", Provider: "cline-free", Cost: "free", Status: "active", Custom: false},
	{ID: "cline-pass/glm-5.2", Provider: "zai", Cost: "pass", Status: "active", Custom: false},
	{ID: "cline-pass/deepseek-v4-flash", Provider: "deepseek", Cost: "pass", Status: "active", Custom: false},
	{ID: "cline-pass/qwen3.7-max", Provider: "qwen", Cost: "pass", Status: "active", Custom: false},
	{ID: "deepseek/deepseek-v4-flash", Provider: "deepseek", Cost: "free", Status: "active", Custom: false},
	{ID: "poolside/laguna-s-2.1:free", Provider: "poolside", Cost: "free", Status: "active", Custom: false},
}

// getAllModels 返回可用模型列表：
//   - 已同步远程模型：Cline 远程（Source=remote）+ opencode 同步（Source=zen）+ 用户自定义
//   - 未同步 / 离线：内置 fallback（Cline + zen 种子表）+ 用户自定义
func getAllModels() []Model {
	p := loadPool()
	poolMu.Lock()
	storedModels := append([]Model(nil), p.Models...)
	poolMu.Unlock()

	var custom []Model
	var remote []Model
	var zen []Model
	for _, m := range storedModels {
		switch m.Source {
		case "remote":
			remote = append(remote, m)
		case "zen":
			zen = append(zen, m)
		default:
			custom = append(custom, m)
		}
	}

	if len(remote) > 0 || len(zen) > 0 || remoteZenActive() {
		result := make([]Model, 0, len(remote)+len(zen)+len(custom))
		result = append(result, remote...)
		result = append(result, zen...)
		result = append(result, custom...)
		return mergeModelCatalogs(result, customProviderCatalogModels())
	}

	builtin := make([]Model, 0, len(builtinModels)+len(zenSeedModels))
	builtin = append(builtin, builtinModels...)
	builtin = append(builtin, builtinZenModels()...)

	result := make([]Model, 0, len(builtin)+len(custom))
	result = append(result, builtin...)
	result = append(result, custom...)
	return mergeModelCatalogs(result, customProviderCatalogModels())
}

func mergeModelCatalogs(primary, additional []Model) []Model {
	result := make([]Model, 0, len(primary)+len(additional))
	indexByID := make(map[string]int, len(primary)+len(additional))
	mergeOne := func(model Model) {
		if index, exists := indexByID[model.ID]; exists {
			result[index].ChannelCount += model.ChannelCount
			if model.Source == "custom_provider" && model.ChannelCount > 0 {
				result[index].Source = model.Source
				result[index].Provider = model.Provider
			}
			if result[index].Context == 0 {
				result[index].Context = model.Context
			}
			if result[index].Output == 0 {
				result[index].Output = model.Output
			}
			return
		}
		indexByID[model.ID] = len(result)
		result = append(result, model)
	}
	for _, model := range primary {
		mergeOne(model)
	}
	for _, model := range additional {
		mergeOne(model)
	}
	return result
}

// getDefaultModel 返回用户设置的默认模型；未设置时优先回退到第一个远程 free 模型，
// 否则用内置 fallback。
func getDefaultModel() string {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	if p.DefaultModel != "" {
		return p.DefaultModel
	}

	for _, m := range p.Models {
		if m.Source == "remote" && m.Cost == "free" {
			return m.ID
		}
	}

	for _, m := range builtinModels {
		if m.Cost == "free" {
			return m.ID
		}
	}
	return fallbackDefaultModel
}

// 当前监听地址（startProxy 启动时赋值，供管理后台展示）。
var (
	listenHost string
	listenPort int
)

// HTTP server 实例与路由表（restartListener 换地址重启时复用）。
var (
	serverMux     *http.ServeMux
	currentServer *http.Server
	serverMu      sync.Mutex
)

// restartListener 用新地址重启 HTTP 监听。
// 注意：必须在 goroutine 中调用——Shutdown 会等待当前 HTTP 请求完成，
// 若在 admin handler 内同步调用会死锁。
func restartListener(host string, port int) error {
	if host == "" {
		host = "127.0.0.1"
	}
	addr := fmt.Sprintf("%s:%d", host, port)
	listenHost = host
	listenPort = port

	serverMu.Lock()
	old := currentServer
	server := &http.Server{Addr: addr, Handler: serverMux}
	currentServer = server
	serverMu.Unlock()

	if old != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = old.Shutdown(ctx)
		cancel()
	}

	fmt.Println("")
	fmt.Println(strings.Repeat("=", 58))
	fmt.Printf("  Listener restarted: %s\n", addr)
	if !isLoopbackHost(host) {
		for _, ip := range detectLocalIPs() {
			fmt.Printf("  http://%s:%d (LAN)\n", ip, port)
		}
		fmt.Println("  !!! 监听非本机地址，管理后台无鉴权，请确认网络环境安全")
	}
	fmt.Println(strings.Repeat("=", 58))
	return server.ListenAndServe()
}

// effectiveAdminHost 返回管理后台/浏览器实际可用的访问地址：
// host 为空或通配地址（0.0.0.0 / ::）时展示回环 127.0.0.1，否则返回 host 本身。
func effectiveAdminHost(host string) string {
	switch host {
	case "", "0.0.0.0", "::":
		return "127.0.0.1"
	}
	return host
}

// detectLocalIPs 检测本机所有可用 IPv4 地址（排除回环、链路本地和未启用的网卡）。
func detectLocalIPs() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	result := make([]string, 0, len(addrs))
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipNet.IP
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		if v4 := ip.To4(); v4 != nil {
			result = append(result, v4.String())
		}
	}
	return result
}

// isLoopbackHost 判断监听地址是否为回环（127.x / localhost），用于安全提示。
func isLoopbackHost(host string) bool {
	switch host {
	case "", "localhost", "127.0.0.1":
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

var passThroughKeys = []string{
	"tools", "tool_choice", "parallel_tool_calls", "functions", "function_call",
	"temperature", "top_p", "top_k", "stop", "presence_penalty", "frequency_penalty",
	"response_format", "user", "n", "logit_bias", "seed", "logprobs", "top_logprobs",
	"stream_options", "metadata", "thinking", "service_tier", "safety_identifier",
	"prompt_cache_key", "prompt_cache_options", "prompt_cache_retention",
}

type chatRequest struct {
	Model               string          `json:"model"`
	Messages            json.RawMessage `json:"messages"`
	Stream              bool            `json:"stream,omitempty"`
	MaxTokens           int             `json:"max_tokens,omitempty"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	Tools               json.RawMessage `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	ReasoningEffortAlt  string          `json:"reasoningEffort,omitempty"`
	Extra               map[string]any  `json:"-"`
}

func validateChatCompletionRequest(params map[string]any) error {
	if model, _ := params["model"].(string); strings.TrimSpace(model) == "" {
		return fmt.Errorf("model is required")
	}
	messages, ok := params["messages"].([]any)
	if !ok || len(messages) == 0 {
		return fmt.Errorf("messages is required and must be a non-empty array")
	}
	if streamValue, exists := params["stream"]; exists {
		if _, ok := streamValue.(bool); !ok {
			return fmt.Errorf("stream must be a boolean")
		}
	}
	if rawOptions, exists := params["stream_options"]; exists {
		if streaming, _ := params["stream"].(bool); !streaming {
			return fmt.Errorf("stream_options may only be set when stream is true")
		}
		options, ok := rawOptions.(map[string]any)
		if !ok {
			return fmt.Errorf("stream_options must be an object")
		}
		for _, field := range []string{"include_usage", "include_obfuscation"} {
			if value, exists := options[field]; exists {
				if _, ok := value.(bool); !ok {
					return fmt.Errorf("stream_options.%s must be a boolean", field)
				}
			}
		}
	}
	if store, exists := params["store"]; exists {
		value, ok := store.(bool)
		if !ok {
			return fmt.Errorf("store must be a boolean")
		}
		if value {
			return fmt.Errorf("store=true is not supported because this proxy does not persist Chat Completions")
		}
	}
	if modalities, exists := params["modalities"]; exists {
		items, ok := modalities.([]any)
		if !ok {
			return fmt.Errorf("modalities must be an array")
		}
		for _, item := range items {
			if item != "text" {
				return fmt.Errorf("only the text modality is supported by this upstream")
			}
		}
	}
	for _, unsupported := range []string{"audio", "prediction", "web_search_options"} {
		if value, exists := params[unsupported]; exists && value != nil {
			return fmt.Errorf("%s is not supported by this chat-completions upstream", unsupported)
		}
	}
	return nil
}

func chatStreamIncludesUsage(params map[string]any) bool {
	options, _ := params["stream_options"].(map[string]any)
	include, _ := options["include_usage"].(bool)
	return include
}

func startProxy(host string, port int) error {
	p := loadPool()
	configureAdminPasswordFromEnvironment()
	if validLoadBalancingStrategy(p.AccountStrategy) {
		cfg := getProxyConfig()
		cfg.Strategy = p.AccountStrategy
		if err := setProxyConfig(cfg); err != nil {
			log.Printf("proxy config save failed: %v", err)
		}
	}
	loadRequestLogs()
	activeCount := 0
	for _, a := range p.Accounts {
		if a.Status == "active" {
			// Try to pre-warm tokens
			if a.AccessToken == "" || time.Now().UnixMilli() >= a.ExpiresAt {
				if err := refreshAccountToken(a); err != nil {
					log.Printf("  Pre-warm failed for %s: %v", a.Email, err)
					continue
				}
			}
			activeCount++
		}
	}
	log.Printf("Loaded %d active accounts from pool", activeCount)

	// 启动时异步同步一次 Cline 官方推荐模型（不阻塞启动）
	startModelSync()

	// opencode zen：定时同步免费模型列表 + 压缩会话状态清理
	if getZenConfig().Enabled {
		startZenModelsRefresher()
	}
	startCompactCleanup()

	freePort(port)

	mux := http.NewServeMux()

	mux.HandleFunc("/v1/health", corsHandler(func(w http.ResponseWriter, r *http.Request) {
		info := map[string]any{
			"status":         "ok",
			"version":        appVersion,
			"activeAccounts": activeCount,
		}
		writeJSON(w, http.StatusOK, info)
	}))
	mux.HandleFunc("/health", corsHandler(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":         "ok",
			"version":        appVersion,
			"activeAccounts": activeCount,
		})
	}))

	// Admin API (frontend + REST)
	registerAdminRoutes(mux)

	apiKeyHandler := func(next http.HandlerFunc) http.HandlerFunc {
		return corsHandler(func(w http.ResponseWriter, r *http.Request) {
			// Allow requests without key if no keys configured
			p := loadPool()
			if len(p.Keys) == 0 {
				next(w, requestWithTenantScope(r, ""))
				return
			}

			key := r.Header.Get("x-api-key")
			if key == "" {
				if b := r.Header.Get("Authorization"); len(b) > 7 && b[:7] == "Bearer " {
					key = b[7:]
				}
			}

			valid := false
			for _, k := range p.Keys {
				if k == key {
					valid = true
					break
				}
			}

			if !valid {
				message := "invalid API key. Generate one at /admin/ or set x-api-key header"
				if strings.Contains(r.URL.Path, "/messages") {
					writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", message)
				} else {
					writeOpenAIError(w, http.StatusUnauthorized, "authentication_error", message)
				}
				return
			}
			next(w, requestWithTenantScope(r, key))
		})
	}

	modelsHandler := apiKeyHandler(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, buildModelsResponse(getListedModels(), isAnthropicModelsRequest(r)))
	})
	mux.HandleFunc("/v1/models", modelsHandler)
	mux.HandleFunc("/models", modelsHandler)

	chatHandler := apiKeyHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}

		body, err := readLimitedRequestBody(w, r, maxProxyRequestBodyBytes)
		if err != nil {
			writeOpenAIError(w, requestBodyErrorStatus(err), "invalid_request_error", err.Error())
			return
		}

		var params map[string]any
		if err := json.Unmarshal(body, &params); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}

		isStream, _ := params["stream"].(bool)
		model, _ := params["model"].(string)
		reqLog := newRequestLog("openai", model, isStream)
		if err := validateChatCompletionRequest(params); err != nil {
			finalizeRequestLog(&reqLog, tokenUsage{}, time.Time{}, reqLog.StartedAt, false, err.Error())
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		includeUsage := chatStreamIncludesUsage(params)
		toolCount := 0
		if tools, ok := params["tools"]; ok {
			if t, ok := tools.([]any); ok {
				toolCount = len(t)
			}
		}
		log.Printf("  client: stream=%v tools=%d model=%s", isStream, toolCount, model)

		// Override system prompt from override.md for OpenAI format
		if override := loadOverrideContent(); override != "" {
			if msgs, ok := params["messages"].([]any); ok {
				found := false
				for _, m := range msgs {
					if mm, ok := m.(map[string]any); ok {
						if mm["role"] == "system" {
							mm["content"] = override
							found = true
							break
						}
					}
				}
				if !found {
					params["messages"] = append([]any{map[string]any{"role": "system", "content": override}}, msgs...)
				}
			}
		}
		attachRequestIsolation(params, reqLog.ID, requestTenantScope(r))
		attachProxyRequestContext(params, r.Context())
		configurePromptEchoGuard(&reqLog, params)
		setRequestLogIsolationMetadata(&reqLog, params)
		if customProviderHasModel(model) {
			response, provider, retryCount, err := callCustomProviderChat(r.Context(), params, isStream)
			setRequestLogIsolationMetadata(&reqLog, params)
			reqLog.Upstream = customProviderUpstreamLabel(provider)
			reqLog.RetryCount = retryCount
			if err != nil {
				log.Printf("  custom provider error: %v", err)
				writeOpenAIUpstreamError(w, &reqLog, err)
				return
			}
			defer response.Body.Close()
			if isStream {
				handleStreamResponse(w, response, nil, &reqLog, includeUsage)
			} else {
				handleNonStreamResponse(w, response, nil, &reqLog)
			}
			return
		}

		// 按 model 自动分流：zen 免费模型 / zen 付费拒绝 / 其余走 Cline 池
		route := resolveModelRoute(model)
		switch route.Route {
		case modelRouteReject:
			msg := fmt.Sprintf("model %q is a paid opencode model; only free models are proxied", model)
			finalizeRequestLog(&reqLog, tokenUsage{}, time.Time{}, reqLog.StartedAt, false, msg)
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", msg)
			return
		case modelRouteZenUnavailable:
			msg := fmt.Sprintf("opencode zen is temporarily unavailable and model %q has no compatible cline fallback", model)
			finalizeRequestLog(&reqLog, tokenUsage{}, time.Time{}, reqLog.StartedAt, false, msg)
			writeOpenAIError(w, http.StatusServiceUnavailable, "service_unavailable_error", msg)
			return
		case modelRouteZen:
			reqLog.Upstream = upstreamOpenCode
			zm, _ := resolveZenInfo(model)
			rawSessionID := requestSessionID(params, r.Header)
			out := maybeCompact(params, zm, namespaceCompactSessionID(requestTenantScope(r), rawSessionID))
			if out.changed {
				log.Printf("  chat %s", out.note)
			}
			resp, err := callZenAPI(params, isStream)
			if err != nil {
				log.Printf("  api error: %v", err)
				writeOpenAIUpstreamError(w, &reqLog, err)
				return
			}
			defer resp.Body.Close()
			if isStream {
				prepared, _, prepareErr := prepareSemanticChatStreamWithTimeout(resp, upstreamFirstEventTimeout)
				if prepareErr != nil {
					writeOpenAIUpstreamError(w, &reqLog, prepareErr)
					return
				}
				handleStreamResponse(w, prepared, nil, &reqLog, includeUsage)
			} else {
				handleNonStreamResponse(w, resp, nil, &reqLog)
			}
			return
		}
		if route.Model != "" && route.Model != model {
			params["model"] = route.Model
		}

		if activeCount == 0 && len(loadPool().Accounts) == 0 {
			writeOpenAIError(w, http.StatusUnauthorized, "authentication_error", "No accounts in pool. Run with --add-account or POST /admin/login to add accounts.")
			return
		}

		reqLog.Upstream = upstreamCline
		if !isStream {
			out, acc, err := callClineNonStream(params)
			setRequestLogEffectiveModel(&reqLog, params)
			setRequestLogIsolationMetadata(&reqLog, params)
			if err != nil {
				log.Printf("  api error: %v", err)
				writeOpenAIUpstreamError(w, &reqLog, err)
				return
			}
			if acc != nil {
				reqLog.AccountID = acc.AccountID
				reqLog.AccountEmail = acc.Email
			}
			usage := parseTokenUsage(out["usage"])
			recordTokenUsage(acc, reqLog.Model, usage)
			if chatResponsePromptEchoed(&reqLog, out) {
				reqLog.ErrorCode = promptEchoErrorCode
				finalizeRequestLog(&reqLog, usage, time.Time{}, reqLog.StartedAt, false, errPromptEcho.Error())
				writeOpenAIErrorCode(w, http.StatusBadGateway, "api_error", promptEchoErrorCode, errPromptEcho.Error())
				return
			}
			finalizeRequestLog(&reqLog, usage, time.Time{}, reqLog.StartedAt, true, "")
			writeJSON(w, http.StatusOK, normalizeOpenAIChatResponse(out, reqLog.Model, false))
			return
		}

		resp, acc, retryCount, err := callClineChatStream(params)
		setRequestLogEffectiveModel(&reqLog, params)
		reqLog.RetryCount = retryCount
		setRequestLogIsolationMetadata(&reqLog, params)
		if acc != nil {
			reqLog.AccountID = acc.AccountID
			reqLog.AccountEmail = acc.Email
		}
		if err != nil {
			log.Printf("  api error: %v", err)
			writeOpenAIUpstreamError(w, &reqLog, err)
			return
		}
		defer resp.Body.Close()

		handleStreamResponse(w, resp, acc, &reqLog, includeUsage)
	})
	mux.HandleFunc("/v1/chat/completions", chatHandler)
	mux.HandleFunc("/chat/completions", chatHandler)

	// Anthropic Messages API support
	anthropicHandler := apiKeyHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		handleAnthropicMessages(w, r)
	})
	mux.HandleFunc("/v1/messages", anthropicHandler)
	mux.HandleFunc("/messages", anthropicHandler)
	countTokensHandler := apiKeyHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		handleAnthropicCountTokens(w, r)
	})
	mux.HandleFunc("/v1/messages/count_tokens", countTokensHandler)
	mux.HandleFunc("/messages/count_tokens", countTokensHandler)

	// OpenAI Responses API support（所有上游：zen 免费模型 + Cline 账号池）
	responsesHandler := apiKeyHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		handleResponses(w, r)
	})
	mux.HandleFunc("/v1/responses", responsesHandler)
	mux.HandleFunc("/responses", responsesHandler)
	mux.HandleFunc("/codex-models.json", handleCodexModels)

	if host == "" {
		host = "127.0.0.1"
	}
	addr := fmt.Sprintf("%s:%d", host, port)
	listenHost = host
	listenPort = port
	serverMux = mux
	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}
	serverMu.Lock()
	currentServer = server
	serverMu.Unlock()

	// 启动后台冷却恢复巡检
	startCooldownRecovery()
	// 主动刷新即将过期的账号 Token，避免长连接或空闲后首个请求才触发续期。
	startTokenRefreshScheduler()

	fmt.Println("")
	fmt.Println(strings.Repeat("=", 58))
	fmt.Printf("  Cline Go Proxy %s - No CLI Required\n", appVersion)
	fmt.Println(strings.Repeat("=", 58))
	fmt.Printf("  http://%s\n", addr)
	fmt.Printf("  http://%s/v1\n", addr)
	if !isLoopbackHost(host) {
		for _, ip := range detectLocalIPs() {
			fmt.Printf("  http://%s:%d (LAN)\n", ip, port)
		}
		fmt.Println("  !!! 监听非本机地址，管理后台无鉴权，请确认网络环境安全")
	}
	fmt.Println("  API Key: any value")
	fmt.Printf("  Model:   %s\n", getDefaultModel())
	fmt.Printf("  Accounts: %d total, %d active\n", len(loadPool().Accounts), activeCount)
	if zc := getZenConfig(); zc.Enabled {
		fmt.Printf("  OpenCode: enabled (%s free models)\n", strings.TrimRight(zc.BaseURL, "/"))
	} else {
		fmt.Println("  OpenCode: disabled")
	}
	fmt.Println(strings.Repeat("=", 58))

	return server.ListenAndServe()
}

func corsHandler(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-api-key, anthropic-version, anthropic-beta")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func cleanMessages(messages []any) []any {
	cleaned := make([]any, 0, len(messages))
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			cleaned = append(cleaned, m)
			continue
		}
		cleaned = append(cleaned, msg)
	}
	return cleaned
}

func integerTokenLimit(value any) (int, bool) {
	switch number := value.(type) {
	case float64:
		return int(number), true
	case int:
		return number, true
	case int64:
		return int(number), true
	case json.Number:
		parsed, err := number.Int64()
		return int(parsed), err == nil
	default:
		return 0, false
	}
}

func supportsThinkingToggle(model string) bool {
	return strings.Contains(model, "deepseek-v4") || strings.HasPrefix(model, "z-ai/glm-5.3")
}

func buildUpstreamBody(params map[string]any, stream bool, sessionID string) map[string]any {
	maxTokens := defaultMaxTokens
	if value, ok := integerTokenLimit(params["max_tokens"]); ok {
		maxTokens = value
	} else if value, ok := integerTokenLimit(params["max_completion_tokens"]); ok {
		maxTokens = value
	}

	model := getDefaultModel()
	if m, ok := params["model"].(string); ok && m != "" {
		model = m
	}

	body := map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
	}
	if sessionID != "" {
		body["session_id"] = sessionID
	}

	if msgsRaw, ok := params["messages"]; ok {
		if msgsArr, ok := msgsRaw.([]any); ok {
			body["messages"] = cleanMessages(msgsArr)
		} else {
			body["messages"] = msgsRaw
		}
	}

	if stream {
		body["stream"] = true
	}

	explicitReasoning := false
	if re, ok := params["reasoning_effort"].(string); ok && re != "" {
		body["reasoning_effort"] = re
		explicitReasoning = true
	} else if re, ok := params["reasoningEffort"].(string); ok && re != "" {
		body["reasoning_effort"] = re
		explicitReasoning = true
	}

	hasTools := false
	for _, key := range []string{"tools", "functions"} {
		if tools, ok := params[key].([]any); ok && len(tools) > 0 {
			hasTools = true
			break
		}
	}
	_, explicitThinking := params["thinking"]
	clientStream, _ := params["stream"].(bool)
	if supportsThinkingToggle(model) && !clientStream && maxTokens <= smallAuxiliaryMaxTokens && !hasTools && !explicitReasoning && !explicitThinking {
		body["thinking"] = map[string]any{"type": "disabled"}
	}

	for _, key := range passThroughKeys {
		if val, ok := params[key]; ok {
			body[key] = val
		}
	}

	return body
}

func clineHeaders(token, taskID string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Content-Type", "application/json")
	h.Set("X-Task-ID", taskID)

	cfg := getProxyConfig()
	for k, v := range cfg.Headers {
		h.Set(k, v)
	}

	return h
}

func callClineAPI(params map[string]any, stream bool) (*http.Response, *Account, error) {
	model, _ := params["model"].(string)
	if model == virtualFreeModel {
		return callFreeClineAPI(params, stream)
	}
	acc := pickAccountForModel(model)
	if acc == nil {
		return nil, nil, fmt.Errorf("no active accounts available. Use --login or admin API to add accounts")
	}
	return callClineAPIWithAccount(acc, params, stream)
}

func callFreeClineAPI(params map[string]any, stream bool) (*http.Response, *Account, error) {
	for _, model := range freeModelChain {
		params["model"] = model
		for attempt := 0; attempt < freeModelAttemptsPerModel; attempt++ {
			account := pickAccountForModelStrict(model)
			if account == nil {
				break
			}
			response, usedAccount, err := callClineAPIWithAccount(account, params, stream)
			if err == nil {
				return response, usedAccount, nil
			}
			var accountErr *clineAccountUnavailableError
			if errors.As(err, &accountErr) || upstreamErrorStatus(err) == http.StatusTooManyRequests {
				continue
			}
			return nil, usedAccount, err
		}
	}
	return nil, nil, newUpstreamHTTPError(
		http.StatusTooManyRequests,
		"no eligible accounts available for virtual free model",
	)
}

func callClineAPIWithAccount(acc *Account, params map[string]any, stream bool) (*http.Response, *Account, error) {
	token, err := ensureAccountToken(acc)
	if err != nil {
		return nil, acc, &clineAccountUnavailableError{err: fmt.Errorf("account %s token failed: %w", acc.Email, err)}
	}

	taskID := newUpstreamTaskID()
	recordUpstreamAttempt(params, taskID)
	requestID, requestHash := proxyRequestAuditFields(params)
	body := buildUpstreamBody(params, stream, taskID)

	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return nil, acc, fmt.Errorf("marshal body: %w", err)
	}

	req, err := http.NewRequestWithContext(proxyRequestContext(params), http.MethodPost, clineAPIBase+"/chat/completions", bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, acc, fmt.Errorf("create request: %w", err)
	}
	req.Header = clineHeaders(token, taskID)

	toolCount := 0
	if tools, ok := params["tools"]; ok {
		if t, ok := tools.([]any); ok {
			toolCount = len(t)
		}
	}
	log.Printf("  upstream: request=%s task=%s request_hmac_sha256=%s account=%s stream=%v tools=%d msgs=%d max_tokens=%v effort=%v thinking=%v",
		requestID, taskID, requestHash, truncateEmail(acc.Email), stream, toolCount, getMsgCount(params),
		body["max_tokens"], body["reasoning_effort"], body["thinking"])

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("  upstream audit: request=%s task=%s response_prefix_hmac_sha256=none error=transport", requestID, taskID)
		poolMu.Lock()
		acc.Status = "cooldown"
		acc.CooldownUntil = time.Now().Add(5 * time.Minute)
		poolMu.Unlock()
		savePool()
		return nil, acc, &clineAccountUnavailableError{err: fmt.Errorf("upstream request: %w", err)}
	}

	resp = wrapAuditedResponse(resp, requestID, taskID)
	if resp.StatusCode == 401 {
		resp.Body.Close()
		// Refresh token and retry
		if err := refreshAccountToken(acc); err == nil {
			token = currentAccountAccessToken(acc)
			req.Header = clineHeaders(token, taskID)
			req.Body = io.NopCloser(bytes.NewReader(bodyJSON))
			resp, err = httpClient.Do(req)
			if err != nil {
				log.Printf("  upstream audit: request=%s task=%s response_prefix_hmac_sha256=none error=retry_transport", requestID, taskID)
				poolMu.Lock()
				acc.Status = "cooldown"
				acc.CooldownUntil = time.Now().Add(5 * time.Minute)
				poolMu.Unlock()
				savePool()
				return nil, acc, &clineAccountUnavailableError{err: fmt.Errorf("upstream retry: %w", err)}
			}
			resp = wrapAuditedResponse(resp, requestID, taskID)
			if resp.StatusCode == 401 {
				resp.Body.Close()
				poolMu.Lock()
				acc.Status = "expired"
				poolMu.Unlock()
				savePool()
				return nil, acc, &clineAccountUnavailableError{err: fmt.Errorf("account %s token expired permanently", acc.Email)}
			}
		} else {
			poolMu.Lock()
			acc.Status = "expired"
			poolMu.Unlock()
			savePool()
			return nil, acc, &clineAccountUnavailableError{err: fmt.Errorf("account %s refresh failed: %w", acc.Email, err)}
		}
	}

	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		bodyStr := string(bodyBytes)
		// 429：模型级冷却 —— 只暂停该模型，账号保持可用，其他模型继续转发
		if resp.StatusCode == 429 {
			model, _ := body["model"].(string)
			until := parseCooldownUntil(bodyStr)
			if model != "" {
				setModelCooldown(acc, model, until)
			} else {
				poolMu.Lock()
				acc.Status = "cooldown"
				acc.CooldownUntil = until
				poolMu.Unlock()
				savePool()
			}
		}
		return nil, acc, newUpstreamHTTPError(resp.StatusCode, bodyStr)
	}

	poolMu.Lock()
	acc.LastUsed = time.Now()
	acc.UsageCount++
	poolMu.Unlock()
	savePoolEventually()
	return resp, acc, nil
}

type accountTestResult struct {
	AccountID    string `json:"accountId"`
	Email        string `json:"email"`
	OK           bool   `json:"ok"`
	DurationMs   int64  `json:"durationMs"`
	InputTokens  int64  `json:"inputTokens"`
	OutputTokens int64  `json:"outputTokens"`
	Error        string `json:"error,omitempty"`
}

// parseCooldownUntil 从 429 响应体中解析 "Try again in 1h 1m" 格式的等待时长，
// 返回预计恢复时间；解析失败则回退到 1 小时后。
var cooldownRe = regexp.MustCompile(`(?i)try\s+again\s+in\s+(\d+)\s*h?(?:\s*(\d+))?\s*m?`)

func parseCooldownUntil(body string) time.Time {
	matches := cooldownRe.FindStringSubmatch(body)
	if len(matches) >= 2 {
		hours, _ := strconv.Atoi(matches[1])
		minutes := 0
		if len(matches) >= 3 && matches[2] != "" {
			minutes, _ = strconv.Atoi(matches[2])
		}
		if hours > 0 || minutes > 0 {
			return time.Now().Add(time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute)
		}
	}
	// 解析失败，回退 1 小时
	return time.Now().Add(1 * time.Hour)
}

// startCooldownRecovery 启动后台 goroutine，每 30 秒检查一次 cooldown 账号，
// 对 CooldownUntil 已过期的账号执行探活，成功则自动激活。
func startCooldownRecovery() {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			p := loadPool()
			poolMu.Lock()
			var toRecover []*Account
			for _, acc := range p.Accounts {
				if acc.Status != "cooldown" {
					continue
				}
				// 有恢复时间且已过期 → 探活
				// 无恢复时间（旧数据）→ 也尝试探活
				if acc.CooldownUntil.IsZero() || time.Now().After(acc.CooldownUntil) {
					toRecover = append(toRecover, acc)
				}
			}
			poolMu.Unlock()

			for _, acc := range toRecover {
				log.Printf("cooldown recovery: testing %s", acc.Email)
				result := testAccount(acc)
				if result.OK {
					log.Printf("cooldown recovery: %s reactivated", acc.Email)
				} else {
					log.Printf("cooldown recovery: %s still unavailable: %s", acc.Email, result.Error)
				}
			}
		}
	}()
}

// testAccount sends a minimal "hi" request through a specific account to verify
// it can complete an upstream call. It does not update aggregate token counters
// or request logs; it is a diagnostic-only probe.
func testAccount(acc *Account) accountTestResult {
	result := accountTestResult{AccountID: acc.AccountID, Email: acc.Email}
	started := time.Now()

	params := map[string]any{
		"model":      getDefaultModel(),
		"max_tokens": 16,
		"stream":     false,
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	}

	resp, _, err := callClineAPIWithAccount(acc, params, false)
	if err != nil {
		result.DurationMs = time.Since(started).Milliseconds()
		result.Error = truncate(err.Error(), 200)
		return result
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		result.DurationMs = time.Since(started).Milliseconds()
		result.Error = "read response: " + truncate(err.Error(), 200)
		return result
	}

	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		result.DurationMs = time.Since(started).Milliseconds()
		result.Error = "decode response: " + truncate(err.Error(), 200)
		return result
	}
	if data, ok := obj["data"]; ok {
		if d, ok := data.(map[string]any); ok {
			obj = d
		}
	}
	obj = normalizeOpenAIResponse(obj)
	usage := parseTokenUsage(obj["usage"])

	result.OK = true
	result.DurationMs = time.Since(started).Milliseconds()
	if usage.Valid {
		result.InputTokens = usage.Prompt
		result.OutputTokens = usage.Completion
	}
	// If the account was in cooldown/expired but the test succeeded, restore it.
	poolMu.Lock()
	wasInactive := acc.Status != "active"
	if wasInactive {
		acc.Status = "active"
	}
	poolMu.Unlock()
	if wasInactive {
		savePool()
	}
	return result
}

type tokenUsage struct {
	Prompt     int64
	Completion int64
	Total      int64
	Cached     int64
	CacheRead  int64
	CacheWrite int64
	Reasoning  int64
	Valid      bool
}

func tokenCount(value any) int64 {
	switch number := value.(type) {
	case float64:
		return int64(number)
	case int:
		return int64(number)
	case int64:
		return number
	case json.Number:
		parsed, _ := number.Int64()
		return parsed
	default:
		return 0
	}
}

func parseTokenUsage(value any) tokenUsage {
	usage, ok := value.(map[string]any)
	if !ok {
		return tokenUsage{}
	}
	read := func(keys ...string) int64 {
		for _, key := range keys {
			if value := tokenCount(usage[key]); value >= 0 {
				if _, exists := usage[key]; exists {
					return value
				}
			}
		}
		return 0
	}
	readNested := func(parent string, keys ...string) int64 {
		details, ok := usage[parent].(map[string]any)
		if !ok {
			return 0
		}
		for _, key := range keys {
			if value := tokenCount(details[key]); value >= 0 {
				if _, exists := details[key]; exists {
					return value
				}
			}
		}
		return 0
	}
	prompt := read("prompt_tokens", "input_tokens")
	completion := read("completion_tokens", "output_tokens")
	nestedCacheRead := readNested("prompt_tokens_details", "cached_tokens")
	if nestedCacheRead == 0 {
		nestedCacheRead = readNested("input_tokens_details", "cached_tokens")
	}
	cacheRead := nestedCacheRead
	if cacheRead == 0 {
		cacheRead = read("cache_read_input_tokens", "prompt_cache_hit_tokens", "cached_tokens")
	}
	cacheWrite := readNested("prompt_tokens_details", "cache_write_tokens")
	if cacheWrite == 0 {
		cacheWrite = readNested("input_tokens_details", "cache_write_tokens")
	}
	if cacheWrite == 0 {
		cacheWrite = read("cache_creation_input_tokens", "prompt_cache_creation_tokens")
	}
	reasoning := readNested("completion_tokens_details", "reasoning_tokens", "thinking_tokens")
	if reasoning == 0 {
		reasoning = readNested("output_tokens_details", "reasoning_tokens", "thinking_tokens")
	}
	cached := cacheRead + cacheWrite
	if nestedCacheRead > 0 {
		cached = cacheRead
	}
	total := read("total_tokens")
	if total == 0 {
		total = prompt + completion
	}
	_, hasUsage := usage["prompt_tokens"]
	if !hasUsage {
		_, hasUsage = usage["input_tokens"]
		if !hasUsage {
			if _, hasUsage = usage["completion_tokens"]; !hasUsage {
				if _, hasUsage = usage["output_tokens"]; !hasUsage {
					_, hasUsage = usage["total_tokens"]
				}
			}
		}
	}
	if !hasUsage {
		_, hasUsage = usage["cache_read_input_tokens"]
		if !hasUsage {
			_, hasUsage = usage["cache_creation_input_tokens"]
			if !hasUsage {
				_, hasUsage = usage["prompt_tokens_details"]
				if !hasUsage {
					_, hasUsage = usage["input_tokens_details"]
				}
			}
		}
	}
	return tokenUsage{
		Prompt: prompt, Completion: completion, Total: total,
		Cached: cached, CacheRead: cacheRead, CacheWrite: cacheWrite, Reasoning: reasoning,
		Valid: hasUsage,
	}
}

func mergeTokenUsage(current, next tokenUsage) tokenUsage {
	if !next.Valid {
		return current
	}
	if next.Prompt != 0 {
		current.Prompt = next.Prompt
	}
	if next.Completion != 0 {
		current.Completion = next.Completion
	}
	if next.Total != 0 {
		current.Total = next.Total
	}
	if next.Cached != 0 {
		current.Cached = next.Cached
	}
	if next.CacheRead != 0 {
		current.CacheRead = next.CacheRead
	}
	if next.CacheWrite != 0 {
		current.CacheWrite = next.CacheWrite
	}
	if next.Reasoning != 0 {
		current.Reasoning = next.Reasoning
	}
	current.Valid = current.Valid || next.Valid
	if current.Total == 0 && (current.Prompt != 0 || current.Completion != 0) {
		current.Total = current.Prompt + current.Completion
	}
	return current
}

func recordTokenUsage(acc *Account, model string, usage tokenUsage) {
	if acc == nil || !usage.Valid {
		return
	}
	// 先判断是否免费模型（getAllModels 会拿 poolMu，必须在持有锁之前计算）
	isFree := model != "" && isFreeModelID(model)
	poolMu.Lock()
	acc.PromptTokens += usage.Prompt
	acc.CompletionTokens += usage.Completion
	acc.TotalTokens += usage.Total
	acc.CachedTokens += usage.Cached
	// 按模型细分统计（仅记录 free 模型）
	if isFree {
		if acc.ModelStats == nil {
			acc.ModelStats = make(map[string]*ModelStat)
		}
		st := acc.ModelStats[model]
		if st == nil {
			st = &ModelStat{ModelID: model, Cost: "free"}
			acc.ModelStats[model] = st
		}
		st.UsageCount++
		st.PromptTokens += usage.Prompt
		st.CompletionTokens += usage.Completion
		st.TotalTokens += usage.Total
		st.CachedTokens += usage.Cached
	}
	poolMu.Unlock()
	savePoolEventually()
}

// isFreeModelID 判断模型是否为 free 计费（用于按模型统计和模型级冷却）。
func isFreeModelID(model string) bool {
	for _, m := range getAllModels() {
		if m.ID == model {
			return m.Cost == "free"
		}
	}
	// 未知模型：按 ID 后缀/前缀启发式判断
	return strings.HasSuffix(model, ":free") || strings.Contains(model, "/free/")
}

// modelCooldownActive 判断某账号下该模型是否处于模型级冷却中。
func modelCooldownActive(acc *Account, model string) bool {
	if acc == nil || model == "" {
		return false
	}
	poolMu.Lock()
	until, ok := acc.ModelCooldowns[model]
	if !ok {
		poolMu.Unlock()
		return false
	}
	if time.Now().After(until) {
		delete(acc.ModelCooldowns, model)
		poolMu.Unlock()
		savePoolEventually()
		return false
	}
	poolMu.Unlock()
	return true
}

// setModelCooldown 记录模型级冷却（429 时调用）：只暂停该模型，账号保持可用。
// fallback 为解析失败时的恢复时长（默认 1 小时）。
func setModelCooldown(acc *Account, model string, until time.Time) {
	if acc == nil || model == "" {
		return
	}
	poolMu.Lock()
	if acc.ModelCooldowns == nil {
		acc.ModelCooldowns = make(map[string]time.Time)
	}
	acc.ModelCooldowns[model] = until
	poolMu.Unlock()
	savePool()
	log.Printf("model cooldown: account=%s model=%s until=%s", truncateEmail(acc.Email), model, until.Format("15:04:05"))
}

func truncateEmail(email string) string {
	if len(email) <= 12 {
		return email
	}
	parts := splitEmail(email)
	if len(parts) == 2 && len(parts[0]) > 3 {
		return parts[0][:3] + "***@" + parts[1]
	}
	if len(email) > 12 {
		return email[:8] + "..."
	}
	return email
}

func splitEmail(email string) []string {
	for i := 0; i < len(email); i++ {
		if email[i] == '@' {
			return []string{email[:i], email[i+1:]}
		}
	}
	return []string{email}
}

func getMsgCount(params map[string]any) int {
	if msgs, ok := params["messages"].([]any); ok {
		return len(msgs)
	}
	return 0
}

func hasStandardChatOutput(obj map[string]any) bool {
	choices, _ := obj["choices"].([]any)
	for _, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		if choice == nil {
			continue
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			delta, _ = choice["message"].(map[string]any)
		}
		if delta == nil {
			continue
		}
		if content, _ := delta["content"].(string); content != "" {
			return true
		}
		if refusal, _ := delta["refusal"].(string); refusal != "" {
			return true
		}
		if toolCalls, _ := delta["tool_calls"].([]any); len(toolCalls) > 0 {
			return true
		}
		if delta["function_call"] != nil {
			return true
		}
	}
	return false
}

func shouldForwardStandardChatChunk(obj map[string]any) bool {
	if obj["moderation"] != nil {
		return true
	}
	choices, _ := obj["choices"].([]any)
	for _, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		if choice == nil {
			continue
		}
		if choice["finish_reason"] != nil {
			return true
		}
		if delta, _ := choice["delta"].(map[string]any); len(delta) > 0 {
			return true
		}
	}
	return false
}

func handleStreamResponse(w http.ResponseWriter, upstream *http.Response, acc *Account, reqLog *RequestLog, includeUsage bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Printf("  streaming not supported for client")
		return
	}

	reader := bufio.NewReader(upstream.Body)
	var latestUsage tokenUsage
	var latestUsagePayload map[string]any
	var firstOutputAt time.Time
	streamID := ""
	streamCreated := any(nil)
	streamModel := ""
	streamOptional := map[string]any{}
	finishReason := ""
	sawDone := false
	hasOutput := false
	var streamFailure error
	var visibleRepetition outputRepetitionGuard
	var reasoningRepetition outputRepetitionGuard
	for {
		line, readErr := reader.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(line, "data:") {
				payload := strings.TrimSpace(line[5:])
				if payload == "[DONE]" {
					sawDone = true
				} else if payload != "" {
					var obj map[string]any
					if err := json.Unmarshal([]byte(payload), &obj); err != nil {
						streamFailure = fmt.Errorf("decode upstream SSE event: %w", err)
						break
					}
					obj = unwrapDataEnvelope(obj)
					if upstreamError := obj["error"]; upstreamError != nil {
						streamFailure = streamError(upstreamError)
						break
					}
					if usage := parseTokenUsage(obj["usage"]); usage.Valid {
						latestUsage = mergeTokenUsage(latestUsage, usage)
					}
					if usage := normalizeChatUsage(obj["usage"]); usage != nil {
						latestUsagePayload = usage
					}
					eventAt := time.Now()
					rawDelta, _ := getNested(obj, "choices", 0, "delta").(map[string]any)
					if rawDelta == nil {
						rawDelta, _ = getNested(obj, "choices", 0, "message").(map[string]any)
					}
					content, _ := rawDelta["content"].(string)
					reasoning := reasoningContent(rawDelta)
					if requestPromptEchoed(reqLog, reasoning, content) {
						streamFailure = errPromptEcho
						break
					}
					if visibleRepetition.Observe(content) {
						streamFailure = errRepetitiveOutput
						break
					}
					if reasoningRepetition.Observe(reasoning) {
						streamFailure = errRepetitiveOutput
						break
					}
					recordRequestLatencyPhases(reqLog, eventAt, reasoning != "", false)

					normalized := normalizeOpenAIChatResponse(obj, reqLog.Model, true)
					if streamID == "" {
						streamID, _ = normalized["id"].(string)
					}
					if streamCreated == nil {
						streamCreated = normalized["created"]
					}
					if model, _ := normalized["model"].(string); streamModel == "" && model != "" {
						streamModel = model
					}
					normalized["id"] = streamID
					normalized["created"] = streamCreated
					normalized["model"] = streamModel
					for _, field := range []string{"obfuscation", "service_tier", "system_fingerprint"} {
						if value, exists := normalized[field]; exists {
							streamOptional[field] = value
						}
					}
					if includeUsage {
						normalized["usage"] = nil
					} else {
						delete(normalized, "usage")
					}
					if reason, _ := getNested(normalized, "choices", 0, "finish_reason").(string); reason != "" {
						finishReason = reason
					}
					if hasStandardChatOutput(normalized) {
						recordRequestLatencyPhases(reqLog, eventAt, false, true)
						hasOutput = true
						if firstOutputAt.IsZero() {
							firstOutputAt = eventAt
						}
					}
					if shouldForwardStandardChatChunk(normalized) {
						normalizedBytes, err := json.Marshal(normalized)
						if err != nil {
							streamFailure = fmt.Errorf("encode Chat Completions chunk: %w", err)
							break
						}
						w.Write([]byte("data: " + string(normalizedBytes) + "\n\n"))
						flusher.Flush()
					}
				}
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				streamFailure = fmt.Errorf("read upstream SSE: %w", readErr)
			}
			break
		}
	}
	if streamFailure == nil && !sawDone && finishReason == "" {
		streamFailure = errStreamEarlyEOF
	}
	if streamFailure == nil && !hasOutput {
		streamFailure = errEmptyResponseContent
	}
	reqLog.FinishReason = finishReason
	reqLog.SawDone = sawDone
	completed := streamFailure == nil
	errorMessage := ""
	if streamFailure != nil {
		_, _, errorCode := responsesUpstreamErrorDetails(streamFailure)
		reqLog.ErrorCode = errorCode
		errorMessage = streamFailure.Error()
		payload, _ := json.Marshal(map[string]any{
			"error": map[string]any{
				"message": errorMessage, "type": "api_error", "param": nil, "code": errorCode,
			},
		})
		w.Write([]byte("data: " + string(payload) + "\n\n"))
		flusher.Flush()
	} else {
		if includeUsage && latestUsagePayload != nil {
			usageChunk := map[string]any{
				"id": streamID, "object": "chat.completion.chunk", "created": streamCreated,
				"model": streamModel, "choices": []any{}, "usage": latestUsagePayload,
			}
			for field, value := range streamOptional {
				usageChunk[field] = value
			}
			payload, _ := json.Marshal(usageChunk)
			w.Write([]byte("data: " + string(payload) + "\n\n"))
		}
		w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}
	recordTokenUsage(acc, reqLog.Model, latestUsage)
	finalizeRequestLog(reqLog, latestUsage, firstOutputAt, reqLog.StartedAt, completed, errorMessage)
}

func hasFirstOutput(obj map[string]any) bool {
	choices, ok := getNested(obj, "choices").([]any)
	if !ok || len(choices) == 0 {
		return false
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return false
	}
	if delta, ok := choice["delta"].(map[string]any); ok {
		if reasoningContent(delta) != "" {
			return true
		}
		if c, _ := delta["content"].(string); c != "" {
			return true
		}
		if tc, ok := delta["tool_calls"].([]any); ok && len(tc) > 0 {
			return true
		}
	}
	if msg, ok := choice["message"].(map[string]any); ok {
		if reasoningContent(msg) != "" {
			return true
		}
		if c, _ := msg["content"].(string); c != "" {
			return true
		}
		if tc, ok := msg["tool_calls"].([]any); ok && len(tc) > 0 {
			return true
		}
	}
	return false
}

func handleNonStreamResponse(w http.ResponseWriter, upstream *http.Response, acc *Account, reqLog *RequestLog) {
	var raw map[string]any
	if err := json.NewDecoder(upstream.Body).Decode(&raw); err != nil {
		finalizeRequestLog(reqLog, tokenUsage{}, time.Time{}, reqLog.StartedAt, false, "decode response: "+err.Error())
		writeOpenAIError(w, http.StatusBadGateway, "api_error", "decode response: "+err.Error())
		return
	}

	// Some Cline responses wrap in {data: {...}}
	out := raw
	if data, ok := raw["data"]; ok {
		if d, ok := data.(map[string]any); ok {
			out = d
		}
	}

	out = normalizeOpenAIResponse(out)
	usage := parseTokenUsage(out["usage"])
	if chatResponsePromptEchoed(reqLog, out) {
		recordTokenUsage(acc, reqLog.Model, usage)
		reqLog.ErrorCode = promptEchoErrorCode
		finalizeRequestLog(reqLog, usage, time.Time{}, reqLog.StartedAt, false, errPromptEcho.Error())
		writeOpenAIErrorCode(w, http.StatusBadGateway, "api_error", promptEchoErrorCode, errPromptEcho.Error())
		return
	}
	recordTokenUsage(acc, reqLog.Model, usage)
	finalizeRequestLog(reqLog, usage, time.Time{}, reqLog.StartedAt, true, "")
	out = normalizeOpenAIChatResponse(out, reqLog.Model, false)

	if msg, ok := getNested(out, "choices", 0, "message").(map[string]any); ok {
		tc, _ := msg["tool_calls"].([]any)
		content, _ := msg["content"].(string)
		log.Printf("  nonstream finish=%v tool_calls=%d content_len=%d",
			getNested(out, "choices", 0, "finish_reason"),
			len(tc), len(content))
	}

	writeJSON(w, http.StatusOK, out)
}

// Anthropic Messages API support
type anthropicMsg struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type toolAccumulator struct {
	id           string
	name         string
	args         string
	started      bool
	contentIndex int
	emittedArgs  int
}

type anthropicReq struct {
	Model        string          `json:"model"`
	MaxTokens    *int            `json:"max_tokens"`
	Messages     []anthropicMsg  `json:"messages"`
	System       json.RawMessage `json:"system,omitempty"`
	Stream       bool            `json:"stream,omitempty"`
	Temperature  *float64        `json:"temperature,omitempty"`
	TopP         *float64        `json:"top_p,omitempty"`
	TopK         *int            `json:"top_k,omitempty"`
	Stop         json.RawMessage `json:"stop_sequences,omitempty"`
	Tools        json.RawMessage `json:"tools,omitempty"`
	ToolChoice   json.RawMessage `json:"tool_choice,omitempty"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
	OutputConfig json.RawMessage `json:"output_config,omitempty"`
	Thinking     json.RawMessage `json:"thinking,omitempty"`
	Extra        map[string]any  `json:"-"`
}

var (
	overrideCacheMu      sync.Mutex
	overrideCachePath    string
	overrideCacheContent string
	overrideCacheChecked time.Time
)

func loadOverrideContent() string {
	path := resolveDataPath("override.md")
	overrideCacheMu.Lock()
	if path == overrideCachePath && time.Since(overrideCacheChecked) < time.Second {
		content := overrideCacheContent
		overrideCacheMu.Unlock()
		return content
	}
	overrideCacheMu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("  override.md read failed: %v", err)
		}
		data = nil
	}
	content := strings.TrimSpace(string(data))
	overrideCacheMu.Lock()
	overrideCachePath = path
	overrideCacheContent = content
	overrideCacheChecked = time.Now()
	overrideCacheMu.Unlock()
	if content != "" {
		log.Printf("  using override.md as system prompt (%d bytes)", len(content))
	}
	return content
}

func extractStringContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try string first
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Try array of content blocks
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err == nil {
		parts := []string{}
		for _, b := range blocks {
			if b["type"] == "text" {
				if t, ok := b["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func anthropicToolsToOpenAI(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		if tMap, ok := t.(map[string]any); ok {
			// Already in OpenAI format
			if tMap["type"] == "function" {
				out = append(out, t)
				continue
			}
			// Convert Anthropic format to OpenAI
			function := map[string]any{
				"name":        tMap["name"],
				"description": tMap["description"],
				"parameters":  tMap["input_schema"],
			}
			if strict, ok := tMap["strict"].(bool); ok {
				function["strict"] = strict
			}
			oai := map[string]any{
				"type":     "function",
				"function": function,
			}
			out = append(out, oai)
		}
	}
	return out
}

func anthropicToolChoiceToOpenAI(raw json.RawMessage) (any, *bool) {
	if len(raw) == 0 {
		return nil, nil
	}
	var choice map[string]any
	if err := json.Unmarshal(raw, &choice); err != nil {
		return nil, nil
	}
	var mapped any
	switch choice["type"] {
	case "auto":
		mapped = "auto"
	case "any":
		mapped = "required"
	case "none":
		mapped = "none"
	case "tool":
		if name, _ := choice["name"].(string); name != "" {
			mapped = map[string]any{
				"type":     "function",
				"function": map[string]any{"name": name},
			}
		}
	}
	var parallel *bool
	if disabled, ok := choice["disable_parallel_tool_use"].(bool); ok {
		enabled := !disabled
		parallel = &enabled
	}
	return mapped, parallel
}

func anthropicOutputConfigToOpenAI(raw json.RawMessage) (string, map[string]any) {
	if len(raw) == 0 {
		return "", nil
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		return "", nil
	}
	effort, _ := config["effort"].(string)
	format, _ := config["format"].(map[string]any)
	if format == nil {
		return effort, nil
	}
	switch format["type"] {
	case "json_schema":
		return effort, map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "anthropic_output",
				"schema": format["schema"],
				"strict": true,
			},
		}
	case "json_object":
		return effort, map[string]any{"type": "json_object"}
	}
	return effort, nil
}

func anthropicThinkingType(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var thinking map[string]any
	if err := json.Unmarshal(raw, &thinking); err != nil {
		return ""
	}
	thinkingType, _ := thinking["type"].(string)
	return thinkingType
}

func configuredAnthropicEffort() string {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()
	switch p.AnthropicEffort {
	case "low", "medium", "high":
		return p.AnthropicEffort
	default:
		return "high"
	}
}

func validateAnthropicCompatibility(req anthropicReq) error {
	if req.Model == "" {
		return fmt.Errorf("model is required")
	}
	if req.MaxTokens != nil && *req.MaxTokens < 0 {
		return fmt.Errorf("max_tokens must be greater than or equal to 0")
	}
	if len(req.Tools) > 0 {
		var tools []any
		if err := json.Unmarshal(req.Tools, &tools); err != nil {
			return fmt.Errorf("invalid tools: %w", err)
		}
		for _, tool := range tools {
			toolMap, _ := tool.(map[string]any)
			if toolMap == nil {
				return fmt.Errorf("invalid tool definition")
			}
			if toolMap["name"] == nil || toolMap["input_schema"] == nil {
				return fmt.Errorf("only Anthropic client tools with name and input_schema can be mapped to the upstream chat API")
			}
		}
	}
	return nil
}

func anthropicImageToOpenAI(block map[string]any) (map[string]any, bool) {
	source, _ := block["source"].(map[string]any)
	if source == nil {
		return nil, false
	}
	var imageURL string
	switch source["type"] {
	case "base64":
		mediaType, _ := source["media_type"].(string)
		data, _ := source["data"].(string)
		if mediaType != "" && data != "" {
			imageURL = "data:" + mediaType + ";base64," + data
		}
	case "url":
		imageURL, _ = source["url"].(string)
	}
	if imageURL == "" {
		return nil, false
	}
	return map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": imageURL},
	}, true
}

func anthropicToolResultContent(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		parts := make([]string, 0, len(value))
		allText := true
		for _, item := range value {
			block, _ := item.(map[string]any)
			if block["type"] != "text" {
				allText = false
			}
			if text, _ := block["text"].(string); text != "" {
				parts = append(parts, text)
			}
		}
		if allText && len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func anthropicContentToOpenAI(blocks []any) (any, []any, []any, string) {
	contentParts := []any{}
	toolCalls := []any{}
	toolResults := []any{}
	reasoningParts := []string{}
	allText := true
	textParts := []string{}

	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch block["type"] {
		case "text":
			if text, _ := block["text"].(string); text != "" {
				textParts = append(textParts, text)
				contentParts = append(contentParts, map[string]any{"type": "text", "text": text})
			}
		case "image":
			if image, ok := anthropicImageToOpenAI(block); ok {
				allText = false
				contentParts = append(contentParts, image)
			}
		case "document":
			source, _ := block["source"].(map[string]any)
			if source != nil && source["type"] == "text" {
				if data, _ := source["data"].(string); data != "" {
					textParts = append(textParts, data)
					contentParts = append(contentParts, map[string]any{"type": "text", "text": data})
				}
			}
		case "tool_use":
			arguments := "{}"
			if input := block["input"]; input != nil {
				if value, ok := input.(string); ok {
					arguments = value
				} else if encoded, err := json.Marshal(input); err == nil {
					arguments = string(encoded)
				}
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   block["id"],
				"type": "function",
				"function": map[string]any{
					"name":      block["name"],
					"arguments": arguments,
				},
			})
		case "tool_result":
			result := anthropicToolResultContent(block["content"])
			if isError, _ := block["is_error"].(bool); isError {
				result = "[tool_error] " + result
			}
			toolResults = append(toolResults, map[string]any{
				"role":         "tool",
				"content":      result,
				"tool_call_id": block["tool_use_id"],
			})
		case "thinking":
			if thinking, _ := block["thinking"].(string); thinking != "" {
				reasoningParts = append(reasoningParts, thinking)
			}
		}
	}

	if len(contentParts) == 0 {
		return "", toolCalls, toolResults, strings.Join(reasoningParts, "\n")
	}
	if allText {
		return strings.Join(textParts, "\n"), toolCalls, toolResults, strings.Join(reasoningParts, "\n")
	}
	return contentParts, toolCalls, toolResults, strings.Join(reasoningParts, "\n")
}

func anthropicToOpenAI(req anthropicReq) map[string]any {
	maxTokens := defaultMaxTokens
	if req.MaxTokens != nil {
		maxTokens = *req.MaxTokens
	}
	openAI := map[string]any{
		"model":      req.Model,
		"max_tokens": maxTokens,
		"stream":     req.Stream,
		"messages":   []any{},
	}
	if req.Temperature != nil {
		openAI["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		openAI["top_p"] = *req.TopP
	}
	if req.TopK != nil {
		openAI["top_k"] = *req.TopK
	}
	if len(req.Stop) > 0 {
		var stop any
		if json.Unmarshal(req.Stop, &stop) == nil {
			openAI["stop"] = stop
		}
	}
	if len(req.Tools) > 0 {
		var tools []any
		if json.Unmarshal(req.Tools, &tools) == nil {
			openAI["tools"] = anthropicToolsToOpenAI(tools)
		}
	}
	if toolChoice, parallel := anthropicToolChoiceToOpenAI(req.ToolChoice); toolChoice != nil {
		openAI["tool_choice"] = toolChoice
		if parallel != nil {
			openAI["parallel_tool_calls"] = *parallel
		}
	}
	effort, format := anthropicOutputConfigToOpenAI(req.OutputConfig)
	if effort != "" {
		openAI["reasoning_effort"] = effort
	}
	if format != nil {
		openAI["response_format"] = format
	}
	switch anthropicThinkingType(req.Thinking) {
	case "disabled":
		openAI["thinking"] = map[string]any{"type": "disabled"}
	case "adaptive", "enabled":
		if effort == "" {
			openAI["reasoning_effort"] = configuredAnthropicEffort()
		}
	}
	if len(req.Metadata) > 0 {
		var metadata map[string]any
		if json.Unmarshal(req.Metadata, &metadata) == nil {
			if userID, _ := metadata["user_id"].(string); userID != "" {
				openAI["user"] = userID
			}
		}
	}

	msgs := []any{}
	systemContent := loadOverrideContent()
	if systemContent == "" && len(req.System) > 0 {
		systemContent = extractStringContent(req.System)
	}
	if systemContent != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": systemContent})
	}

	for _, message := range req.Messages {
		switch content := message.Content.(type) {
		case string:
			msgs = append(msgs, map[string]any{"role": message.Role, "content": content})
		case []any:
			convertedContent, toolCalls, toolResults, reasoningContent := anthropicContentToOpenAI(content)
			if message.Role == "assistant" {
				converted := map[string]any{"role": "assistant", "content": convertedContent}
				if reasoningContent != "" {
					converted["reasoning_content"] = reasoningContent
				}
				if len(toolCalls) > 0 {
					converted["tool_calls"] = toolCalls
				}
				msgs = append(msgs, converted)
				continue
			}
			msgs = append(msgs, toolResults...)
			if text, ok := convertedContent.(string); !ok || text != "" {
				msgs = append(msgs, map[string]any{"role": message.Role, "content": convertedContent})
			}
		}
	}

	openAI["messages"] = msgs
	return openAI
}

func anthropicUsageFromOpenAI(openAI map[string]any) map[string]any {
	inputTokens := int64(0)
	outputTokens := int64(0)
	cacheReadTokens := int64(0)
	cacheCreationTokens := int64(0)
	thinkingTokens := int64(0)
	usage, _ := openAI["usage"].(map[string]any)
	if usage != nil {
		cacheReadTokens = tokenCount(usage["cache_read_input_tokens"])
		cacheCreationTokens = tokenCount(usage["cache_creation_input_tokens"])
		inputDetails, _ := usage["prompt_tokens_details"].(map[string]any)
		if inputDetails == nil {
			inputDetails, _ = usage["input_tokens_details"].(map[string]any)
		}
		if inputDetails != nil {
			if value := tokenCount(inputDetails["cached_tokens"]); value > 0 {
				cacheReadTokens = value
			}
			if value := tokenCount(inputDetails["cache_write_tokens"]); value > 0 {
				cacheCreationTokens = value
			}
		}

		if promptTokens := tokenCount(usage["prompt_tokens"]); promptTokens > 0 {
			// OpenAI prompt_tokens includes cached tokens. Anthropic reports fresh,
			// cache-read, and cache-creation input as separate additive counters.
			inputTokens = promptTokens - cacheReadTokens - cacheCreationTokens
			if inputTokens < 0 {
				inputTokens = 0
			}
		} else {
			inputTokens = tokenCount(usage["input_tokens"])
		}
		outputTokens = tokenCount(usage["completion_tokens"])
		if outputTokens == 0 {
			outputTokens = tokenCount(usage["output_tokens"])
		}
		outputDetails, _ := usage["completion_tokens_details"].(map[string]any)
		if outputDetails == nil {
			outputDetails, _ = usage["output_tokens_details"].(map[string]any)
		}
		if outputDetails != nil {
			thinkingTokens = tokenCount(outputDetails["reasoning_tokens"])
			if thinkingTokens == 0 {
				thinkingTokens = tokenCount(outputDetails["thinking_tokens"])
			}
		}
	}
	return map[string]any{
		"input_tokens":                inputTokens,
		"cache_creation_input_tokens": cacheCreationTokens,
		"cache_read_input_tokens":     cacheReadTokens,
		"output_tokens":               outputTokens,
		"output_tokens_details": map[string]any{
			"thinking_tokens": thinkingTokens,
		},
	}
}

func openAIToAnthropic(openAI map[string]any) map[string]any {
	messageID := newResponseID("msg_")
	out := map[string]any{
		"id":            messageID,
		"type":          "message",
		"role":          "assistant",
		"model":         getNested(openAI, "model"),
		"stop_sequence": nil,
	}

	choices, _ := getNested(openAI, "choices").([]any)
	if len(choices) == 0 {
		out["content"] = []any{map[string]any{"type": "text", "text": ""}}
		out["stop_reason"] = "end_turn"
		out["usage"] = anthropicUsageFromOpenAI(openAI)
		return out
	}

	choice0, _ := choices[0].(map[string]any)
	if choice0 == nil {
		out["content"] = []any{map[string]any{"type": "text", "text": ""}}
		out["stop_reason"] = "end_turn"
		out["usage"] = anthropicUsageFromOpenAI(openAI)
		return out
	}
	msg, _ := choice0["message"].(map[string]any)
	if msg == nil {
		msg, _ = choice0["delta"].(map[string]any)
	}

	text := ""
	if msg != nil {
		if c, ok := msg["content"].(string); ok {
			text = sanitizeContent(c)
		}
	}

	contentBlocks := []any{}
	if reasoning := reasoningContent(msg); reasoning != "" {
		contentBlocks = append(contentBlocks, map[string]any{
			"type":      "thinking",
			"thinking":  reasoning,
			"signature": proxyThinkingSignature(messageID, reasoning),
		})
	}
	if text != "" {
		contentBlocks = append(contentBlocks, map[string]any{"type": "text", "text": text})
	}

	// Convert tool_calls to Anthropic tool_use blocks
	if msg != nil {
		if tc, ok := msg["tool_calls"].([]any); ok && len(tc) > 0 {
			for _, tcItem := range tc {
				if tcMap, ok := tcItem.(map[string]any); ok {
					funcData, _ := tcMap["function"].(map[string]any)
					if funcData == nil {
						continue
					}
					input := funcData["arguments"]
					// OpenAI arguments is a JSON string; Anthropic expects an object
					if argsStr, ok := input.(string); ok {
						var argsObj any
						if json.Unmarshal([]byte(argsStr), &argsObj) == nil {
							input = argsObj
						}
					}
					block := map[string]any{
						"type":  "tool_use",
						"id":    tcMap["id"],
						"name":  funcData["name"],
						"input": input,
					}
					contentBlocks = append(contentBlocks, block)
				}
			}
		}
	}
	if len(contentBlocks) == 0 {
		contentBlocks = append(contentBlocks, map[string]any{"type": "text", "text": ""})
	}

	out["content"] = contentBlocks

	switch getNested(openAI, "choices", 0, "finish_reason") {
	case "stop":
		out["stop_reason"] = "end_turn"
	case "length":
		out["stop_reason"] = "max_tokens"
	case "tool_calls":
		out["stop_reason"] = "tool_use"
	case "content_filter":
		out["stop_reason"] = "refusal"
	default:
		out["stop_reason"] = "end_turn"
	}
	out["usage"] = anthropicUsageFromOpenAI(openAI)

	return out
}

func handleAnthropicChatResponse(w http.ResponseWriter, response *http.Response, reqLog *RequestLog) {
	var raw map[string]any
	if err := json.NewDecoder(response.Body).Decode(&raw); err != nil {
		message := "decode response: " + err.Error()
		finalizeRequestLog(reqLog, tokenUsage{}, time.Time{}, reqLog.StartedAt, false, message)
		writeAnthropicError(w, http.StatusBadGateway, "api_error", message)
		return
	}
	chatResponse := normalizeOpenAIResponse(unwrapDataEnvelope(raw))
	usage := parseTokenUsage(chatResponse["usage"])
	if chatResponsePromptEchoed(reqLog, chatResponse) {
		reqLog.ErrorCode = promptEchoErrorCode
		finalizeRequestLog(reqLog, usage, time.Time{}, reqLog.StartedAt, false, errPromptEcho.Error())
		writeAnthropicError(w, http.StatusBadGateway, "api_error", errPromptEcho.Error())
		return
	}
	finalizeRequestLog(reqLog, usage, time.Time{}, reqLog.StartedAt, true, "")
	writeJSON(w, http.StatusOK, openAIToAnthropic(chatResponse))
}

func handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	body, err := readLimitedRequestBody(w, r, maxProxyRequestBodyBytes)
	if err != nil {
		writeAnthropicError(w, requestBodyErrorStatus(err), "invalid_request_error", err.Error())
		return
	}

	var req anthropicReq
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if err := validateAnthropicCompatibility(req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	if len(req.Messages) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "messages is required")
		return
	}

	if req.MaxTokens == nil {
		maxTokens := defaultMaxTokens
		req.MaxTokens = &maxTokens
	}

	reqLog := newRequestLog("anthropic", req.Model, req.Stream)
	openAIReq := anthropicToOpenAI(req)
	attachRequestIsolation(openAIReq, reqLog.ID, requestTenantScope(r))
	attachProxyRequestContext(openAIReq, r.Context())
	configurePromptEchoGuard(&reqLog, openAIReq)
	setRequestLogIsolationMetadata(&reqLog, openAIReq)
	estimatedInputTokens := estimateRawRequestTokens(body)
	reqLog.EstimatedInputTokens = estimatedInputTokens

	log.Printf("  anthropic: model=%s stream=%v msgs=%d history_reasoning_chars=%d",
		req.Model, req.Stream, len(req.Messages), reasoningHistoryChars(openAIReq))
	if customProviderHasModel(req.Model) {
		response, provider, retryCount, callErr := callCustomProviderChat(r.Context(), openAIReq, req.Stream)
		setRequestLogIsolationMetadata(&reqLog, openAIReq)
		reqLog.Upstream = customProviderUpstreamLabel(provider)
		reqLog.RetryCount = retryCount
		if callErr != nil {
			log.Printf("  anthropic custom provider error: %v", callErr)
			writeAnthropicUpstreamError(w, &reqLog, tokenUsage{}, callErr)
			return
		}
		defer response.Body.Close()
		if req.Stream {
			handleAnthropicStream(w, response, nil, &reqLog, estimatedInputTokens)
		} else {
			handleAnthropicChatResponse(w, response, &reqLog)
		}
		return
	}

	// 按 model 自动分流（与 chat 端点一致）：zen 免费/付费拒绝/Cline 池
	route := resolveModelRoute(req.Model)
	switch route.Route {
	case modelRouteReject:
		msg := fmt.Sprintf("model %q is a paid opencode model; only free models are proxied", req.Model)
		finalizeRequestLog(&reqLog, tokenUsage{}, time.Time{}, reqLog.StartedAt, false, msg)
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", msg)
		return
	case modelRouteZenUnavailable:
		msg := fmt.Sprintf("opencode zen is temporarily unavailable and model %q has no compatible cline fallback", req.Model)
		finalizeRequestLog(&reqLog, tokenUsage{}, time.Time{}, reqLog.StartedAt, false, msg)
		writeAnthropicError(w, 529, "overloaded_error", msg)
		return
	case modelRouteZen:
		reqLog.Upstream = upstreamOpenCode
		zm, _ := resolveZenInfo(req.Model)
		rawSessionID := r.Header.Get("x-opencode-session")
		out := maybeCompact(openAIReq, zm, namespaceCompactSessionID(requestTenantScope(r), rawSessionID))
		if out.changed {
			log.Printf("  anthropic %s", out.note)
		}
		resp, err := callZenAPI(openAIReq, req.Stream)
		if err != nil {
			log.Printf("  anthropic api error: %v", err)
			writeAnthropicUpstreamError(w, &reqLog, tokenUsage{}, err)
			return
		}
		defer resp.Body.Close()
		if req.Stream {
			prepared, diagnostic, prepareErr := prepareSemanticChatStream(resp)
			if prepareErr != nil {
				finalizeRequestLog(&reqLog, diagnostic.Usage, time.Time{}, reqLog.StartedAt, false, prepareErr.Error())
				log.Printf("  anthropic zen stream rejected: finish=%s reasoning_chars=%d thinking_tokens=%d error=%v",
					diagnostic.FinishReason, diagnostic.ReasoningChars, diagnostic.Usage.Reasoning, prepareErr)
				writeAnthropicError(w, http.StatusBadGateway, "api_error", prepareErr.Error())
				return
			}
			handleAnthropicStream(w, prepared, nil, &reqLog, estimatedInputTokens)
		} else {
			handleAnthropicChatResponse(w, resp, &reqLog)
		}
		return
	}
	if route.Model != "" && route.Model != req.Model {
		openAIReq["model"] = route.Model
	}

	activeCount := 0
	p := loadPool()
	for _, a := range p.Accounts {
		if a.Status == "active" {
			activeCount++
		}
	}

	if activeCount == 0 && len(p.Accounts) == 0 {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "No accounts in pool")
		return
	}
	reqLog.Upstream = upstreamCline

	if !req.Stream {
		out, acc, err := callClineNonStream(openAIReq)
		setRequestLogEffectiveModel(&reqLog, openAIReq)
		setRequestLogIsolationMetadata(&reqLog, openAIReq)
		if err != nil {
			log.Printf("  anthropic api error: %v", err)
			writeAnthropicUpstreamError(w, &reqLog, tokenUsage{}, err)
			return
		}
		if acc != nil {
			reqLog.AccountID = acc.AccountID
			reqLog.AccountEmail = acc.Email
		}
		usage := parseTokenUsage(out["usage"])
		recordTokenUsage(acc, reqLog.Model, usage)
		if chatResponsePromptEchoed(&reqLog, out) {
			reqLog.ErrorCode = promptEchoErrorCode
			finalizeRequestLog(&reqLog, usage, time.Time{}, reqLog.StartedAt, false, errPromptEcho.Error())
			writeAnthropicError(w, http.StatusBadGateway, "api_error", errPromptEcho.Error())
			return
		}
		finalizeRequestLog(&reqLog, usage, time.Time{}, reqLog.StartedAt, true, "")
		writeJSON(w, http.StatusOK, openAIToAnthropic(out))
		return
	}

	requestFingerprint := reqLog.RequestHMAC
	if requestFingerprint == "" {
		requestFingerprint = semanticRequestFingerprint(openAIReq)
	}
	if circuitDiagnostic, suppressed := activeSemanticEmptyCircuit(requestFingerprint, time.Now()); suppressed {
		log.Printf("  anthropic semantic-empty circuit: identical request suppressed finish=%s reasoning_chars=%d thinking_tokens=%d",
			circuitDiagnostic.FinishReason, circuitDiagnostic.ReasoningChars, circuitDiagnostic.Usage.Reasoning)
		writeAnthropicSemanticEmpty(w, &reqLog, circuitDiagnostic, true, estimatedInputTokens)
		return
	}

	resp, acc, diagnostic, err := callClineAnthropicStream(openAIReq)
	setRequestLogEffectiveModel(&reqLog, openAIReq)
	reqLog.RetryCount = diagnostic.RetryCount
	setRequestLogIsolationMetadata(&reqLog, openAIReq)
	if err != nil {
		if acc != nil {
			reqLog.AccountID = acc.AccountID
			reqLog.AccountEmail = acc.Email
		}
		log.Printf("  anthropic api error: %v", err)
		log.Printf("  anthropic stream rejected: finish=%s reasoning_chars=%d thinking_tokens=%d",
			diagnostic.FinishReason, diagnostic.ReasoningChars, diagnostic.Usage.Reasoning)
		if isEmptyResponseError(err) {
			rememberSemanticEmptyCircuit(requestFingerprint, diagnostic, time.Now())
			writeAnthropicSemanticEmpty(w, &reqLog, diagnostic, false, estimatedInputTokens)
			return
		}
		writeAnthropicUpstreamError(w, &reqLog, diagnostic.Usage, err)
		return
	}
	defer resp.Body.Close()
	if acc != nil {
		reqLog.AccountID = acc.AccountID
		reqLog.AccountEmail = acc.Email
	}

	handleAnthropicStream(w, resp, acc, &reqLog, estimatedInputTokens)
}

func estimateChatInputTokens(params map[string]any) int {
	input := map[string]any{"messages": params["messages"]}
	for _, key := range []string{"tools", "tool_choice", "response_format"} {
		if value, ok := params[key]; ok {
			input[key] = value
		}
	}
	tokens := estimateJSON(input)
	if tokens < 1 {
		return 1
	}
	return tokens
}

func estimateRawRequestTokens(body []byte) int {
	tokens := len(body) / 4
	if tokens < 1 {
		return 1
	}
	return tokens
}

func reasoningHistoryChars(params map[string]any) int {
	messages, _ := params["messages"].([]any)
	total := 0
	for _, rawMessage := range messages {
		message, _ := rawMessage.(map[string]any)
		total += len([]rune(reasoningContent(message)))
	}
	return total
}

func recordSemanticEmptyDiagnostic(reqLog *RequestLog, diagnostic semanticStreamDiagnostic, retrySuppressed bool) {
	reqLog.ErrorCode = semanticEmptyErrorCode
	reqLog.FinishReason = diagnostic.FinishReason
	reqLog.ReasoningChars = diagnostic.ReasoningChars
	reqLog.ThinkingTokens = diagnostic.Usage.Reasoning
	reqLog.RetryCount = diagnostic.RetryCount
	if attempts := diagnostic.RetryCount + 1; reqLog.UpstreamAttempts < attempts {
		reqLog.UpstreamAttempts = attempts
	}
	reqLog.RetrySuppressed = retrySuppressed
}

func writeAnthropicSemanticEmpty(w http.ResponseWriter, reqLog *RequestLog, diagnostic semanticStreamDiagnostic, retrySuppressed bool, estimatedInputTokens int) {
	recordSemanticEmptyDiagnostic(reqLog, diagnostic, retrySuppressed)
	details := semanticEmptyResponseFor(reqLog.Model, estimatedInputTokens, reqLog.UpstreamAttempts)
	if details.Status == 529 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", semanticEmptyRetryAfterSeconds))
	}
	finalizeRequestLog(reqLog, diagnostic.Usage, time.Time{}, reqLog.StartedAt, false, details.Message)
	writeAnthropicError(w, details.Status, details.ErrorType, details.Message)
}

func handleAnthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	body, err := readLimitedRequestBody(w, r, maxProxyRequestBodyBytes)
	if err != nil {
		writeAnthropicError(w, requestBodyErrorStatus(err), "invalid_request_error", err.Error())
		return
	}
	var req anthropicReq
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if err := validateAnthropicCompatibility(req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "messages is required")
		return
	}
	inputTokens := estimateRawRequestTokens(body)
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": inputTokens})
}

func handleAnthropicStream(w http.ResponseWriter, upstream *http.Response, acc *Account, reqLog *RequestLog, estimatedInputTokens int) {
	log.Printf("  anthropic stream: starting real-time forward")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}

	emit := func(event string, data any) {
		d, _ := json.Marshal(data)
		w.Write([]byte(fmt.Sprintf("event: %s\n", event)))
		w.Write([]byte(fmt.Sprintf("data: %s\n\n", string(d))))
		flusher.Flush()
	}

	msgID := "msg_" + secureRandomHex(16)
	stopReason := "end_turn"
	emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"content":       []any{},
			"model":         reqLog.Model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":                estimatedInputTokens,
				"cache_creation_input_tokens": 0,
				"cache_read_input_tokens":     0,
				"output_tokens":               0,
			},
		},
	})

	nextContentIndex := 0
	textIndex := -1
	hasText := false
	textOpen := false
	thinkingIndex := -1
	thinkingOpen := false
	var thinkingText strings.Builder
	reasoningChars := 0
	pendingTools := map[int]*toolAccumulator{}
	toolOrder := []int{}
	emittedTools := 0
	upstreamFinishReason := ""
	var streamFailure error

	closeTextBlock := func() {
		if !textOpen {
			return
		}
		emit("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": textIndex,
		})
		textOpen = false
	}

	closeThinkingBlock := func() {
		if !thinkingOpen {
			return
		}
		emit("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": thinkingIndex,
			"delta": map[string]any{
				"type":      "signature_delta",
				"signature": proxyThinkingSignature(msgID, thinkingText.String()),
			},
		})
		emit("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": thinkingIndex,
		})
		thinkingOpen = false
		thinkingText.Reset()
	}

	emitToolBlock := func(acc *toolAccumulator, index int) {
		args := strings.TrimSpace(acc.args)
		if args == "" {
			args = "{}"
		}
		emit("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": index,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    acc.id,
				"name":  acc.name,
				"input": map[string]any{},
			},
		})
		emit("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": index,
			"delta": map[string]any{
				"type":         "input_json_delta",
				"partial_json": args,
			},
		})
		emit("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": index,
		})
	}

	startToolBlock := func(acc *toolAccumulator) {
		if acc.started || acc.id == "" || acc.name == "" {
			return
		}
		acc.started = true
		acc.contentIndex = nextContentIndex
		nextContentIndex++
		emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": acc.contentIndex,
			"content_block": map[string]any{"type": "tool_use", "id": acc.id, "name": acc.name, "input": map[string]any{}},
		})
	}

	emitPendingToolArgs := func(acc *toolAccumulator) {
		if !acc.started || acc.emittedArgs >= len(acc.args) {
			return
		}
		emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": acc.contentIndex,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": acc.args[acc.emittedArgs:]},
		})
		acc.emittedArgs = len(acc.args)
	}

	closeStartedTool := func(acc *toolAccumulator) {
		if !acc.started {
			return
		}
		if strings.TrimSpace(acc.args) == "" {
			acc.args = "{}"
		}
		emitPendingToolArgs(acc)
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": acc.contentIndex})
	}

	reader := bufio.NewReader(upstream.Body)
	var latestUsage tokenUsage
	var latestRawUsage map[string]any
	var firstOutputAt time.Time
	var visibleRepetition outputRepetitionGuard
	var reasoningRepetition outputRepetitionGuard

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if !errors.Is(err, io.EOF) {
				streamFailure = fmt.Errorf("read upstream stream: %w", err)
				emit("error", map[string]any{
					"type":  "error",
					"error": map[string]any{"type": "api_error", "message": streamFailure.Error()},
				})
			}
			break
		}
		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[5:])
		if payload == "" || payload == "[DONE]" {
			continue
		}

		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			continue
		}
		if data, ok := obj["data"]; ok {
			if d, ok := data.(map[string]any); ok {
				obj = d
			}
		}
		if rawUsage, ok := obj["usage"].(map[string]any); ok {
			latestRawUsage = rawUsage
		}
		if usage := parseTokenUsage(obj["usage"]); usage.Valid {
			latestUsage = mergeTokenUsage(latestUsage, usage)
		}
		// Detect upstream SSE error
		if errPayload, ok := obj["error"]; ok {
			errBody, _ := json.Marshal(errPayload)
			log.Printf("  upstream SSE error: %s", string(errBody))
			emit("error", map[string]any{"type": "error", "error": errPayload})
			streamFailure = streamError(errPayload)
			break
		}

		choices, _ := getNested(obj, "choices").([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		if choice == nil {
			continue
		}

		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			delta = choice
		}

		eventAt := time.Now()
		reasoning := reasoningContent(delta)
		content, _ := delta["content"].(string)
		toolCalls, _ := delta["tool_calls"].([]any)
		if requestPromptEchoed(reqLog, reasoning, content) {
			streamFailure = errPromptEcho
			reqLog.ErrorCode = promptEchoErrorCode
		} else if visibleRepetition.Observe(content) || reasoningRepetition.Observe(reasoning) {
			streamFailure = errRepetitiveOutput
			reqLog.ErrorCode = repetitiveOutputErrorCode
		}
		if streamFailure != nil {
			emit("error", map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "api_error", "message": streamFailure.Error()},
			})
			break
		}
		visible := content != "" || len(toolCalls) > 0
		recordRequestLatencyPhases(reqLog, eventAt, reasoning != "", visible)
		if firstOutputAt.IsZero() && (reasoning != "" || visible) {
			firstOutputAt = eventAt
		}

		if reasoning != "" {
			closeTextBlock()
			if !thinkingOpen {
				thinkingIndex = nextContentIndex
				nextContentIndex++
				thinkingOpen = true
				emit("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": thinkingIndex,
					"content_block": map[string]any{
						"type":      "thinking",
						"thinking":  "",
						"signature": "",
					},
				})
			}
			thinkingText.WriteString(reasoning)
			reasoningChars += len([]rune(reasoning))
			emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": thinkingIndex,
				"delta": map[string]any{
					"type":     "thinking_delta",
					"thinking": reasoning,
				},
			})
		}

		// Text content delta
		if c, ok := delta["content"].(string); ok && c != "" {
			closeThinkingBlock()
			if !textOpen {
				hasText = true
				textIndex = nextContentIndex
				nextContentIndex++
				textOpen = true
				emit("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": textIndex,
					"content_block": map[string]any{
						"type": "text",
						"text": "",
					},
				})
			}
			emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": textIndex,
				"delta": map[string]any{
					"type": "text_delta",
					"text": sanitizeContent(c),
				},
			})
		}

		// Tool calls - accumulate and emit when complete
		if tcRaw, ok := delta["tool_calls"].([]any); ok {
			if len(tcRaw) > 0 {
				closeThinkingBlock()
				closeTextBlock()
			}
			for _, tc := range tcRaw {
				tcMap, _ := tc.(map[string]any)
				if tcMap == nil {
					continue
				}
				idx := 0
				if i, ok := tcMap["index"].(float64); ok {
					idx = int(i)
				}
				acc, exists := pendingTools[idx]
				if !exists {
					acc = &toolAccumulator{}
					pendingTools[idx] = acc
					toolOrder = append(toolOrder, idx)
				}
				if id, ok := tcMap["id"].(string); ok && id != "" {
					acc.id = id
				}
				if fn, ok := tcMap["function"].(map[string]any); ok {
					if name, ok := fn["name"].(string); ok && name != "" {
						acc.name = name
					}
					if args, ok := fn["arguments"].(string); ok && args != "" {
						acc.args += args
					}
				}
				if len(toolOrder) == 1 && pendingTools[toolOrder[0]] == acc {
					startToolBlock(acc)
					emitPendingToolArgs(acc)
				}
			}
		}

		// Finish reason
		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			upstreamFinishReason = fr
			switch fr {
			case "length":
				stopReason = "max_tokens"
			case "tool_calls":
				stopReason = "tool_use"
			}
		}
	}

	closeThinkingBlock()
	closeTextBlock()

	if streamFailure != nil {
		finalizeRequestLog(reqLog, latestUsage, firstOutputAt, reqLog.StartedAt, false, streamFailure.Error())
		log.Printf("  anthropic stream failed: finish=%s text=%v tools=%d reasoning_chars=%d error=%v",
			upstreamFinishReason, hasText, len(pendingTools), reasoningChars, streamFailure)
		return
	}

	// OpenAI streams function arguments in arbitrary JSON fragments. Emit the
	// accumulated value using Anthropic's input_json_delta event sequence.
	for _, upstreamIndex := range toolOrder {
		acc := pendingTools[upstreamIndex]
		if acc.id == "" || acc.name == "" {
			continue
		}
		if acc.started {
			closeStartedTool(acc)
		} else {
			emitToolBlock(acc, nextContentIndex)
			nextContentIndex++
		}
		emittedTools++
	}
	if len(toolOrder) > 0 {
		stopReason = "tool_use"
	}
	if reasoningChars == 0 && !hasText && emittedTools == 0 {
		emptyErr := fmt.Errorf("%w (finish=%s reasoning_chars=%d thinking_tokens=%d)",
			errEmptyResponseContent, upstreamFinishReason, reasoningChars, latestUsage.Reasoning)
		emit("error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": emptyErr.Error()},
		})
		finalizeRequestLog(reqLog, latestUsage, firstOutputAt, reqLog.StartedAt, false, emptyErr.Error())
		log.Printf("  anthropic stream empty: finish=%s reasoning_chars=%d thinking_tokens=%d",
			upstreamFinishReason, reasoningChars, latestUsage.Reasoning)
		return
	}

	finalUsage := anthropicUsageFromOpenAI(map[string]any{"usage": latestRawUsage})
	emit("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": finalUsage,
	})
	recordTokenUsage(acc, reqLog.Model, latestUsage)
	finalizeRequestLog(reqLog, latestUsage, firstOutputAt, reqLog.StartedAt, true, "")

	emit("message_stop", map[string]any{"type": "message_stop"})
	log.Printf("  anthropic stream done: hasText=%v tools=%d reason=%s upstream_finish=%s reasoning_chars=%d thinking_tokens=%d",
		hasText, emittedTools, stopReason, upstreamFinishReason, reasoningChars, latestUsage.Reasoning)
}

func normalizeOpenAIResponse(obj map[string]any) map[string]any {
	out := make(map[string]any)
	for k, v := range obj {
		if k == "provider_metadata" || k == "proxy_metadata" {
			continue
		}
		out[k] = v
	}

	if choices, ok := out["choices"].([]any); ok {
		normalized := make([]any, 0, len(choices))
		for _, ch := range choices {
			if c, ok := ch.(map[string]any); ok {
				nc := make(map[string]any)
				for k, v := range c {
					if k == "provider_metadata" || k == "proxy_metadata" {
						continue
					}
					nc[k] = v
				}
				if msg, ok := nc["message"].(map[string]any); ok {
					nc["message"] = normalizeMessage(msg)
				}
				if delta, ok := nc["delta"].(map[string]any); ok {
					nd := make(map[string]any)
					for k, v := range delta {
						if k == "provider_metadata" || k == "proxy_metadata" {
							continue
						}
						nd[k] = v
					}
					if tc, ok := nd["tool_calls"].([]any); ok && len(tc) > 0 {
						if nd["content"] == nil {
							nd["content"] = ""
						}
					}
					nc["delta"] = nd
				}
				normalized = append(normalized, nc)
			} else {
				normalized = append(normalized, ch)
			}
		}
		out["choices"] = normalized
	}

	return out
}

func copyAllowedFields(source map[string]any, fields ...string) map[string]any {
	out := make(map[string]any, len(fields))
	for _, field := range fields {
		if value, exists := source[field]; exists {
			out[field] = value
		}
	}
	return out
}

func normalizeChatToolCalls(value any, streaming bool) []any {
	toolCalls, _ := value.([]any)
	normalized := make([]any, 0, len(toolCalls))
	for _, rawCall := range toolCalls {
		call, _ := rawCall.(map[string]any)
		if call == nil {
			continue
		}
		fields := []string{"id", "type", "function", "custom"}
		if streaming {
			fields = append(fields, "index")
		}
		cleanCall := copyAllowedFields(call, fields...)
		if function, _ := cleanCall["function"].(map[string]any); function != nil {
			cleanCall["function"] = copyAllowedFields(function, "name", "arguments")
		}
		if custom, _ := cleanCall["custom"].(map[string]any); custom != nil {
			cleanCall["custom"] = copyAllowedFields(custom, "name", "input")
		}
		normalized = append(normalized, cleanCall)
	}
	return normalized
}

func normalizeChatMessageFields(value any, streaming bool) map[string]any {
	message, _ := value.(map[string]any)
	if message == nil {
		return map[string]any{}
	}
	fields := []string{"role", "content", "refusal", "function_call", "tool_calls"}
	if !streaming {
		fields = append(fields, "annotations", "audio")
	}
	out := copyAllowedFields(message, fields...)
	if _, exists := out["tool_calls"]; exists {
		out["tool_calls"] = normalizeChatToolCalls(out["tool_calls"], streaming)
		if _, hasContent := out["content"]; !hasContent {
			out["content"] = ""
		}
	}
	return out
}

func normalizeChatUsage(value any) map[string]any {
	usage, _ := value.(map[string]any)
	if usage == nil {
		return nil
	}
	out := copyAllowedFields(usage,
		"prompt_tokens", "completion_tokens", "total_tokens",
		"prompt_tokens_details", "completion_tokens_details",
	)
	if details, _ := out["prompt_tokens_details"].(map[string]any); details != nil {
		out["prompt_tokens_details"] = copyAllowedFields(details,
			"audio_tokens", "cache_write_tokens", "cached_tokens", "image_tokens", "text_tokens",
		)
	}
	if details, _ := out["completion_tokens_details"].(map[string]any); details != nil {
		out["completion_tokens_details"] = copyAllowedFields(details,
			"accepted_prediction_tokens", "audio_tokens", "reasoning_tokens",
			"rejected_prediction_tokens", "text_tokens", "compute_units",
		)
	}
	return out
}

func normalizeOpenAIChatResponse(obj map[string]any, fallbackModel string, streaming bool) map[string]any {
	source := normalizeOpenAIResponse(obj)
	topLevelFields := []string{
		"id", "choices", "created", "model", "moderation", "service_tier", "system_fingerprint", "usage",
	}
	if streaming {
		topLevelFields = append(topLevelFields, "obfuscation")
	} else {
		topLevelFields = append(topLevelFields, "metadata")
	}
	out := copyAllowedFields(source, topLevelFields...)
	if id, _ := out["id"].(string); id == "" {
		out["id"] = newResponseID("chatcmpl_")
	}
	if _, exists := out["created"]; !exists {
		out["created"] = time.Now().Unix()
	}
	if model, _ := out["model"].(string); model == "" {
		out["model"] = fallbackModel
	}
	if streaming {
		out["object"] = "chat.completion.chunk"
	} else {
		out["object"] = "chat.completion"
	}

	choices, _ := source["choices"].([]any)
	normalizedChoices := make([]any, 0, len(choices))
	for _, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		if choice == nil {
			continue
		}
		cleanChoice := copyAllowedFields(choice, "index", "finish_reason", "logprobs")
		if streaming {
			cleanChoice["delta"] = normalizeChatMessageFields(choice["delta"], true)
		} else {
			cleanChoice["message"] = normalizeChatMessageFields(choice["message"], false)
		}
		normalizedChoices = append(normalizedChoices, cleanChoice)
	}
	out["choices"] = normalizedChoices
	if usage := normalizeChatUsage(source["usage"]); usage != nil {
		out["usage"] = usage
	} else {
		delete(out, "usage")
	}
	return out
}

func sanitizeContent(s string) string {
	return s
}

func normalizeMessage(msg map[string]any) map[string]any {
	out := make(map[string]any)
	for k, v := range msg {
		if k == "provider_metadata" || k == "proxy_metadata" {
			continue
		}
		out[k] = v
	}
	if tc, ok := out["tool_calls"].([]any); ok && len(tc) > 0 {
		if out["content"] == nil {
			out["content"] = ""
		}
	}
	if c, ok := out["content"].(string); ok {
		out["content"] = sanitizeContent(c)
	}
	return out
}

func getNested(obj map[string]any, keys ...any) any {
	current := any(obj)
	for _, key := range keys {
		switch k := key.(type) {
		case string:
			if m, ok := current.(map[string]any); ok {
				current = m[k]
			} else {
				return nil
			}
		case int:
			if arr, ok := current.([]any); ok && k < len(arr) {
				current = arr[k]
			} else {
				return nil
			}
		default:
			return nil
		}
	}
	return current
}

func freePort(port int) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return // port is free
	}
	conn.Close()

	// Try to kill the process using the port
	cmd := execCommand("powershell", "-Command",
		fmt.Sprintf(`$p=Get-NetTCPConnection -LocalPort %d -ErrorAction SilentlyContinue; if($p){Stop-Process -Id $p.OwningProcess -Force}`, port))
	_ = cmd.Run()
	time.Sleep(500 * time.Millisecond)
}
