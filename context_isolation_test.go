package main

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestUpstreamTaskIDsAreUniqueUnderConcurrency(t *testing.T) {
	const count = 10_000
	ids := make(chan string, count)
	var group sync.WaitGroup
	group.Add(count)
	for range count {
		go func() {
			defer group.Done()
			ids <- newUpstreamTaskID()
		}()
	}
	group.Wait()
	close(ids)

	seen := make(map[string]struct{}, count)
	for id := range ids {
		if !strings.HasPrefix(id, "sess_") || len(id) < len("sess_")+32 {
			t.Fatalf("task ID has insufficient entropy: %q", id)
		}
		if _, exists := seen[id]; exists {
			t.Fatalf("duplicate task ID: %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestUpstreamBodyOmitsUndocumentedSessionID(t *testing.T) {
	body := buildUpstreamBody(map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "private canary"}},
	}, true)
	if _, exists := body["session_id"]; exists {
		t.Fatalf("undocumented session_id leaked upstream: %#v", body)
	}
}

func TestTenantScopeNamespacesSharedStateAndClientCacheKeys(t *testing.T) {
	firstTenant := tenantScopeForAPIKey("first-secret-key")
	secondTenant := tenantScopeForAPIKey("second-secret-key")
	if firstTenant == secondTenant || strings.Contains(firstTenant, "first-secret-key") {
		t.Fatalf("tenant scopes are not isolated/non-reversible: %q %q", firstTenant, secondTenant)
	}

	firstSession := namespaceCompactSessionID(firstTenant, "shared-client-session")
	secondSession := namespaceCompactSessionID(secondTenant, "shared-client-session")
	if firstSession == secondSession || strings.Contains(firstSession, "shared-client-session") {
		t.Fatalf("compaction sessions are not tenant-scoped: %q %q", firstSession, secondSession)
	}

	firstParams := map[string]any{"prompt_cache_key": "shared-cache", "user": "same-user"}
	secondParams := map[string]any{"prompt_cache_key": "shared-cache", "user": "same-user"}
	namespaceClientRequestIdentity(firstParams, firstTenant)
	namespaceClientRequestIdentity(secondParams, secondTenant)
	if firstParams["prompt_cache_key"] == secondParams["prompt_cache_key"] || firstParams["user"] == secondParams["user"] {
		t.Fatalf("client identities were not tenant-scoped: first=%#v second=%#v", firstParams, secondParams)
	}
	if strings.Contains(firstParams["prompt_cache_key"].(string), "shared-cache") {
		t.Fatalf("raw client cache key remains observable: %#v", firstParams)
	}
}

func TestUnauthenticatedRequestsDoNotShareTenantState(t *testing.T) {
	first := requestWithTenantScope(httptest.NewRequest("GET", "/", nil), "")
	second := requestWithTenantScope(httptest.NewRequest("GET", "/", nil), "")
	if requestTenantScope(first) == requestTenantScope(second) {
		t.Fatal("unauthenticated requests unexpectedly share a tenant scope")
	}
}

func TestTenantScopedCompactionStateCannotCrossAPIKeys(t *testing.T) {
	compactStatesMu.Lock()
	previous := compactStates
	compactStates = map[string]*compactState{}
	compactStatesMu.Unlock()
	t.Cleanup(func() {
		compactStatesMu.Lock()
		compactStates = previous
		compactStatesMu.Unlock()
	})

	firstKey := namespaceCompactSessionID(tenantScopeForAPIKey("first-key"), "same-session")
	secondKey := namespaceCompactSessionID(tenantScopeForAPIKey("second-key"), "same-session")
	updateCompactState(firstKey, "first tenant private summary", "private recent context")
	if state := loadCompactState(secondKey); state.summary != "" || state.recent != "" {
		t.Fatalf("second tenant received first tenant state: %#v", state)
	}
	if state := loadCompactState(firstKey); state.summary != "first tenant private summary" {
		t.Fatalf("first tenant lost its own state: %#v", state)
	}
}

func TestCanonicalRequestAuditHashDoesNotExposeContent(t *testing.T) {
	params := map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "private audit canary"}},
	}
	hash := canonicalRequestAuditHash(params)
	if len(hash) != 64 || strings.Contains(hash, "private") {
		t.Fatalf("invalid request audit hash: %q", hash)
	}
	params["messages"] = []any{map[string]any{"role": "user", "content": "different canary"}}
	if hash == canonicalRequestAuditHash(params) {
		t.Fatal("different request histories produced the same audit hash")
	}
}

func TestResponseAuditLogsOnlyHMACNotContent(t *testing.T) {
	var logs bytes.Buffer
	previousWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousWriter) })

	response := wrapAuditedResponse(&http.Response{
		Body: io.NopCloser(strings.NewReader("private response canary")),
	}, "req_test", "sess_test")
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatalf("read audited response: %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatalf("close audited response: %v", err)
	}
	if strings.Contains(logs.String(), "private response canary") {
		t.Fatalf("response content leaked into audit log: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "response_prefix_hmac_sha256=") {
		t.Fatalf("response audit HMAC missing: %s", logs.String())
	}
}

func TestRequestLogIsolationMetadataTracksFinalAttempt(t *testing.T) {
	params := map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "audit canary"}},
	}
	recordUpstreamAttempt(params, "sess_first")
	recordUpstreamAttempt(params, "sess_second")
	entry := newRequestLog("anthropic", "deepseek/deepseek-v4-flash", true)
	setRequestLogIsolationMetadata(&entry, params)
	if entry.UpstreamTaskID != "sess_second" || entry.RetryCount != 1 || len(entry.RequestHMAC) != 64 {
		t.Fatalf("isolation metadata = %#v", entry)
	}
}
