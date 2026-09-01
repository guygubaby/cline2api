package main

import (
	"sync"
	"time"
)

type Account struct {
	tokenMu          sync.Mutex
	AccountID        string    `json:"accountId"`
	Email            string    `json:"email"`
	RefreshToken     string    `json:"refreshToken"`
	AccessToken      string    `json:"-"`
	ExpiresAt        int64     `json:"-"`
	Status           string    `json:"status"` // active, cooldown, expired
	CooldownUntil    time.Time `json:"cooldownUntil,omitempty"`
	LastUsed         time.Time `json:"lastUsed"`
	UsageCount       int64     `json:"usageCount"`
	PromptTokens     int64     `json:"promptTokens"`
	CompletionTokens int64     `json:"completionTokens"`
	TotalTokens      int64     `json:"totalTokens"`
	CachedTokens     int64     `json:"cachedTokens"`
	CreatedAt        time.Time `json:"createdAt"`
	// ModelStats 按模型细分的用量统计（仅记录 free 模型）
	ModelStats map[string]*ModelStat `json:"modelStats,omitempty"`
	// ModelCooldowns 模型级冷却：modelID → 恢复时间（429 时记录，只暂停该模型）
	ModelCooldowns map[string]time.Time `json:"modelCooldowns,omitempty"`
	// ModelLatencies 保存该账号各模型的首个语义事件延迟，用于 least_latency 选择。
	ModelLatencies map[string]*ModelLatencyStat `json:"modelLatencies,omitempty"`
}

type ModelLatencyStat struct {
	EWMAms  float64 `json:"ewmaMs"`
	Samples int64   `json:"samples"`
}

type Model struct {
	ID           string `json:"id"`
	Provider     string `json:"provider"`
	Cost         string `json:"cost"`   // "free" | "pass"
	Status       string `json:"status"` // "active"
	Custom       bool   `json:"custom"` // true=用户手动添加，可删除
	ChannelCount int    `json:"channelCount,omitempty"`
	// Source 标记模型来源："remote"=Cline，"zen"=opencode，"custom_provider"=第三方渠道，空=内置/用户自定义
	Source string `json:"source,omitempty"`
	// Context / Output 上下文与最大输出 token（opencode 模型记录；0=未知）
	Context int `json:"context,omitempty"`
	Output  int `json:"output,omitempty"`
}

// ModelStat 是单个模型在某账号下的用量统计（仅统计 free 模型）。
type ModelStat struct {
	ModelID          string `json:"modelId"`
	Cost             string `json:"cost"`
	UsageCount       int64  `json:"usageCount"`
	PromptTokens     int64  `json:"promptTokens"`
	CompletionTokens int64  `json:"completionTokens"`
	TotalTokens      int64  `json:"totalTokens"`
	CachedTokens     int64  `json:"cachedTokens"`
}

type AccountPool struct {
	Accounts            []*Account `json:"accounts"`
	CurrentIdx          int        `json:"currentIdx"`
	Keys                []string   `json:"keys,omitempty"`
	Models              []Model    `json:"models,omitempty"`
	DefaultModel        string     `json:"defaultModel,omitempty"`
	ModelListConfigured bool       `json:"modelListConfigured,omitempty"`
	ListedModelIDs      []string   `json:"listedModelIds,omitempty"`
	AccountStrategy     string     `json:"accountStrategy,omitempty"`
	AnthropicEffort     string     `json:"anthropicEffort,omitempty"`
	// 访问设置：监听地址与管理后台密码（后台 UI 保存）
	ListenHost        string `json:"listenHost,omitempty"`
	AdminPasswordHash string `json:"adminPasswordHash,omitempty"`
	AdminPasswordSalt string `json:"adminPasswordSalt,omitempty"`
}

type LoginMethod int

const (
	MethodDeviceOAuth LoginMethod = iota
	MethodRefreshToken
	MethodSSOCookie
)
