package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestAdminFrontendAssets(t *testing.T) {
	root := httptest.NewRecorder()
	adminStaticHandler(root, httptest.NewRequest(http.MethodGet, "/admin/", nil))
	if root.Code != http.StatusOK || !strings.Contains(root.Body.String(), `<div id="root"></div>`) {
		t.Fatalf("admin index: status=%d body=%q", root.Code, root.Body.String())
	}
	if root.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("admin index must not be cached")
	}
	for _, route := range []string{"accounts", "import", "logs", "model-visibility", "providers", "settings", "settings/general", "settings/api-keys", "settings/security", "settings/models", "settings/upstreams", "settings/advanced", "about"} {
		page := httptest.NewRecorder()
		adminStaticHandler(page, httptest.NewRequest(http.MethodGet, "/admin/"+route, nil))
		if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `<div id="root"></div>`) {
			t.Fatalf("admin route %s: status=%d", route, page.Code)
		}
	}
	asset := regexp.MustCompile(`/admin/assets/[^" ]+\.js`).FindString(root.Body.String())
	if asset == "" {
		t.Fatal("admin script missing")
	}
	response := httptest.NewRecorder()
	adminStaticHandler(response, httptest.NewRequest(http.MethodGet, asset, nil))
	if response.Code != http.StatusOK || response.Body.Len() == 0 || response.Header().Get("Cache-Control") == "" {
		t.Fatalf("admin asset: status=%d bytes=%d", response.Code, response.Body.Len())
	}
	missing := httptest.NewRecorder()
	adminStaticHandler(missing, httptest.NewRequest(http.MethodGet, "/admin/unknown", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unknown admin path: %d", missing.Code)
	}
}
