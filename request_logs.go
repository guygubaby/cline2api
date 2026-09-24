package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"sync"
	"time"
)

const (
	requestLogMaxEntries   = 5000
	requestLogMaxAge       = 30 * 24 * time.Hour
	requestLogDefaultLimit = 50
	requestLogMaxLimit     = 100
)

type RequestLog struct {
	ID           string    `json:"id"`
	StartedAt    time.Time `json:"startedAt"`
	FinishedAt   time.Time `json:"finishedAt"`
	AccountID    string    `json:"accountId"`
	AccountEmail string    `json:"accountEmail"`
	Protocol     string    `json:"protocol"`
	// Upstream 标记上游来源：cline、opencode 或 custom:<渠道名>
	Upstream             string  `json:"upstream,omitempty"`
	Model                string  `json:"model"`
	Stream               bool    `json:"stream"`
	InputTokens          int64   `json:"inputTokens"`
	OutputTokens         int64   `json:"outputTokens"`
	CachedTokens         int64   `json:"cachedTokens"`
	TotalTokens          int64   `json:"totalTokens"`
	UsageAvailable       bool    `json:"usageAvailable"`
	DurationMs           int64   `json:"durationMs"`
	TTFTMs               int64   `json:"ttftMs"`
	UpstreamTTFTMs       int64   `json:"upstreamTtftMs,omitempty"`
	ThinkingTTFTMs       int64   `json:"thinkingTtftMs,omitempty"`
	VisibleTTFTMs        int64   `json:"visibleTtftMs,omitempty"`
	OutputTPS            float64 `json:"outputTokensPerSecond"`
	Completed            bool    `json:"completed"`
	Error                string  `json:"error,omitempty"`
	ErrorCode            string  `json:"errorCode,omitempty"`
	FinishReason         string  `json:"finishReason,omitempty"`
	SawDone              bool    `json:"sawDone,omitempty"`
	RetryCount           int     `json:"retryCount,omitempty"`
	UpstreamAttempts     int     `json:"upstreamAttempts,omitempty"`
	EstimatedInputTokens int     `json:"estimatedInputTokens,omitempty"`
	RequestHMAC          string  `json:"requestHmac,omitempty"`
	UpstreamTaskID       string  `json:"upstreamTaskId,omitempty"`
	ReasoningChars       int     `json:"reasoningChars,omitempty"`
	ThinkingTokens       int64   `json:"thinkingTokens,omitempty"`
	RetrySuppressed      bool    `json:"retrySuppressed,omitempty"`
	promptEchoGuard      *promptEchoGuard
}

var (
	requestLogs         []RequestLog
	requestLogsMu       sync.Mutex
	requestLogsSaveMu   sync.Mutex
	requestLogsPath     string
	requestLogsDirty    bool
	requestLogsSaveOnce sync.Once
	requestLogsSaveWake = make(chan string, 1)
)

func newRequestLog(protocol, model string, stream bool) RequestLog {
	return RequestLog{
		ID:        newProxyRequestID(),
		StartedAt: time.Now(),
		Protocol:  protocol,
		Model:     model,
		Stream:    stream,
	}
}

func init() {
	requestLogsPath = resolveDataPath(".cline-request-logs.json")
}

func loadRequestLogs() {
	data, err := os.ReadFile(requestLogsPath)
	if err != nil {
		return
	}
	var entries []RequestLog
	if err := json.Unmarshal(data, &entries); err != nil {
		return
	}
	requestLogsMu.Lock()
	requestLogs = pruneRequestLogsLocked(entries)
	requestLogsMu.Unlock()
}

func pruneRequestLogsLocked(entries []RequestLog) []RequestLog {
	if len(entries) == 0 {
		return entries
	}
	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].StartedAt.Equal(entries[j].StartedAt) {
			return entries[i].StartedAt.After(entries[j].StartedAt)
		}
		return entries[i].ID > entries[j].ID
	})

	cutoff := time.Now().Add(-requestLogMaxAge)
	pruned := entries[:0]
	for _, e := range entries {
		if e.StartedAt.Before(cutoff) {
			continue
		}
		pruned = append(pruned, e)
	}
	if len(pruned) > requestLogMaxEntries {
		pruned = pruned[:requestLogMaxEntries]
	}
	return pruned
}

func requestLogNewer(left, right RequestLog) bool {
	if !left.StartedAt.Equal(right.StartedAt) {
		return left.StartedAt.After(right.StartedAt)
	}
	return left.ID > right.ID
}

