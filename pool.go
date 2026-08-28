package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	pool                      *AccountPool
	poolMu                    sync.Mutex
	poolSaveMu                sync.Mutex
	poolPath                  string
	tokenRefreshSchedulerOnce sync.Once
)

const (
	tokenRefreshCheckInterval = time.Minute
	tokenRefreshAhead         = 5 * time.Minute
)

type tokenRefreshFailure struct {
	AccountID string `json:"accountId"`
	Email     string `json:"email"`
	Error     string `json:"error"`
}

type tokenRefreshSummary struct {
	Total     int                   `json:"total"`
	Refreshed int                   `json:"refreshed"`
	Skipped   int                   `json:"skipped"`
	Failed    int                   `json:"failed"`
	Failures  []tokenRefreshFailure `json:"failures,omitempty"`
}

func init() {
	poolPath = resolveDataPath(".cline-accounts.json")
}

// resolveDataPath 按优先级查找数据文件：exe 目录 → 工作目录 → 用户主目录。
// 找到则用该路径（兼容旧版本在项目根目录存储的文件）；
// 都找不到则回退到 exe 目录（首次运行会在该位置创建）。
func resolveDataPath(filename string) string {
	// 1. exe 所在目录
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), filename)
		if fileExists(p) {
			return p
		}
	}
	// 2. 当前工作目录
	if pwd, err := os.Getwd(); err == nil {
		p := filepath.Join(pwd, filename)
		if fileExists(p) {
			return p
		}
	}
	// 3. 用户主目录下的 .cline2api/
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".cline2api", filename)
		if fileExists(p) {
			return p
		}
	}
	// 回退：exe 目录（首次运行在此创建）
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), filename)
	}
	pwd, _ := os.Getwd()
	return filepath.Join(pwd, filename)
}

func loadPool() *AccountPool {
	poolMu.Lock()
	defer poolMu.Unlock()

	if pool != nil {
		return pool
	}

	data, err := os.ReadFile(poolPath)
	if err != nil {
		pool = &AccountPool{Accounts: []*Account{}, Keys: []string{}, Models: []Model{}}
		return pool
	}

	var p AccountPool
	if err := json.Unmarshal(data, &p); err != nil {
		pool = &AccountPool{Accounts: []*Account{}, Keys: []string{}, Models: []Model{}}
		return pool
	}

	if p.Accounts == nil {
		p.Accounts = []*Account{}
	}
	if p.Keys == nil {
		p.Keys = []string{}
	}
	if p.Models == nil {
		p.Models = []Model{}
	}
	pool = &p
	return pool
}

func savePool() {
	poolSaveMu.Lock()
	defer poolSaveMu.Unlock()

	data, err := json.MarshalIndent(pool, "", "  ")
	if err != nil {
		log.Printf("Failed to encode accounts: %v", err)
		return
	}
	if err := writeFileDurably(poolPath, data, 0600); err != nil {
		log.Printf("Failed to save accounts: %v", err)
	}
}

func addAccount(acc *Account) {
	p := loadPool()
	poolMu.Lock()
	p.Accounts = append(p.Accounts, acc)
	poolMu.Unlock()
	savePool()
}

func removeAccount(accountID string) bool {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	for i, a := range p.Accounts {
		if a.AccountID == accountID {
			p.Accounts = append(p.Accounts[:i], p.Accounts[i+1:]...)
			savePool()
			return true
		}
	}
	return false
}

func getAccountByID(accountID string) *Account {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	for _, a := range p.Accounts {
		if a.AccountID == accountID {
			return a
		}
	}
	return nil
}

func refreshAccountToken(acc *Account) error {
	if acc == nil {
		return fmt.Errorf("account is nil")
	}

	acc.tokenMu.Lock()
	defer acc.tokenMu.Unlock()
	return refreshAccountTokenLocked(acc)
}

func refreshAccountTokenLocked(acc *Account) error {
	if acc.RefreshToken == "" {
		acc.Status = "expired"
		savePool()
		return fmt.Errorf("refresh token is empty")
	}

	resp, err := refreshClineToken(acc.RefreshToken)
	if err != nil {
		// A proactive refresh can fail transiently while the current access token
		// is still usable. Keep that account active so the scheduler can retry.
		if acc.AccessToken == "" || time.Now().UnixMilli() >= acc.ExpiresAt {
			acc.Status = "expired"
			savePool()
		}
		return fmt.Errorf("token refresh failed: %w", err)
	}

	acc.AccessToken = "workos:" + resp.Data.AccessToken
	if resp.Data.RefreshToken != "" {
		acc.RefreshToken = resp.Data.RefreshToken
	}
	acc.ExpiresAt = parseExpiry(resp.Data.ExpiresAt) - 60000
	acc.Status = "active"
	savePool()
	return nil
}

func snapshotAccounts() []*Account {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	accounts := make([]*Account, len(p.Accounts))
	copy(accounts, p.Accounts)
	return accounts
}

