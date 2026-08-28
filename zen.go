package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// opencode Zen 免费模型支持
// 按请求中的 model 自动分流：zen 免费模型 → https://opencode.ai/zen/v1；
// zen 付费模型 → 400 拒绝；其余 → Cline 账号池。
// zen 模型与 Cline 远程模型同存于 pool.Models（Source="zen"），
// 同步采用全量替换：官方下架的模型自动移除，不会残留僵尸条目。
// ============================================================================

const zenAPIBase = "https://opencode.ai/zen/v1"

// 请求日志中的上游标记
const (
	upstreamCline    = "cline"
	upstreamOpenCode = "opencode"
)

const zenModelSyncInterval = 10 * time.Minute

// zenSeedModels 内置 zen 免费模型种子表（含别名），仅作为从未同步成功时的离线 fallback。
// 与 builtinModels（Cline 侧）同一模式：同步成功后以远程列表为准。
type zenSeedModel struct {
	ID      string
	Aliases []string
	Context int
	Output  int
}

var zenSeedModels = []zenSeedModel{
	{ID: "deepseek-v4-flash-free", Aliases: []string{"deepseek-v4-flash", "deepseek-v4"}, Context: 200000, Output: 128000},
	{ID: "mimo-v2.5-free", Aliases: []string{"mimo-v2.5", "mimo"}, Context: 200000, Output: 32000},
	{ID: "ling-3.0-flash-free", Aliases: []string{"ling-3.0-flash", "ling"}, Context: 200000, Output: 32768},
	{ID: "nemotron-3-ultra-free", Aliases: []string{"nemotron-3-ultra", "nemotron"}, Context: 1000000, Output: 128000},
	{ID: "north-mini-code-free", Aliases: []string{"north-mini-code", "north-mini"}, Context: 256000, Output: 64000},
	{ID: "laguna-s-2.1-free", Aliases: []string{"laguna-s-2.1", "laguna"}, Context: 200000, Output: 32768},
	{ID: "longcat-2.0-free", Aliases: []string{"longcat-2.0", "longcat"}, Context: 200000, Output: 32768},
	{ID: "big-pickle", Context: 200000, Output: 32000},
}

// builtinZenModels 把种子表转成 Model 条目（离线 fallback 用，Source="seed"）。
func builtinZenModels() []Model {
	out := make([]Model, 0, len(zenSeedModels))
	for _, m := range zenSeedModels {
		out = append(out, Model{
			ID:       m.ID,
			Provider: "opencode",
			Cost:     "free",
			Status:   "active",
			Custom:   false,
			Source:   "seed",
			Context:  m.Context,
			Output:   m.Output,
		})
	}
	return out
}

// remoteZenEnabled：zen 官方 /models 同步成功过 → 以远程列表为准，
// 种子表中已下架的模型自动休眠（不再出现在列表和路由里）。与 remoteModelsEnabled 同一模式。
var (
	remoteZenEnabled   bool
	remoteZenEnabledMu sync.Mutex
)

func remoteZenActive() bool {
	remoteZenEnabledMu.Lock()
	defer remoteZenEnabledMu.Unlock()
	return remoteZenEnabled
}

// isZenSource 判断模型来源是否属于 opencode 体系（同步条目 "zen" / 内置种子 "seed"）。
func isZenSource(m Model) bool {
	return m.Source == "zen" || m.Source == "seed"
}

// currentZenModels 返回当前生效的 zen 模型（pool 中 zen 来源条目；
// 从未同步成功时回退到种子表）。
func currentZenModels() []Model {
	p := loadPool()
	poolMu.Lock()
	var zen []Model
	for _, m := range p.Models {
		if isZenSource(m) {
			zen = append(zen, m)
		}
	}
	poolMu.Unlock()
	if len(zen) > 0 || remoteZenActive() {
		return zen
	}
	return builtinZenModels()
}

// resolveZenInfo 解析模型名到当前生效的 zen 模型。支持别名与 "opencode/" 前缀。
// 别名优先于精确 ID 匹配之后、但优先级高于付费同名 ID（种子的 free 别名不会被覆盖）。
func resolveZenInfo(id string) (Model, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Model{}, false
	}
	models := currentZenModels()

	// 别名表：seed 模型的别名 → 正式 ID（用种子表数据补全上下文）
	contextOf := func(m Model) Model { return m }
	for _, sm := range zenSeedModels {
		for _, a := range sm.Aliases {
			if a == id {
				// 种子模型必须在当前生效列表里才算数（否则就是被下架了）
				for _, m := range models {
					if m.ID == sm.ID && m.Cost == "free" {
						return contextOf(m), true
					}
				}
				break
			}
		}
	}

	tryOne := func(name string) (Model, bool) {
		for _, m := range models {
			if m.ID == name {
				return m, true
			}
		}
		return Model{}, false
	}
	if m, ok := tryOne(id); ok {
		return m, true
	}
	if strings.HasPrefix(id, "opencode/") {
		if m, ok := tryOne(strings.TrimPrefix(id, "opencode/")); ok {
			return m, true
		}
	}
	return Model{}, false
}