func insertRequestLogLocked(entries []RequestLog, entry RequestLog) []RequestLog {
	index := sort.Search(len(entries), func(index int) bool {
		return !requestLogNewer(entries[index], entry)
	})
	entries = append(entries, RequestLog{})
	copy(entries[index+1:], entries[index:])
	entries[index] = entry
	cutoff := time.Now().Add(-requestLogMaxAge)
	for len(entries) > 0 && entries[len(entries)-1].StartedAt.Before(cutoff) {
		entries = entries[:len(entries)-1]
	}
	if len(entries) > requestLogMaxEntries {
		entries = entries[:requestLogMaxEntries]
	}
	return entries
}

func flushRequestLogs(path string) {
	requestLogsSaveMu.Lock()
	defer requestLogsSaveMu.Unlock()
	requestLogsMu.Lock()
	if !requestLogsDirty {
		requestLogsMu.Unlock()
		return
	}
	entries := append([]RequestLog(nil), requestLogs...)
	requestLogsDirty = false
	requestLogsMu.Unlock()
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		log.Printf("Failed to encode request logs: %v", err)
		requestLogsMu.Lock()
		requestLogsDirty = true
		requestLogsMu.Unlock()
		return
	}
	if err := writeFileDurably(path, data, 0600); err != nil {
		log.Printf("Failed to save request logs: %v", err)
		requestLogsMu.Lock()
		requestLogsDirty = true
		requestLogsMu.Unlock()
	}
}

func saveRequestLogsEventually() {
	requestLogsSaveOnce.Do(func() {
		go func() {
			for path := range requestLogsSaveWake {
				time.Sleep(deferredWriteDelay)
				flushRequestLogs(path)
			}
		}()
	})
	select {
	case requestLogsSaveWake <- requestLogsPath:
	default:
	}
}

func appendRequestLog(entry RequestLog) {
	if entry.ID == "" {
		entry.ID = fmt.Sprintf("req_%d", entry.StartedAt.UnixNano())
	}
	requestLogsMu.Lock()
	requestLogs = insertRequestLogLocked(requestLogs, entry)
	requestLogsDirty = true
	requestLogsMu.Unlock()
	saveRequestLogsEventually()
}

func setRequestLogEffectiveModel(entry *RequestLog, params map[string]any) {
	if entry == nil {
		return
	}
	if model, _ := params["model"].(string); model != "" {
		entry.Model = model
	}
}

func setRequestLogIsolationMetadata(entry *RequestLog, params map[string]any) {
	if entry == nil {
		return
	}
	entry.RequestHMAC, _ = params[proxyAuditHashParamKey].(string)
	if entry.RequestHMAC == "" {
		entry.RequestHMAC = canonicalRequestAuditHash(params)
		params[proxyAuditHashParamKey] = entry.RequestHMAC
	}
	entry.UpstreamTaskID, _ = params[proxyUpstreamTaskParamKey].(string)
	if attempts, _ := params[proxyUpstreamCountParamKey].(int); attempts > 0 {
		entry.UpstreamAttempts = attempts
		if entry.RetryCount < attempts-1 {
			entry.RetryCount = attempts - 1
		}
	}
	if upstreamTTFT, ok := params[proxyUpstreamTTFTParamKey].(int64); ok && upstreamTTFT > 0 {
		entry.UpstreamTTFTMs = upstreamTTFT
	}
}

type requestLogPage struct {
	Items      []RequestLog `json:"items"`
	NextCursor string       `json:"nextCursor"`
	HasMore    bool         `json:"hasMore"`
}

type requestUsageSummary struct {
	Requests     int64 `json:"requests"`
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
	CachedTokens int64 `json:"cachedTokens"`
	TotalTokens  int64 `json:"totalTokens"`
}

type requestUsageDay struct {
	Date string `json:"date"`
	requestUsageSummary
}

type requestUsagePeriod struct {
	Summary requestUsageSummary `json:"summary"`
	Days    []requestUsageDay   `json:"days"`
}

func (s *requestUsageSummary) add(entry RequestLog) {
	s.Requests++
	s.InputTokens += entry.InputTokens
	s.OutputTokens += entry.OutputTokens
	s.CachedTokens += entry.CachedTokens
	s.TotalTokens += entry.TotalTokens
}

