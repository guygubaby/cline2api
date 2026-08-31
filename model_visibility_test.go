package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFilterListedModelsUsesAllModelsUntilConfigured(t *testing.T) {
	models := []Model{{ID: "m1"}, {ID: "m2"}}
	listed := filterListedModels(models, false, nil)
	if len(listed) != 2 {
		t.Fatalf("unconfigured model list = %#v, want all models", listed)
	}
}

func TestFilterListedModelsSupportsPartialAndEmptySelections(t *testing.T) {
	models := []Model{{ID: "m1"}, {ID: "m2"}, {ID: "m3"}}
	partial := filterListedModels(models, true, []string{"m2"})
	if len(partial) != 1 || partial[0].ID != "m2" {
		t.Fatalf("partial model list = %#v, want m2", partial)
	}
	empty := filterListedModels(models, true, []string{})
	if len(empty) != 0 {
		t.Fatalf("configured empty model list = %#v, want empty", empty)
	}
}

func TestHandleAdminModelVisibilityPersistsCanonicalSelection(t *testing.T) {
	p := loadPool()
	poolMu.Lock()
	oldModels := p.Models
	oldConfigured := p.ModelListConfigured
	oldListedModelIDs := p.ListedModelIDs
	p.Models = []Model{
		{ID: "m1", Provider: "test", Status: "active", Source: "remote"},
		{ID: "m2", Provider: "test", Status: "active", Source: "remote"},
	}
	p.ModelListConfigured = false
	p.ListedModelIDs = nil
	poolMu.Unlock()
	t.Cleanup(func() {
		poolMu.Lock()
		p.Models = oldModels
		p.ModelListConfigured = oldConfigured
		p.ListedModelIDs = oldListedModelIDs
		poolMu.Unlock()
		savePool()
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/admin/api/models/visibility", strings.NewReader(`{
		"configured":true,
		"modelIds":["m2","m2"]
	}`))
	handleAdminModelVisibility(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	poolMu.Lock()
	configured := p.ModelListConfigured
	listedModelIDs := append([]string{}, p.ListedModelIDs...)
	poolMu.Unlock()
	if !configured || len(listedModelIDs) != 1 || listedModelIDs[0] != "m2" {
		t.Fatalf("stored visibility = configured:%v ids:%#v", configured, listedModelIDs)
	}
	listed := getListedModels()
	if len(listed) != 1 || listed[0].ID != "m2" {
		t.Fatalf("public model list = %#v, want m2", listed)
	}

	var response apiResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !response.Success {
		t.Fatalf("visibility response = %#v", response)
	}
}

func TestHandleAdminModelVisibilityRejectsUnknownModels(t *testing.T) {
	p := loadPool()
	poolMu.Lock()
	oldModels := p.Models
	oldConfigured := p.ModelListConfigured
	oldListedModelIDs := p.ListedModelIDs
	p.Models = []Model{{ID: "known", Provider: "test", Status: "active", Source: "remote"}}
	p.ModelListConfigured = false
	p.ListedModelIDs = nil
	poolMu.Unlock()
	t.Cleanup(func() {
		poolMu.Lock()
		p.Models = oldModels
		p.ModelListConfigured = oldConfigured
		p.ListedModelIDs = oldListedModelIDs
		poolMu.Unlock()
		savePool()
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/admin/api/models/visibility", strings.NewReader(`{
		"configured":true,
		"modelIds":["missing"]
	}`))
	handleAdminModelVisibility(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
	}
	poolMu.Lock()
	configured := p.ModelListConfigured
	poolMu.Unlock()
	if configured {
		t.Fatal("invalid model selection mutated the listing config")
	}
}