func refreshAllAccountTokens(accounts []*Account) tokenRefreshSummary {
	summary := tokenRefreshSummary{Total: len(accounts)}
	for _, acc := range accounts {
		if acc == nil {
			summary.Failed++
			summary.Failures = append(summary.Failures, tokenRefreshFailure{Error: "missing refresh token"})
			continue
		}
		if err := refreshAccountToken(acc); err != nil {
			summary.Failed++
			summary.Failures = append(summary.Failures, tokenRefreshFailure{
				AccountID: acc.AccountID,
				Email:     acc.Email,
				Error:     truncate(err.Error(), 200),
			})
			continue
		}
		summary.Refreshed++
	}
	return summary
}

func currentAccountAccessToken(acc *Account) string {
	if acc == nil {
		return ""
	}
	acc.tokenMu.Lock()
	defer acc.tokenMu.Unlock()
	return acc.AccessToken
}

func refreshAccountTokenIfExpiring(acc *Account, now time.Time, refreshBefore time.Duration) (bool, error) {
	if acc == nil {
		return false, nil
	}

	acc.tokenMu.Lock()
	defer acc.tokenMu.Unlock()

	if acc.Status != "active" {
		return false, nil
	}
	if acc.AccessToken != "" && acc.ExpiresAt > now.Add(refreshBefore).UnixMilli() {
		return false, nil
	}
	if err := refreshAccountTokenLocked(acc); err != nil {
		return false, err
	}
	return true, nil
}

func refreshExpiringAccountTokens(accounts []*Account, now time.Time, refreshBefore time.Duration) tokenRefreshSummary {
	summary := tokenRefreshSummary{Total: len(accounts)}
	for _, acc := range accounts {
		refreshed, err := refreshAccountTokenIfExpiring(acc, now, refreshBefore)
		if err != nil {
			summary.Failed++
			summary.Failures = append(summary.Failures, tokenRefreshFailure{
				AccountID: acc.AccountID,
				Email:     acc.Email,
				Error:     truncate(err.Error(), 200),
			})
			continue
		}
		if refreshed {
			summary.Refreshed++
		} else {
			summary.Skipped++
		}
	}
	return summary
}

func startTokenRefreshScheduler() {
	tokenRefreshSchedulerOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(tokenRefreshCheckInterval)
			defer ticker.Stop()
			for now := range ticker.C {
				summary := refreshExpiringAccountTokens(snapshotAccounts(), now, tokenRefreshAhead)
				if summary.Refreshed > 0 || summary.Failed > 0 {
					log.Printf("token refresh check: refreshed=%d failed=%d", summary.Refreshed, summary.Failed)
				}
				for _, failure := range summary.Failures {
					log.Printf("token refresh check failed: account=%s error=%s", truncateEmail(failure.Email), failure.Error)
				}
			}
		}()
	})
}

func pickAccount() *Account {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()
	return pickAccountLocked(p)
}

// pickAccountForModel 按轮询/策略挑选一个「该模型未处于模型级冷却」的账号；
// 所有 active 账号对该模型都冷却时回退到普通 pickAccount（请求会得到模型级 429 提示）。
// 空模型名等同于 pickAccount。
func pickAccountForModel(model string) *Account {
	if model == "" {
		return pickAccount()
	}

	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	active := make([]*Account, 0)
	for _, a := range p.Accounts {
		if a.Status == "active" {
			active = append(active, a)
		}
	}
	if len(active) == 0 {
		return nil
	}

	// 该模型未冷却的账号列表
	eligible := make([]*Account, 0, len(active))
	for _, a := range active {
		until, cool := a.ModelCooldowns[model]
		if !cool || time.Now().After(until) {
			if cool {
				delete(a.ModelCooldowns, model)
			}
			eligible = append(eligible, a)
		}
	}

	if len(eligible) == 0 {
		// 全部冷却 → 回退普通轮询，请求会带出模型级错误
		return pickAccountLocked(p)
	}

	cfg := getProxyConfig()
	var acc *Account
	switch cfg.Strategy {
	case "fill":
		acc = eligible[0]
	case "random":
		n := time.Now().UnixNano() % int64(len(eligible))
		acc = eligible[n]
	default: // round_robin
		if p.CurrentIdx >= len(eligible) {
			p.CurrentIdx = 0
		}
		acc = eligible[p.CurrentIdx]
		p.CurrentIdx = (p.CurrentIdx + 1) % len(eligible)
	}
	savePool()
	return acc
}

// pickAlternativeAccountForModel returns one active account other than the
// account that produced an empty response. It is used only for one bounded
// retry and does not change the configured pool strategy or cooldown state.
func pickAlternativeAccountForModel(model string, excluded *Account) *Account {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	now := time.Now()
	for _, account := range p.Accounts {
		if account == nil || account.Status != "active" || account == excluded {
			continue
		}
		if excluded != nil && account.AccountID != "" && account.AccountID == excluded.AccountID {
			continue
		}
		if until, cooling := account.ModelCooldowns[model]; cooling && now.Before(until) {
			continue
		}
		return account
	}
	return nil
}

