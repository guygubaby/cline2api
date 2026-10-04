package main

import (
	"net/http"
	"net/http/httptest"
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

func TestAdminNeverBypassesSessionWithoutDatabase(t *testing.T) {
	previousDB := authDB
	authDB = nil
	t.Cleanup(func() { authDB = previousDB })
	called := false
	handler := requireAdminAuth(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	for _, remoteAddr := range []string{"127.0.0.1:54321", "192.0.2.10:54321"} {
		request := httptest.NewRequest(http.MethodGet, "http://example.test/admin/api/stats", nil)
		request.RemoteAddr = remoteAddr
		response := httptest.NewRecorder()
		handler(response, request)
		if response.Code != http.StatusServiceUnavailable || called {
			t.Fatalf("admin without auth database from %s: status=%d called=%v", remoteAddr, response.Code, called)
		}
	}
}

func TestAdminPasswordUsesArgon2AndLegacyHashesStillVerify(t *testing.T) {
	salt := randomHex(16)
	hash := hashAdminPassword(salt, "correct horse battery staple")
	if len(hash) <= len(adminPasswordHashPrefix) || hash[:len(adminPasswordHashPrefix)] != adminPasswordHashPrefix {
		t.Fatalf("admin hash is not Argon2id: %q", hash)
	}
	if !verifyStoredPassword(hash, salt, "correct horse battery staple") || verifyStoredPassword(hash, salt, "wrong") {
		t.Fatal("Argon2id password verification failed")
	}

	legacy := legacyAdminPasswordHash(salt, "legacy-password")
	if !verifyStoredPassword(legacy, salt, "legacy-password") {
		t.Fatal("legacy password hash compatibility failed")
	}
}
