package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestSummarizeRequestUsageByViewerDate(t *testing.T) {
	now := time.Date(2026, 9, 24, 1, 0, 0, 0, time.UTC) // 09:00 in UTC+8
	requestLogsMu.Lock()
	previous := requestLogs
	requestLogs = []RequestLog{
		{StartedAt: now.Add(-30 * time.Minute), InputTokens: 10, OutputTokens: 5, CachedTokens: 2, TotalTokens: 15},
		{StartedAt: now.Add(-10 * time.Hour), InputTokens: 20, OutputTokens: 7, TotalTokens: 27},
		{StartedAt: now.Add(-25 * time.Hour), InputTokens: 30, TotalTokens: 30},
		{StartedAt: now.Add(-8 * 24 * time.Hour), InputTokens: 40, TotalTokens: 40},
		{StartedAt: now.Add(-31 * 24 * time.Hour), InputTokens: 50, TotalTokens: 50},
	}
	requestLogsMu.Unlock()
	t.Cleanup(func() {
		requestLogsMu.Lock()
		requestLogs = previous
		requestLogsMu.Unlock()
	})

	today := summarizeRequestUsage(now, "today", -480)
	if today.Summary.Requests != 1 || today.Summary.TotalTokens != 15 || len(today.Days) != 1 || today.Days[0].Date != "2026-09-24" {
		t.Fatalf("today usage = %+v", today)
	}
	lastDay := summarizeRequestUsage(now, "1d", -480)
	if lastDay.Summary.Requests != 2 || lastDay.Summary.TotalTokens != 42 || len(lastDay.Days) != 2 || lastDay.Days[1].Date != "2026-09-23" {
		t.Fatalf("last 24 hours usage = %+v", lastDay)
	}
	week := summarizeRequestUsage(now, "7d", -480)
	if week.Summary.Requests != 3 || len(week.Days) != 7 || week.Days[6].Date != "2026-09-18" {
		t.Fatalf("seven calendar days usage = %+v", week)
	}
	all := summarizeRequestUsage(now, "all", -480)
	if all.Summary.Requests != 4 || all.Summary.TotalTokens != 112 || len(all.Days) != 9 || all.Days[8].Date != "2026-09-16" {
		t.Fatalf("all retained usage = %+v", all)
	}
	encoded, err := json.Marshal(week.Days[0])
	if err != nil || string(encoded) != `{"date":"2026-09-24","requests":1,"inputTokens":10,"outputTokens":5,"cachedTokens":2,"totalTokens":15}` {
		t.Fatalf("daily JSON = %s, err = %v", encoded, err)
	}
}

func TestPruneRequestLogsBoundsAgeAndCount(t *testing.T) {
	now := time.Now()
	old := now.Add(-31 * 24 * time.Hour)
	entries := []RequestLog{
		{ID: "old", StartedAt: old},
		{ID: "keep1", StartedAt: now.Add(-time.Hour)},
		{ID: "keep2", StartedAt: now.Add(-2 * time.Hour)},
	}

	requestLogsMu.Lock()
	requestLogs = pruneRequestLogsLocked(entries)
	requestLogsMu.Unlock()

	if len(requestLogs) != 2 {
		t.Fatalf("expected 2 entries after age pruning, got %d", len(requestLogs))
	}
	if requestLogs[0].ID != "keep1" {
		t.Fatalf("expected newest first, got %q", requestLogs[0].ID)
	}
}

func TestListRequestLogsCursorPagination(t *testing.T) {
	now := time.Now()
	entries := make([]RequestLog, 0, 75)
	for i := 0; i < 75; i++ {
		entries = append(entries, RequestLog{
			ID:        "req_" + string(rune('a'+i)),
			StartedAt: now.Add(-time.Duration(75-i) * time.Second),
		})
	}

	requestLogsMu.Lock()
	requestLogs = pruneRequestLogsLocked(entries)
	requestLogsMu.Unlock()

	page1, err := listRequestLogs(50, "")
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1.Items) != 50 || !page1.HasMore {
		t.Fatalf("page1 unexpected: len=%d hasMore=%v", len(page1.Items), page1.HasMore)
	}
	if page1.Items[0].ID != requestLogs[0].ID {
		t.Fatalf("page1 first item = %q, want %q", page1.Items[0].ID, requestLogs[0].ID)
	}

	page2, err := listRequestLogs(50, page1.NextCursor)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2.Items) != 25 || page2.HasMore {
		t.Fatalf("page2 unexpected: len=%d hasMore=%v", len(page2.Items), page2.HasMore)
	}

	if page2.Items[0].ID != requestLogs[50].ID {
		t.Fatalf("page2 first item = %q, want %q", page2.Items[0].ID, requestLogs[50].ID)
	}
}

func TestListRequestLogsRejectsInvalidCursor(t *testing.T) {
	if _, err := listRequestLogs(50, "not-a-valid-cursor"); err == nil {
		t.Fatal("expected error for invalid cursor")
	}
}

func TestParseTokenUsageCachePrecedence(t *testing.T) {
	usage := parseTokenUsage(map[string]any{
		"prompt_tokens": float64(100),
		"prompt_tokens_details": map[string]any{
			"cached_tokens": float64(40),
		},
		"cache_read_input_tokens":     float64(10),
		"cache_creation_input_tokens": float64(5),
	})
	if usage.Cached != 40 {
		t.Fatalf("expected nested precedence cached=40, got %d", usage.Cached)
	}
	if usage.Prompt != 100 {
		t.Fatalf("expected prompt=100, got %d", usage.Prompt)
	}
}
