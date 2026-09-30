package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// clineRecommendedModelsURL 是 Cline 官方的「推荐/免费模型」接口（无需认证）。
// 参考 model-api.md：Cline 4.1.15 的 Free Models 由该接口直接返回。
var clineRecommendedModelsURL = "https://api.cline.bot/api/v1/ai/cline/recommended-models"

const modelSyncTimeout = 10 * time.Second

// clineRemoteModel 对应接口返回的单个模型字段。
type clineRemoteModel struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Tags        []string `json:"tags"`
	ContextWin  int      `json:"contextWindow"`
	MaxInput    int      `json:"maxInputTokens"`
	MaxTokens   int      `json:"maxTokens"`
	ReleaseDate string   `json:"releaseDate"`
	Family      string   `json:"family"`
}

// clineRecommendedResponse 对应接口返回结构：recommended / free / clinePass 三个数组。
type clineRecommendedResponse struct {
	Recommended []clineRemoteModel `json:"recommended"`
	Free        []clineRemoteModel `json:"free"`
	ClinePass   []clineRemoteModel `json:"clinePass"`
}

// modelSyncResult 是一次模型同步的结果（供管理后台弹窗展示）。
type modelSyncResult struct {
	Changed  bool     `json:"changed"`
	Added    []string `json:"added"`
	Removed  []string `json:"removed"`
	SyncedAt string   `json:"syncedAt"`
	Total    int      `json:"total"`
	Error    string   `json:"error,omitempty"`
}

type modelMeta struct {
	Context int
	Output  int
}

// The recommended-models endpoint may omit limits. Keep only limits confirmed
// as hard upstream constraints here; unknown models remain unclamped.
var clineModelMeta = map[string]modelMeta{
	"gemini-3.8-flash": {Context: 1_048_576, Output: 65_536},
}

var clineModelPrefixes = []string{"cline-free/", "cline-pass/", "google/", "cline/"}

func modelBaseName(id string) string {
	id = strings.TrimSpace(id)
	for _, prefix := range clineModelPrefixes {
		if strings.HasPrefix(id, prefix) {
			return strings.TrimPrefix(id, prefix)
		}
	}
	return id
}

func lookupClineModelMeta(id string) (modelMeta, bool) {
	meta, ok := clineModelMeta[modelBaseName(id)]
	return meta, ok
}

func preserveLockedModelMetadata(models []Model, previous map[string]Model) {
	for index := range models {
		old, exists := previous[models[index].ID]
		if !exists || !old.MetaLocked {
			continue
		}
		models[index].Context = old.Context
		models[index].Output = old.Output
		models[index].MetaLocked = true
	}
}

var (
	modelSyncMu   sync.Mutex
	lastModelSync modelSyncResult
	modelSyncRan  bool // 启动后是否已同步过（避免重复）
	modelSyncBusy bool // 同步进行中（防并发触发）
)

// remoteModelsEnabled 远程同步成功后置 true：此后 getAllModels 以远程模型为主，
// 内置硬编码模型（已失效）仅作为离线 fallback。
var (
	remoteModelsEnabled   bool
	remoteModelsEnabledMu sync.Mutex
)

// fetchClineRecommendedModels 拉取并解析 Cline 官方推荐模型接口。
func fetchClineRecommendedModels() (clineRecommendedResponse, error) {
	client := &http.Client{Timeout: modelSyncTimeout, Transport: httpTransport}
	resp, err := client.Get(clineRecommendedModelsURL)
	if err != nil {
		return clineRecommendedResponse{}, fmt.Errorf("fetch models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return clineRecommendedResponse{}, fmt.Errorf("models API returned status %d", resp.StatusCode)
	}

	var data clineRecommendedResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return clineRecommendedResponse{}, fmt.Errorf("decode models: %w", err)
	}
	return data, nil
}

// remoteCost 判断远程模型计费：tags 含 "FREE" 或来自 free 数组 → free，否则 pass。
func remoteCost(m clineRemoteModel, inFreeList bool) string {
	if inFreeList {
		return "free"
	}
	for _, t := range m.Tags {
		if strings.EqualFold(t, "FREE") {
			return "free"
		}
	}
	return "pass"
}

// remoteProvider 从模型 ID 前缀推断 provider，无前缀时归为 "cline"。
func remoteProvider(id string) string {
	if idx := strings.Index(id, "/"); idx > 0 {
		return id[:idx]
	}
	return "cline"
}

