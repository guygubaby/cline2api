package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestAdminOriginGuardRejectsCrossSiteRequests(t *testing.T) {
	called := false
	handler := adminOriginGuard(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/admin/api/password", nil)
	request.Header.Set("Origin", "https://evil.example")
	response := httptest.NewRecorder()

	handler(response, request)

	if response.Code != http.StatusForbidden || called {
		t.Fatalf("cross-site admin request: status=%d called=%v", response.Code, called)
	}
}

func TestAdminOriginGuardAllowsSameOriginRequests(t *testing.T) {
	called := false
	handler := adminOriginGuard(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/admin/api/password", nil)
	request.Header.Set("Origin", "http://127.0.0.1")
	response := httptest.NewRecorder()

	handler(response, request)

	if response.Code != http.StatusNoContent || !called {
		t.Fatalf("same-origin admin request: status=%d called=%v", response.Code, called)
	}
	if response.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("admin endpoint exposed permissive CORS")
	}
}

func TestRemoteAdminRequiresPasswordByDefault(t *testing.T) {
	withTestPool(t, &AccountPool{})
	t.Setenv(allowInsecureAdminEnv, "")
	called := false
	handler := requireAdminAuth(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodGet, "http://example.test/admin/api/stats", nil)
	request.RemoteAddr = "192.0.2.10:54321"
	response := httptest.NewRecorder()

	handler(response, request)

	if response.Code != http.StatusForbidden || called {
		t.Fatalf("remote passwordless admin: status=%d called=%v", response.Code, called)
	}
}

func TestAdminPasswordUsesArgon2AndLegacyHashesStillVerify(t *testing.T) {
	testPool := &AccountPool{}
	withTestPool(t, testPool)
	oldPoolPath := poolPath
	poolPath = filepath.Join(t.TempDir(), "accounts.json")
	t.Cleanup(func() { poolPath = oldPoolPath })
	setAdminPassword("correct horse battery staple")
	if len(testPool.AdminPasswordHash) <= len(adminPasswordHashPrefix) || testPool.AdminPasswordHash[:len(adminPasswordHashPrefix)] != adminPasswordHashPrefix {
		t.Fatalf("admin hash is not Argon2id: %q", testPool.AdminPasswordHash)
	}
	if !verifyAdminPassword("correct horse battery staple") || verifyAdminPassword("wrong") {
		t.Fatal("Argon2id password verification failed")
	}

	testPool.AdminPasswordHash = legacyAdminPasswordHash(testPool.AdminPasswordSalt, "legacy-password")
	if !verifyAdminPassword("legacy-password") {
		t.Fatal("legacy password hash compatibility failed")
	}
}
