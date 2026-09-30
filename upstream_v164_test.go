package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRefreshFailureStateOnlyExpiresRejectedTokens(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

	valid := &Account{AccessToken: "usable", ExpiresAt: now.Add(time.Hour).UnixMilli(), Status: "active"}
	applyRefreshFailureState(valid, errors.New("temporary DNS failure"), now)
	if valid.Status != "active" {
		t.Fatalf("valid token status = %q, want active", valid.Status)
	}

	unavailable := &Account{ExpiresAt: now.Add(-time.Minute).UnixMilli(), Status: "active"}
	applyRefreshFailureState(unavailable, errors.New("upstream 503"), now)
	if unavailable.Status != "cooldown" || !unavailable.CooldownUntil.Equal(now.Add(cooldownRecoveryRetry)) {
		t.Fatalf("transient failure state = %q until %s", unavailable.Status, unavailable.CooldownUntil)
	}

	rejected := &Account{AccessToken: "old", ExpiresAt: now.Add(time.Hour).UnixMilli(), Status: "active"}
	applyRefreshFailureState(rejected, &refreshRejectedError{statusCode: http.StatusUnauthorized}, now)
	if rejected.Status != "expired" {
		t.Fatalf("rejected token status = %q, want expired", rejected.Status)
	}
}

func TestClineModelOutputHardLimit(t *testing.T) {
	withIsolatedPool(t)
	body := buildUpstreamBody(map[string]any{
		"model":      "google/gemini-3.8-flash",
		"max_tokens": 128_000,
	}, false, "session")
	if body["max_tokens"] != 65_536 {
		t.Fatalf("gemini max_tokens = %#v, want 65536", body["max_tokens"])
	}

	body = buildUpstreamBody(map[string]any{
		"model":      "unknown/model",
		"max_tokens": 128_000,
	}, false, "session")
	if body["max_tokens"] != 128_000 {
		t.Fatalf("unknown model was unexpectedly clamped: %#v", body["max_tokens"])
	}
}

func TestDefaultModelUsesLivePassModelInsteadOfHardcodedFallback(t *testing.T) {
	isolated := withIsolatedPool(t)
	poolMu.Lock()
	isolated.Models = []Model{{ID: "live/pass-only", Source: "remote", Cost: "pass", Status: "active"}}
	poolMu.Unlock()
	remoteModelsEnabledMu.Lock()
	previous := remoteModelsEnabled
	remoteModelsEnabled = true
	remoteModelsEnabledMu.Unlock()
	t.Cleanup(func() {
		remoteModelsEnabledMu.Lock()
		remoteModelsEnabled = previous
		remoteModelsEnabledMu.Unlock()
	})

	if got := getDefaultModel(); got != "live/pass-only" {
		t.Fatalf("default model = %q, want live/pass-only", got)
	}
}

func TestCanceledClientDoesNotCooldownModel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	params := map[string]any{}
	attachProxyRequestContext(params, ctx)
	account := &Account{ModelCooldowns: map[string]time.Time{}}

	coolAccountAfterStreamInitializationError(params, account, "test/model", errUpstreamFirstEventTimeout)
	coolAnthropicAccountAfterPrepareError(params, account, "test/model", errUpstreamFirstEventTimeout)
	if len(account.ModelCooldowns) != 0 {
		t.Fatalf("client cancellation created model cooldowns: %#v", account.ModelCooldowns)
	}
}

func TestEnvironmentProxyIsReloadedPerRequest(t *testing.T) {
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	request := httptest.NewRequest(http.MethodGet, "https://provider.example/v1/models", nil)

	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:8101")
	t.Setenv("https_proxy", "")
	first, err := clineEnvProxy(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:8102")
	second, err := clineEnvProxy(request)
	if err != nil {
		t.Fatal(err)
	}
	if first == nil || second == nil || first.String() == second.String() || second.String() != "http://127.0.0.1:8102" {
		t.Fatalf("environment proxies = %v then %v", first, second)
	}
}

func TestClineSyncRetainsAndMarksDelistedModels(t *testing.T) {
	isolated := withIsolatedPool(t)
	poolMu.Lock()
	isolated.Models = []Model{{ID: "old/gone", Source: "remote", Cost: "free", Status: "active"}}
	poolMu.Unlock()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"free":[{"id":"new/live","tags":["FREE"]}]}`))
	}))
	defer server.Close()
	oldURL := clineRecommendedModelsURL
	clineRecommendedModelsURL = server.URL
	remoteModelsEnabledMu.Lock()
	oldRemoteEnabled := remoteModelsEnabled
	remoteModelsEnabledMu.Unlock()
	modelSyncMu.Lock()
	oldLastSync, oldSyncRan, oldSyncBusy := lastModelSync, modelSyncRan, modelSyncBusy
	modelSyncMu.Unlock()
	t.Cleanup(func() {
		clineRecommendedModelsURL = oldURL
		remoteModelsEnabledMu.Lock()
		remoteModelsEnabled = oldRemoteEnabled
		remoteModelsEnabledMu.Unlock()
		modelSyncMu.Lock()
		lastModelSync, modelSyncRan, modelSyncBusy = oldLastSync, oldSyncRan, oldSyncBusy
		modelSyncMu.Unlock()
	})

	result := syncClineModels()
	if result.Error != "" || len(result.Removed) != 1 || result.Removed[0] != "old/gone" {
		t.Fatalf("sync result = %#v", result)
	}
	poolMu.Lock()
	models := append([]Model(nil), isolated.Models...)
	poolMu.Unlock()
	foundDelisted := false
	for _, model := range models {
		if model.ID == "old/gone" {
			foundDelisted = model.Delisted
		}
	}
	if !foundDelisted {
		t.Fatalf("delisted model was not retained: %#v", models)
	}

	markModelGone("old/gone")
	poolMu.Lock()
	defer poolMu.Unlock()
	for _, model := range isolated.Models {
		if model.ID == "old/gone" {
			t.Fatalf("confirmed missing model was not removed: %#v", isolated.Models)
		}
	}
}

func TestModelGoneDetectionIsConservative(t *testing.T) {
	if !isModelGoneError(http.StatusBadRequest, `{"error":"model not found"}`) {
		t.Fatal("expected explicit model-not-found error to match")
	}
	if isModelGoneError(http.StatusTooManyRequests, `{"error":"model not found"}`) ||
		isModelGoneError(http.StatusBadRequest, `{"error":"invalid parameter"}`) {
		t.Fatal("unrelated or retryable error matched model-gone detection")
	}
}

func TestSelectedAccountExport(t *testing.T) {
	isolated := withIsolatedPool(t)
	poolMu.Lock()
	isolated.Accounts = []*Account{
		{AccountID: "one", Email: "one@example.com", RefreshToken: "token-one"},
		{AccountID: "two", Email: "two@example.com", RefreshToken: "token-two"},
	}
	poolMu.Unlock()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/export", strings.NewReader(`{"ids":["two"]}`))
	handleExportAccounts(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Tokens []struct {
			Email string `json:"email"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Tokens) != 1 || payload.Tokens[0].Email != "two@example.com" {
		t.Fatalf("exported tokens = %#v", payload.Tokens)
	}
}
