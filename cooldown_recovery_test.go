package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestCooldownRecoveryFailureSchedulesLaterRetry(t *testing.T) {
	now := time.Date(2026, 9, 2, 6, 0, 0, 0, time.UTC)
	account := &Account{
		AccountID:     "cooldown-account",
		Email:         "cooldown@example.com",
		Status:        "cooldown",
		CooldownUntil: now.Add(-time.Minute),
	}
	withTestPool(t, &AccountPool{Accounts: []*Account{account}})
	oldPoolPath := poolPath
	poolPath = filepath.Join(t.TempDir(), ".cline-accounts.json")
	t.Cleanup(func() { poolPath = oldPoolPath })

	probeCalls := 0
	runCooldownRecovery(now, func(*Account) accountTestResult {
		probeCalls++
		return accountTestResult{Error: "upstream unavailable"}
	})

	if probeCalls != 1 {
		t.Fatalf("probe calls = %d, want 1", probeCalls)
	}
	if !account.CooldownUntil.After(now.Add(30 * time.Second)) {
		t.Fatalf("failed recovery retry time = %s, want a backoff beyond the next scheduler tick", account.CooldownUntil)
	}
}
