package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

var (
	pool                      *AccountPool
	poolMu                    sync.Mutex
	poolSaveMu                sync.Mutex
	poolDeferredSaveOnce      sync.Once
	poolDeferredSaveWake      = make(chan string, 1)
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

func savePoolToPath(path string) {
	poolSaveMu.Lock()
	defer poolSaveMu.Unlock()
	poolMu.Lock()
	data, err := json.MarshalIndent(pool, "", "  ")
	poolMu.Unlock()
	if err != nil {
		log.Printf("Failed to encode accounts: %v", err)
		return
	}
	if err := writeFileDurably(path, data, 0600); err != nil {
		log.Printf("Failed to save accounts: %v", err)
	}
}

func savePool() { savePoolToPath(poolPath) }

func savePoolEventually() {
	poolDeferredSaveOnce.Do(func() {
		go func() {
			for path := range poolDeferredSaveWake {
				time.Sleep(deferredWriteDelay)
				savePoolToPath(path)
			}
		}()
	})
	select {
	case poolDeferredSaveWake <- poolPath:
	default:
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
	found := false
	for i, a := range p.Accounts {
		if a.AccountID == accountID {
			p.Accounts = append(p.Accounts[:i], p.Accounts[i+1:]...)
			found = true
			break
		}
	}
	poolMu.Unlock()
	if found {
		savePool()
	}
	return found
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
		poolMu.Lock()
		acc.Status = "expired"
		poolMu.Unlock()
		savePool()
		return fmt.Errorf("refresh token is empty")
	}

	resp, err := refreshClineToken(acc.RefreshToken)
	if err != nil {
		// A proactive refresh can fail transiently while the current access token
		// is still usable. Keep that account active so the scheduler can retry.
		if acc.AccessToken == "" || time.Now().UnixMilli() >= acc.ExpiresAt {
			poolMu.Lock()
			acc.Status = "expired"
			poolMu.Unlock()
			savePool()
		}
		return fmt.Errorf("token refresh failed: %w", err)
	}

	poolMu.Lock()
	acc.AccessToken = "workos:" + resp.Data.AccessToken
	if resp.Data.RefreshToken != "" {
		acc.RefreshToken = resp.Data.RefreshToken
	}
	acc.ExpiresAt = parseExpiry(resp.Data.ExpiresAt) - 60000
	acc.Status = "active"
	poolMu.Unlock()
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
	return pickAccountForModelWithFallback(model, true)
}

func pickAccountForModelStrict(model string) *Account {
	return pickAccountForModelWithFallback(model, false)
}

func pickAccountForModelWithFallback(model string, fallbackToActive bool) *Account {
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
	now := time.Now()
	for _, a := range active {
		if accountAvailableForModel(a, model, now, nil) {
			eligible = append(eligible, a)
		}
	}

	if len(eligible) == 0 {
		if fallbackToActive {
			// 普通模型保留旧行为，让上游返回准确的模型级错误。
			return pickAccountLocked(p)
		}
		return nil
	}

	return selectAccountByStrategy(eligible, model, &p.CurrentIdx)
}

func selectAccountByStrategy(accounts []*Account, model string, currentIndex *int) *Account {
	switch getProxyConfig().Strategy {
	case "fill":
		return accounts[0]
	case "random":
		index := int(time.Now().UnixNano() % int64(len(accounts)))
		return accounts[index]
	case "least_latency":
		if model == "" {
			return accounts[0]
		}
		return fastestAccountForModel(accounts, model)
	default: // round_robin
		if *currentIndex >= len(accounts) {
			*currentIndex = 0
		}
		account := accounts[*currentIndex]
		*currentIndex = (*currentIndex + 1) % len(accounts)
		return account
	}
}

func fastestAccountForModel(accounts []*Account, model string) *Account {
	var fastest *Account
	fastestLatency := 0.0
	for _, account := range accounts {
		latency, known := accountLatency(account, model)
		if !known {
			return account
		}
		if fastest == nil || latency < fastestLatency {
			fastest = account
			fastestLatency = latency
		}
	}
	if fastest != nil {
		return fastest
	}
	return accounts[0]
}

func observeAccountModelLatency(account *Account, model string, elapsed time.Duration) {
	if account == nil || model == "" || elapsed <= 0 {
		return
	}
	loadPool()
	poolMu.Lock()
	if account.ModelLatencies == nil {
		account.ModelLatencies = map[string]*ModelLatencyStat{}
	}
	stat := account.ModelLatencies[model]
	if stat == nil {
		stat = &ModelLatencyStat{}
		account.ModelLatencies[model] = stat
	}
	observeModelLatency(stat, elapsed)
	poolMu.Unlock()
	savePoolEventually()
}

func bestAlternativeAccountLatency(account *Account, model string) float64 {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()
	best := 0.0
	for _, candidate := range p.Accounts {
		if candidate == nil || candidate == account || candidate.Status != "active" {
			continue
		}
		stat := candidate.ModelLatencies[model]
		if stat == nil || stat.Samples == 0 || stat.EWMAms <= 0 {
			continue
		}
		if best == 0 || stat.EWMAms < best {
			best = stat.EWMAms
		}
	}
	return best
}

func coolSlowAccountIfNeeded(account *Account, model string, elapsed time.Duration) {
	if anomalouslySlowLatency(elapsed, bestAlternativeAccountLatency(account, model)) {
		setModelCooldown(account, model, time.Now().Add(slowChannelCooldown))
	}
}

// pickAlternativeAccountForModel returns one eligible account after excluded.
func pickAlternativeAccountForModel(model string, excluded *Account) *Account {
	accounts := pickAlternativeAccountsForModel(model, excluded, 1)
	if len(accounts) == 0 {
		return nil
	}
	return accounts[0]
}

func sameAccount(left, right *Account) bool {
	if left == nil || right == nil {
		return false
	}
	return left == right || (left.AccountID != "" && left.AccountID == right.AccountID)
}

func accountAvailableForModel(account *Account, model string, now time.Time, excluded *Account) bool {
	if account == nil || account.Status != "active" || sameAccount(account, excluded) {
		return false
	}
	until, cooling := account.ModelCooldowns[model]
	if !cooling {
		return true
	}
	if now.Before(until) {
		return false
	}
	delete(account.ModelCooldowns, model)
	return true
}

func accountIndex(accounts []*Account, target *Account) int {
	for index, account := range accounts {
		if sameAccount(account, target) {
			return index
		}
	}
	return -1
}

func availableAccountsForModel(accounts []*Account, model string, excluded *Account, start int) []*Account {
	available := make([]*Account, 0, len(accounts))
	now := time.Now()
	for offset := 0; offset < len(accounts); offset++ {
		account := accounts[(start+offset)%len(accounts)]
		if accountAvailableForModel(account, model, now, excluded) {
			available = append(available, account)
		}
	}
	return available
}

func accountLatency(account *Account, model string) (float64, bool) {
	stat := account.ModelLatencies[model]
	if stat == nil || stat.Samples == 0 || stat.EWMAms <= 0 {
		return 0, false
	}
	return stat.EWMAms, true
}

func orderAccountsByLatency(accounts []*Account, model string) {
	sort.SliceStable(accounts, func(left, right int) bool {
		leftLatency, leftKnown := accountLatency(accounts[left], model)
		rightLatency, rightKnown := accountLatency(accounts[right], model)
		if leftKnown != rightKnown {
			return !leftKnown
		}
		if !leftKnown {
			return false
		}
		return leftLatency < rightLatency
	})
}

func rotateAccounts(accounts []*Account, start int) []*Account {
	if start == 0 || len(accounts) < 2 {
		return accounts
	}
	rotated := make([]*Account, len(accounts))
	for index := range accounts {
		rotated[index] = accounts[(start+index)%len(accounts)]
	}
	return rotated
}

func pickAlternativeAccountsForModel(model string, excluded *Account, limit int) []*Account {
	if limit <= 0 {
		return nil
	}
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	strategy := getProxyConfig().Strategy
	start := 0
	if strategy == "round_robin" || strategy == "" {
		start = accountIndex(p.Accounts, excluded) + 1
	}
	candidates := availableAccountsForModel(p.Accounts, model, excluded, start)
	if len(candidates) == 0 {
		return nil
	}

	switch strategy {
	case "least_latency":
		orderAccountsByLatency(candidates, model)
	case "random":
		candidates = rotateAccounts(candidates, randIntn(len(candidates)))
	}
	if limit > len(candidates) {
		limit = len(candidates)
	}
	return append([]*Account(nil), candidates[:limit]...)
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
	return selectAccountByStrategy(active, "", &p.CurrentIdx)
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
		if len(a.ModelLatencies) > 0 {
			cp.ModelLatencies = make(map[string]*ModelLatencyStat, len(a.ModelLatencies))
			for modelID, latency := range a.ModelLatencies {
				latencyCopy := *latency
				cp.ModelLatencies[modelID] = &latencyCopy
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