// pickAccountLocked 在已持有 poolMu 的前提下执行普通轮询挑选（供 pickAccountForModel 回退用）。
func pickAccountLocked(p *AccountPool) *Account {
	active := make([]*Account, 0)
	for _, a := range p.Accounts {
		if a.Status == "active" {
			active = append(active, a)
		}
	}
	if len(active) == 0 {
		return nil
	}
	cfg := getProxyConfig()
	var acc *Account
	switch cfg.Strategy {
	case "fill":
		acc = active[0]
	case "random":
		n := time.Now().UnixNano() % int64(len(active))
		acc = active[n]
	default:
		if p.CurrentIdx >= len(active) {
			p.CurrentIdx = 0
		}
		acc = active[p.CurrentIdx]
		p.CurrentIdx = (p.CurrentIdx + 1) % len(active)
	}
	savePool()
	return acc
}

func ensureAccountToken(acc *Account) (string, error) {
	if acc == nil {
		return "", fmt.Errorf("account is nil")
	}

	acc.tokenMu.Lock()
	defer acc.tokenMu.Unlock()

	if acc.AccessToken != "" && time.Now().UnixMilli() < acc.ExpiresAt {
		return acc.AccessToken, nil
	}

	if err := refreshAccountTokenLocked(acc); err != nil {
		return "", err
	}

	return acc.AccessToken, nil
}

func listAccounts() []*Account {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	result := make([]*Account, len(p.Accounts))
	for i, a := range p.Accounts {
		// Don't expose tokens
		cp := &Account{
			AccountID:        a.AccountID,
			Email:            a.Email,
			Status:           a.Status,
			CooldownUntil:    a.CooldownUntil,
			LastUsed:         a.LastUsed,
			UsageCount:       a.UsageCount,
			PromptTokens:     a.PromptTokens,
			CompletionTokens: a.CompletionTokens,
			TotalTokens:      a.TotalTokens,
			CachedTokens:     a.CachedTokens,
			CreatedAt:        a.CreatedAt,
		}
		// 按模型细分统计（脱敏拷贝）
		if len(a.ModelStats) > 0 {
			cp.ModelStats = make(map[string]*ModelStat, len(a.ModelStats))
			for mid, st := range a.ModelStats {
				sc := *st
				cp.ModelStats[mid] = &sc
			}
		}
		// 模型级冷却（脱敏拷贝）
		if len(a.ModelCooldowns) > 0 {
			cp.ModelCooldowns = make(map[string]time.Time, len(a.ModelCooldowns))
			for mid, until := range a.ModelCooldowns {
				cp.ModelCooldowns[mid] = until
			}
		}
		result[i] = cp
	}
	return result
}

func addAccountFromDeviceAuth() (*Account, error) {
	fmt.Println()
	fmt.Println("=== Add New Cline Account (OAuth) ===")
	fmt.Println()

	device, err := workosDeviceAuth()
	if err != nil {
		return nil, err
	}

	authURL := device.VerificationURIComplete
	if authURL == "" {
		authURL = device.VerificationURI
	}

	fmt.Println("  1. Open this URL in your browser:")
	fmt.Println("     " + authURL)
	fmt.Println("  2. Enter code: " + device.UserCode)
	fmt.Println("  3. Log in with Google, GitHub, or email")
	fmt.Println()

	_ = openBrowser(authURL)
	fmt.Println("  Waiting for authorization...")

	interval := device.Interval
	if interval < 5 {
		interval = 5
	}
	expiresIn := device.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 300
	}

	workosTok, err := pollWorkosToken(device.DeviceCode, interval, expiresIn)
	if err != nil {
		return nil, err
	}

	fmt.Println("  WorkOS authorized. Registering with Cline...")

	cline, err := registerWithCline(workosTok.AccessToken, workosTok.RefreshToken)
	if err != nil {
		return nil, err
	}

	if cline.Data.RefreshToken == "" {
		return nil, fmt.Errorf("cline registration missing refresh token")
	}

	email := "unknown"
	if cline.Data.UserInfo != nil && cline.Data.UserInfo.Email != "" {
		email = cline.Data.UserInfo.Email
	}

	acc := &Account{
		AccountID:    fmt.Sprintf("acc_%d", time.Now().UnixMilli()),
		Email:        email,
		RefreshToken: cline.Data.RefreshToken,
		AccessToken:  "workos:" + cline.Data.AccessToken,
		ExpiresAt:    parseExpiry(cline.Data.ExpiresAt) - 60000,
		Status:       "active",
		CreatedAt:    time.Now(),
	}

	addAccount(acc)
	fmt.Printf("  Account added! Email: %s\n", email)
	return acc, nil
}