// isZenFreeModel 判定 zen 模型是否免费（用于路由：非免费的 zen 模型直接拒绝）。
// 种子白名单兜底：官方免费模型即使同步条目漏标 -free 后缀也不会被误拒。
func isZenFreeModel(m Model) bool {
	if m.Cost == "free" || strings.HasSuffix(m.ID, "-free") {
		return true
	}
	for _, sm := range zenSeedModels {
		if sm.ID == m.ID {
			return true
		}
	}
	return false
}

const (
	modelRouteZen            = "zen"
	modelRouteCline          = "cline"
	modelRouteReject         = "reject"
	modelRouteZenUnavailable = "zen_unavailable"
)

type modelRouteDecision struct {
	Route string
	Model string
}

var zenClineFallbackModels = map[string]string{
	"deepseek-v4-flash-free": "deepseek/deepseek-v4-flash",
	"laguna-s-2.1-free":      "poolside/laguna-s-2.1:free",
}

func availableClineFallback(zenModel string) (string, bool) {
	fallback, ok := zenClineFallbackModels[zenModel]
	if !ok {
		return "", false
	}
	for _, model := range getAllModels() {
		if model.ID == fallback && !isZenSource(model) && model.Status == "active" {
			return fallback, true
		}
	}
	return "", false
}

// resolveModelRoute separates client-facing model IDs from upstream IDs.
// Zen uses bare IDs, while Cline requires provider/model IDs.
func resolveModelRoute(id string) modelRouteDecision {
	m, ok := resolveZenInfo(id)
	if !ok {
		return modelRouteDecision{Route: modelRouteCline, Model: id}
	}
	if !isZenFreeModel(m) {
		return modelRouteDecision{Route: modelRouteReject, Model: m.ID}
	}
	// 极端情况：同名 ID 同时是 cline 模型（自定义冲突）→ 让给 cline
	p := loadPool()
	poolMu.Lock()
	for _, pm := range p.Models {
		if !isZenSource(pm) && pm.ID == strings.TrimPrefix(strings.TrimSpace(id), "opencode/") {
			poolMu.Unlock()
			return modelRouteDecision{Route: modelRouteCline, Model: id}
		}
	}
	poolMu.Unlock()

	cfg := getZenConfig()
	if cfg.Failover && zenFailedNow() {
		if fallback, available := availableClineFallback(m.ID); available {
			log.Printf("  failover: zen degraded, %q routed to cline model %q", id, fallback)
			return modelRouteDecision{Route: modelRouteCline, Model: fallback}
		}
		log.Printf("  failover: zen degraded, %q has no compatible cline model", id)
		return modelRouteDecision{Route: modelRouteZenUnavailable, Model: m.ID}
	}
	return modelRouteDecision{Route: modelRouteZen, Model: m.ID}
}

func routeModel(id string) string {
	return resolveModelRoute(id).Route
}

// ============ zen 配置 ============

type zenCompactConfig struct {
	Auto         bool   `json:"auto"`         // 超限自动摘要压缩，默认开启
	Buffer       int    `json:"buffer"`       // 预留输出缓冲 token，默认 20000
	KeepTokens   int    `json:"keepTokens"`   // 压缩后尾部保留 token 预算，默认 8000
	SummaryModel string `json:"summaryModel"` // 摘要生成模型，空=用请求模型本身
	MaxSummary   int    `json:"maxSummary"`   // 摘要最大输出 token，默认 4096
}

type zenConfigData struct {
	Enabled         bool             `json:"enabled"`
	Key             string           `json:"key"`
	BaseURL         string           `json:"baseURL"`
	Proxies         []string         `json:"proxies"`
	ProxyStrategy   string           `json:"proxyStrategy"` // round_robin / random / fill
	MaxConcurrency  int              `json:"maxConcurrency"`
	Retries         int              `json:"retries"`
	Failover        bool             `json:"failover"`
	FailoverCount   int              `json:"failoverCount"`
	FailoverMinutes int              `json:"failoverMinutes"`
	Compaction      zenCompactConfig `json:"compaction"`
}