// summarizeRequestUsage groups retained request logs by the viewer's calendar dates.
func summarizeRequestUsage(now time.Time, period string, timezoneOffsetMinutes int) requestUsagePeriod {
	location := time.FixedZone("dashboard", -timezoneOffsetMinutes*60)
	localNow := now.In(location)
	today := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, location)
	start := today
	switch period {
	case "1d":
		start = now.Add(-24 * time.Hour)
	case "7d":
		start = today.AddDate(0, 0, -6)
	case "14d":
		start = today.AddDate(0, 0, -13)
	case "30d":
		start = today.AddDate(0, 0, -29)
	}
	localStart := start.In(location)
	firstDay := time.Date(localStart.Year(), localStart.Month(), localStart.Day(), 0, 0, 0, 0, location)

	result := requestUsagePeriod{Days: make([]requestUsageDay, 0, 30)}
	dayIndexes := make(map[string]int)
	for day := today; !day.Before(firstDay); day = day.AddDate(0, 0, -1) {
		date := day.Format("2006-01-02")
		dayIndexes[date] = len(result.Days)
		result.Days = append(result.Days, requestUsageDay{Date: date})
	}

	requestLogsMu.Lock()
	defer requestLogsMu.Unlock()
	for _, entry := range requestLogs {
		if entry.StartedAt.Before(start) || entry.StartedAt.After(now) {
			continue
		}
		index, ok := dayIndexes[entry.StartedAt.In(location).Format("2006-01-02")]
		if !ok {
			continue
		}
		result.Summary.add(entry)
		result.Days[index].requestUsageSummary.add(entry)
	}
	return result
}

func encodeCursor(entry RequestLog) string {
	key := fmt.Sprintf("%d|%s", entry.StartedAt.UnixNano(), entry.ID)
	return base64.RawURLEncoding.EncodeToString([]byte(key))
}

func decodeCursor(cursor string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("invalid cursor")
	}
	var ts int64
	var id string
	if _, err := fmt.Sscanf(string(raw), "%d|%s", &ts, &id); err != nil || id == "" {
		return time.Time{}, "", fmt.Errorf("invalid cursor")
	}
	return time.Unix(0, ts), id, nil
}

func listRequestLogs(limit int, cursor string) (requestLogPage, error) {
	if limit <= 0 {
		limit = requestLogDefaultLimit
	}
	if limit > requestLogMaxLimit {
		limit = requestLogMaxLimit
	}

	var afterTime time.Time
	var afterID string
	if cursor != "" {
		t, id, err := decodeCursor(cursor)
		if err != nil {
			return requestLogPage{}, err
		}
		afterTime = t
		afterID = id
	}

	requestLogsMu.Lock()
	defer requestLogsMu.Unlock()

	result := make([]RequestLog, 0, limit)
	var lastEntry RequestLog
	for _, e := range requestLogs {
		if cursor != "" {
			if e.StartedAt.After(afterTime) {
				continue
			}
			if e.StartedAt.Equal(afterTime) && e.ID >= afterID {
				continue
			}
		}
		result = append(result, e)
		lastEntry = e
		if len(result) >= limit {
			break
		}
	}

	page := requestLogPage{Items: result}
	if len(result) == limit {
		page.NextCursor = encodeCursor(lastEntry)
		page.HasMore = true
	}
	return page, nil
}

func finalizeRequestLog(entry *RequestLog, usage tokenUsage, firstOutputAt time.Time, startedAt time.Time, completed bool, errMsg string) {
	entry.FinishedAt = time.Now()
	entry.DurationMs = entry.FinishedAt.Sub(startedAt).Milliseconds()
	entry.Completed = completed
	entry.Error = truncate(errMsg, 200)

	if usage.Valid {
		entry.UsageAvailable = true
		entry.InputTokens = usage.Prompt
		entry.OutputTokens = usage.Completion
		entry.CachedTokens = usage.Cached
		entry.TotalTokens = usage.Total
	}

	if !firstOutputAt.IsZero() && usage.Valid && usage.Completion > 0 {
		entry.TTFTMs = firstOutputAt.Sub(startedAt).Milliseconds()
		generationMs := entry.FinishedAt.Sub(firstOutputAt).Seconds()
		if generationMs > 0 {
			entry.OutputTPS = float64(usage.Completion) / generationMs
		}
	}

	entry.promptEchoGuard = nil
	appendRequestLog(*entry)
}