// syncClineModels 执行一次模型同步并持久化：
//  1. 拉取远程推荐模型（free / clinePass / recommended）
//  2. 与池中现有 remote 模型比较，得到 added / removed
//  3. 更新 AccountPool.Models（替换 Source=remote 的旧条目），保存
//  4. 记录 lastModelSync 供管理后台弹窗
//
// 任何一步失败都会把错误写进 lastModelSync，不阻塞服务启动。
func syncClineModels() modelSyncResult {
	modelSyncMu.Lock()
	if modelSyncBusy {
		modelSyncMu.Unlock()
		return lastModelSync
	}
	modelSyncBusy = true
	modelSyncMu.Unlock()
	defer func() { modelSyncMu.Lock(); modelSyncBusy = false; modelSyncMu.Unlock() }()

	res := modelSyncResult{SyncedAt: time.Now().Format(time.RFC3339)}
	fail := func(err error) modelSyncResult {
		log.Printf("models sync failed: %v", err)
		res.Error = err.Error()
		modelSyncMu.Lock()
		lastModelSync = res
		modelSyncMu.Unlock()
		return res
	}

	data, err := fetchClineRecommendedModels()
	if err != nil {
		return fail(err)
	}

	// 组装远程模型列表（去重，free 数组在前）
	var remote []Model
	seen := make(map[string]bool)
	addGroup := func(group []clineRemoteModel, inFree bool) {
		for _, m := range group {
			if m.ID == "" || seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			contextTokens := m.MaxInput
			if contextTokens == 0 {
				contextTokens = m.ContextWin
			}
			entry := Model{
				ID:       m.ID,
				Provider: remoteProvider(m.ID),
				Cost:     remoteCost(m, inFree),
				Status:   "active",
				Custom:   false,
				Source:   "remote",
				Context:  contextTokens,
				Output:   m.MaxTokens,
			}
			if meta, ok := lookupClineModelMeta(m.ID); ok {
				if entry.Context == 0 {
					entry.Context = meta.Context
				}
				if entry.Output == 0 {
					entry.Output = meta.Output
				}
			}
			remote = append(remote, entry)
		}
	}
	addGroup(data.Free, true)
	addGroup(data.ClinePass, false)
	addGroup(data.Recommended, false)

	if len(remote) == 0 {
		return fail(fmt.Errorf("models API returned empty list"))
	}

	// 与池中现有 remote 模型比较
	p := loadPool()
	poolMu.Lock()
	oldRemote := make(map[string]Model)
	oldRemoteOrder := make([]Model, 0)
	var kept []Model
	for _, m := range p.Models {
		if m.Source == "remote" {
			oldRemote[m.ID] = m
			oldRemoteOrder = append(oldRemoteOrder, m)
			continue
		}
		kept = append(kept, m)
	}
	preserveLockedModelMetadata(remote, oldRemote)
	for index := range remote {
		if meta, ok := lookupClineModelMeta(remote[index].ID); ok {
			if remote[index].Context == 0 || remote[index].Context > meta.Context {
				remote[index].Context = meta.Context
			}
			if remote[index].Output == 0 || remote[index].Output > meta.Output {
				remote[index].Output = meta.Output
			}
		}
	}
	for index := range remote {
		if _, exists := oldRemote[remote[index].ID]; !exists {
			res.Added = append(res.Added, remote[index].ID)
		}
	}
	// A catalog disappearance is not proof that routing has stopped. Preserve
	// the entry as delisted and remove it only after an explicit model-gone
	// response (or an administrator action).
	for _, old := range oldRemoteOrder {
		if seen[old.ID] {
			continue
		}
		if !old.Delisted {
			res.Removed = append(res.Removed, old.ID)
		}
		old.Delisted = true
		kept = append(kept, old)
	}
	kept = append(kept, remote...)
	p.Models = kept
	res.Total = len(remote)
	res.Changed = len(res.Added) > 0 || len(res.Removed) > 0
	poolMu.Unlock()
	savePool()

	remoteModelsEnabledMu.Lock()
	remoteModelsEnabled = true
	remoteModelsEnabledMu.Unlock()

	modelSyncMu.Lock()
	lastModelSync = res
	modelSyncRan = true
	modelSyncMu.Unlock()

	log.Printf("models sync: %d models, +%d added, -%d removed",
		res.Total, len(res.Added), len(res.Removed))
	return res
}

var modelGonePattern = regexp.MustCompile(
	`(?i)model[\s_-]*(not[\s_-]*found|does\s+not\s+exist|no\s+such|unknown|invalid)|(invalid|unknown|no\s+such)[\s_-]*model`)

func isModelGoneError(status int, body string) bool {
	if status != http.StatusBadRequest && status != http.StatusNotFound {
		return false
	}
	return modelGonePattern.MatchString(body)
}

func markModelGone(model string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	p := loadPool()
	poolMu.Lock()
	removed := false
	for index, candidate := range p.Models {
		if candidate.ID == model && candidate.Delisted && !candidate.Custom {
			p.Models = append(p.Models[:index], p.Models[index+1:]...)
			removed = true
			break
		}
	}
	if removed && p.DefaultModel == model {
		p.DefaultModel = ""
	}
	poolMu.Unlock()
	if removed {
		savePool()
		log.Printf("model %q removed: upstream confirmed the delisted model no longer exists", model)
	}
}

// triggerModelSync 供管理后台手动触发同步；非阻塞等待完成并返回结果。
func triggerModelSync() modelSyncResult {
	return syncClineModels()
}

// getModelSyncResult 返回最近一次同步结果（供管理后台展示）。
func getModelSyncResult() modelSyncResult {
	modelSyncMu.Lock()
	defer modelSyncMu.Unlock()
	if !modelSyncRan {
		return modelSyncResult{SyncedAt: ""}
	}
	return lastModelSync
}

// startModelSync 在服务启动时异步同步一次（不阻塞启动）。
func startModelSync() {
	go func() {
		if !modelSyncRan {
			syncClineModels()
		}
	}()
}

// remoteModelsActive 返回远程模型是否已启用（同步成功过）。
func remoteModelsActive() bool {
	remoteModelsEnabledMu.Lock()
	defer remoteModelsEnabledMu.Unlock()
	return remoteModelsEnabled
}

// POST /admin/api/models/sync — 手动触发一次模型同步
func handleAdminModelSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}
	res := triggerModelSync()
	if res.Error != "" {
		writeAPI(w, http.StatusBadGateway, apiResponse{Success: false, Error: res.Error, Message: tAPI(r, "model_sync_failed")})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: res, Message: tAPI(r, "model_sync_done")})
}