func defaultZenConfig() *zenConfigData {
	return &zenConfigData{
		Enabled:         true,
		Key:             "public",
		BaseURL:         zenAPIBase,
		ProxyStrategy:   "round_robin",
		MaxConcurrency:  8,
		Retries:         3,
		Failover:        true,
		FailoverCount:   3,
		FailoverMinutes: 5,
		Compaction: zenCompactConfig{
			Auto:       true,
			Buffer:     20000,
			KeepTokens: 8000,
			MaxSummary: 4096,
		},
	}
}

var (
	zenConfig   *zenConfigData
	zenConfigMu sync.Mutex
)

// getZenConfig 惰性加载配置（避免依赖包初始化顺序）。
func getZenConfig() *zenConfigData {
	zenConfigMu.Lock()
	defer zenConfigMu.Unlock()
	if zenConfig == nil {
		cfg := defaultZenConfig()
		if data, err := os.ReadFile(resolveDataPath(".cline-zen.json")); err == nil {
			if err := json.Unmarshal(data, cfg); err != nil {
				log.Printf("zen config parse failed: %v", err)
			}
		}
		if cfg.Key == "" {
			cfg.Key = "public"
		}
		if cfg.BaseURL == "" {
			cfg.BaseURL = zenAPIBase
		}
		if cfg.ProxyStrategy != "random" && cfg.ProxyStrategy != "fill" {
			cfg.ProxyStrategy = "round_robin"
		}
		zenConfig = cfg
	}
	return zenConfig
}

// setZenConfig 原子替换配置并持久化，重建信号量与 HTTP 传输层。
func setZenConfig(c *zenConfigData) {
	c.BaseURL = strings.TrimSpace(c.BaseURL)
	if c.BaseURL == "" {
		c.BaseURL = zenAPIBase
	}
	if c.Key == "" {
		c.Key = "public"
	}
	if c.ProxyStrategy != "round_robin" && c.ProxyStrategy != "random" && c.ProxyStrategy != "fill" {
		c.ProxyStrategy = "round_robin"
	}
	if c.MaxConcurrency <= 0 {
		c.MaxConcurrency = 8
	}
	if c.Retries < 0 {
		c.Retries = 3
	}
	zenConfigMu.Lock()
	zenConfig = c
	zenConfigMu.Unlock()

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		log.Printf("zen config encode failed: %v", err)
		return
	}
	if err := writeFileDurably(resolveDataPath(".cline-zen.json"), data, 0600); err != nil {
		log.Printf("zen config save failed: %v", err)
	}
	rebuildZenTransport()
	rebuildZenSem()
}

// ============ 限流防御状态机 ============

var (
	zenSem       chan struct{} // 并发信号量（防上游瞬时超限）
	zenFailCount int           // 连续失败计数
	zenFailUntil time.Time     // 故障转移截止时间
	zenStateMu   sync.Mutex
)

func rebuildZenSem() {
	n := getZenConfig().MaxConcurrency
	if n <= 0 {
		n = 8
	}
	zenStateMu.Lock()
	zenSem = make(chan struct{}, n)
	zenStateMu.Unlock()
}

func markZenSuccess() {
	zenStateMu.Lock()
	zenFailCount = 0
	zenFailUntil = time.Time{}
	zenStateMu.Unlock()
}

func markZenFail() {
	cfg := getZenConfig()
	thr := cfg.FailoverCount
	if thr <= 0 {
		thr = 3
	}
	window := cfg.FailoverMinutes
	if window <= 0 {
		window = 5
	}
	zenStateMu.Lock()
	zenFailCount++
	if zenFailCount >= thr {
		zenFailUntil = time.Now().Add(time.Duration(window) * time.Minute)
		log.Printf("  zen failover armed: %d consecutive failures, compatible models use cline for %dm", zenFailCount, window)
	}
	zenStateMu.Unlock()
}

// zenFailedNow 当前是否处于故障转移窗口内。
func zenFailedNow() bool {
	zenStateMu.Lock()
	defer zenStateMu.Unlock()
	if zenFailUntil.IsZero() {
		return false
	}
	if time.Now().After(zenFailUntil) {
		zenFailCount = 0
		zenFailUntil = time.Time{}
		return false
	}
	return true
}

// isRateLimited 限流信号识别：429/503 直接命中；502/403 按错误体关键词。
func isRateLimited(status int, body string) bool {
	if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
		return true
	}
	if status == http.StatusBadGateway || status == http.StatusForbidden {
		low := strings.ToLower(body)
		for _, kw := range []string{"resourceexhausted", "limit reached", "rate limit", "too many", "overloaded", "busy"} {
			if strings.Contains(low, kw) {
				return true
			}
		}
	}
	return false
}

// parseRetryAfter 解析 Retry-After 响应头（秒数或 HTTP 日期）；解析失败返回 0。
func parseRetryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	var secs int
	if _, err := fmt.Sscanf(h, "%d", &secs); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return 0
}

// validateProxyList 校验代理列表格式：支持 http/https/socks5/socks5h，必须含 host:port。
func validateProxyList(proxies []string) error {
	for _, p := range proxies {
		line := strings.TrimSpace(p)
		if line == "" {
			continue
		}
		u, err := url.Parse(line)
		if err != nil {
			return fmt.Errorf("proxy %q invalid: %v", line, err)
		}
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return fmt.Errorf("proxy %q: unsupported scheme (http/https/socks5/socks5h)", line)
		}
		if u.Host == "" {
			return fmt.Errorf("proxy %q: missing host:port", line)
		}
		if _, _, err := net.SplitHostPort(u.Host); err != nil {
			return fmt.Errorf("proxy %q: missing port", line)
		}
	}
	return nil
}

// ============ 客户端身份轮换 ============
// opencode 服务端可能按 session / UA 维度记账限流；每次请求生成全新身份，
// 等价于每个请求都来自一台新装的客户端。

var zenUserAgents = []string{
	"opencode/latest/1.18.14/cli",
	"opencode/latest/1.18.13/cli",
	"opencode/1.18.14/cli",
	"opencode/1.18.13/cli",
	"opencode/1.18.12/cli",
	"opencode/1.18.11/cli",
	"opencode/latest/1.18.14/desktop",
	"opencode/latest/1.18.13/desktop",
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func randIntn(n int) int {
	if n <= 0 {
		return 0
	}
	b := make([]byte, 4)
	rand.Read(b)
	v := int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3])
	if v < 0 {
		v = -v
	}
	return v % n
}

// withRetryJitter 在退避时长上叠加 0~25% 抖动，错开并发重试。
func withRetryJitter(delay time.Duration) time.Duration {
	if delay <= 0 {
		return delay
	}
	return delay + time.Duration(float64(delay)*float64(randIntn(26))/100)
}

// freshZenIdentity 生成一组全新客户端身份（session, request-id, user-agent）。
func freshZenIdentity() (string, string, string) {
	return "sess_" + randHex(16),
		"user_" + randHex(8),
		zenUserAgents[randIntn(len(zenUserAgents))]
}

// ============ zen 上游调用 ============

// buildZenBody 构造 zen 请求体：只带 OpenAI 兼容字段，模型名改写为 zen 正式 ID。
func buildZenBody(params map[string]any, stream bool) map[string]any {
	body := map[string]any{}
	for _, key := range passThroughKeys {
		if val, ok := params[key]; ok {
			body[key] = val
		}
	}
	for _, key := range []string{"model", "messages", "max_tokens", "max_completion_tokens"} {
		if val, ok := params[key]; ok {
			body[key] = val
		}
	}
	body["stream"] = stream
	if model, ok := params["model"].(string); ok {
		if m, ok := resolveZenInfo(model); ok {
			body["model"] = m.ID
		} else if model != "" {
			body["model"] = strings.TrimPrefix(model, "opencode/")
		}
	}
	delete(body, "reasoning_effort")
	delete(body, "reasoningEffort")
	return body
}

// callZenAPI 调用 zen 上游：并发信号量 + 指数退避重试 + 代理冷却 + 故障转移计数。
// 身份头每次轮换。返回的响应由调用方关闭。
func callZenAPI(params map[string]any, stream bool) (*http.Response, error) {
	cfg := getZenConfig()
	bodyJSON, err := json.Marshal(buildZenBody(params, stream))
	if err != nil {
		return nil, fmt.Errorf("marshal zen body: %w", err)
	}
	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/chat/completions"

	zenStateMu.Lock()
	sem := zenSem
	if sem == nil {
		rebuildZenSemLocked()
		sem = zenSem
	}
	zenStateMu.Unlock()
	sem <- struct{}{}
	defer func() { <-sem }()

	retries := cfg.Retries
	if retries <= 0 {
		retries = 3
	}
	delay := time.Second

	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest("POST", endpoint, bytes.NewReader(bodyJSON))
		if err != nil {
			return nil, fmt.Errorf("create zen request: %w", err)
		}
		sess, user, ua := freshZenIdentity()
		req.Header.Set("Authorization", "Bearer "+cfg.Key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", ua)
		req.Header.Set("x-opencode-session", sess)
		req.Header.Set("x-opencode-request", user)
		req.Header.Set("x-opencode-client", "cli")
		if model, _ := params["model"].(string); model != "" {
			if m, ok := resolveZenInfo(model); ok {
				req.Header.Set("x-opencode-model", m.ID)
			}
		}
		log.Printf("  zen upstream: model=%v stream=%v msgs=%d via=%s attempt=%d session=%s",
			bodyParamsModel(params), stream, getMsgCount(params), describeZenProxy(), attempt+1, truncate(sess, 24))

		resp, err := getZenHTTPClient().Do(req)
		if err != nil {
			// 网络错误：先退避重试，耗尽后计入故障转移。
			if attempt < retries {
				log.Printf("  zen network error (%v), retry %d/%d after %v", err, attempt+1, retries, delay)
				time.Sleep(withRetryJitter(delay))
				delay *= 2
				continue
			}
			markZenFail()
			return nil, fmt.Errorf("zen request: %w", err)
		}
		if resp.StatusCode == http.StatusOK {
			markZenSuccess()
			return resp, nil
		}

		bodyBytes := readAllLimited(resp.Body, 64<<10)
		resp.Body.Close()
		reason := fmt.Sprintf("zen API %d: %s", resp.StatusCode, truncate(string(bodyBytes), 500))

		if isRateLimited(resp.StatusCode, string(bodyBytes)) {
			// 冷却当前出口代理（Retry-After 优先，默认 10 分钟）
			if idx := lastZenProxyIdx(); idx >= 0 {
				d := parseRetryAfter(resp.Header.Get("Retry-After"))
				if d <= 0 {
					d = 10 * time.Minute
				}
				cooldownZenProxy(idx, d)
			}
			if attempt < retries {
				wait := delay
				if ra := parseRetryAfter(resp.Header.Get("Retry-After")); ra > wait {
					wait = ra
				}
				log.Printf("  zen rate limited (%d), retry %d/%d after %v", resp.StatusCode, attempt+1, retries, wait)
				time.Sleep(withRetryJitter(wait))
				delay *= 2
				continue
			}
			markZenFail()
			return nil, fmt.Errorf("%s", reason)
		}

		// 4xx 通常是单次请求、模型或凭据问题，不能让一个客户端错误把
		// 所有 Zen 模型切进 Cline。只有服务端错误才触发全局故障转移。
		if resp.StatusCode >= http.StatusInternalServerError {
			markZenFail()
		}
		return nil, fmt.Errorf("%s", reason)
	}
}

func bodyParamsModel(params map[string]any) string {
	m, _ := params["model"].(string)
	return m
}

// readAllLimited 读取响应体，最多 limit 字节（防御异常大的错误页）。
func readAllLimited(r io.Reader, limit int64) []byte {
	data, _ := io.ReadAll(io.LimitReader(r, limit))
	return data
}

func rebuildZenSemLocked() {
	n := zenConfig.MaxConcurrency
	if n <= 0 {
		n = 8
	}
	zenSem = make(chan struct{}, n)
}

func describeZenProxy() string {
	cfg := getZenConfig()
	if len(cfg.Proxies) == 0 {
		return "direct"
	}
	idx := lastZenProxyIdx()
	if idx < 0 {
		idx = 0
	}
	idx %= len(cfg.Proxies)
	return fmt.Sprintf("proxy[%d]=%s", idx+1, truncate(maskProxyURL(cfg.Proxies[idx]), 60))
}

// ============ 模型同步 ============

// syncZenModels 拉取 zen 官方 /models 并全量替换 pool 中 Source=="zen" 条目：
// 计算新增/移除清单 —— 官方下架的模型自动从列表消失，不留僵尸条目。
// 自定义模型（Custom=true 或其他 Source）不受影响。
func syncZenModels() modelSyncResult {
	res := modelSyncResult{SyncedAt: time.Now().Format(time.RFC3339)}
	fail := func(err error) modelSyncResult {
		log.Printf("zen models sync failed: %v", err)
		res.Error = err.Error()
		return res
	}

	cfg := getZenConfig()
	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/models"
	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return fail(err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Key)
	client := &http.Client{Timeout: 25 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fail(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fail(fmt.Errorf("models API returned status %d", resp.StatusCode))
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return fail(err)
	}

	// 组装远程 zen 模型（去重；计费按 -free 后缀或种子白名单判定）
	seedFree := make(map[string]bool, len(zenSeedModels))
	for _, sm := range zenSeedModels {
		seedFree[sm.ID] = true
	}
	seen := make(map[string]bool)
	var remote []Model
	for _, item := range payload.Data {
		id := item.ID
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		cost := "pass"
		if strings.HasSuffix(id, "-free") || seedFree[id] {
			cost = "free"
		}
		remote = append(remote, Model{
			ID:       id,
			Provider: "opencode",
			Cost:     cost,
			Status:   "active",
			Custom:   false,
			Source:   "zen",
		})
	}
	if len(remote) == 0 {
		return fail(fmt.Errorf("models API returned empty list"))
	}

	// 补全上下文信息：远程接口不带 context/output，优先沿用种子表/旧值
	fillMeta := func(m Model) Model {
		for _, sm := range zenSeedModels {
			if sm.ID == m.ID {
				m.Context, m.Output = sm.Context, sm.Output
				return m
			}
		}
		if m.Context == 0 {
			m.Context = 200000
		}
		if m.Output == 0 {
			m.Output = 32768
		}
		return m
	}
	for i := range remote {
		remote[i] = fillMeta(remote[i])
	}

	p := loadPool()
	poolMu.Lock()
	oldIDs := make(map[string]bool)
	var kept []Model
	for _, m := range p.Models {
		if m.Source == "zen" {
			oldIDs[m.ID] = true
			continue
		}
		kept = append(kept, m)
	}
	for _, m := range remote {
		if !oldIDs[m.ID] {
			res.Added = append(res.Added, m.ID)
		}
	}
	for id := range oldIDs {
		if !seen[id] {
			res.Removed = append(res.Removed, id)
		}
	}
	kept = append(kept, remote...)
	p.Models = kept
	res.Total = len(remote)
	res.Changed = len(res.Added) > 0 || len(res.Removed) > 0
	poolMu.Unlock()
	savePool()

	remoteZenEnabledMu.Lock()
	remoteZenEnabled = true
	remoteZenEnabledMu.Unlock()

	log.Printf("zen models sync: %d models, +%d added, -%d removed", res.Total, len(res.Added), len(res.Removed))
	return res
}

// startZenModelsRefresher 启动定时同步（10 分钟一次，不阻塞启动）。
func startZenModelsRefresher() {
	go func() {
		time.Sleep(2 * time.Second) // 错开启动高峰
		if cfg := getZenConfig(); cfg.Enabled {
			setLastZenModelSync(syncZenModels())
		}
		ticker := time.NewTicker(zenModelSyncInterval)
		defer ticker.Stop()
		for range ticker.C {
			if cfg := getZenConfig(); cfg.Enabled {
				setLastZenModelSync(syncZenModels())
			}
		}
	}()
}

// ============ 最近一次同步结果（管理后台展示） ============

var (
	lastZenSync    modelSyncResult
	lastZenSyncRan bool
	lastZenSyncMu  sync.Mutex
)

func setLastZenModelSync(res modelSyncResult) {
	lastZenSyncMu.Lock()
	lastZenSync = res
	lastZenSyncRan = true
	lastZenSyncMu.Unlock()
}

func lastZenModelSync() modelSyncResult {
	lastZenSyncMu.Lock()
	defer lastZenSyncMu.Unlock()
	if !lastZenSyncRan {
		return modelSyncResult{SyncedAt: ""}
	}
	return lastZenSync
}

// opencodeUsageToday 从请求日志聚合今日 opencode 上游用量（后台仪表盘卡片用）。
func opencodeUsageToday() map[string]any {
	requestLogsMu.Lock()
	defer requestLogsMu.Unlock()

	var requests int64
	var input, output, total int64
	today := time.Now().Format("2006-01-02")
	for _, e := range requestLogs {
		if e.Upstream != upstreamOpenCode || e.StartedAt.Format("2006-01-02") != today {
			continue
		}
		requests++
		input += e.InputTokens
		output += e.OutputTokens
		total += e.TotalTokens
	}
	return map[string]any{
		"requests":     requests,
		"inputTokens":  input,
		"outputTokens": output,
		"totalTokens":  total,
	}
}
