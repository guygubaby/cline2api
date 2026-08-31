package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestCodexModelCatalogIncludesSupportedModels(t *testing.T) {
	data, err := os.ReadFile("codex-models.json")
	if err != nil {
		t.Fatalf("read Codex model catalog: %v", err)
	}
	var catalog struct {
		Models []struct {
			Slug                       string `json:"slug"`
			BaseInstructions           string `json:"base_instructions"`
			ContextWindow              int    `json:"context_window"`
			SupportsReasoningSummaries bool   `json:"supports_reasoning_summaries"`
			ApplyPatchToolType         string `json:"apply_patch_tool_type"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &catalog); err != nil {
		t.Fatalf("decode Codex model catalog: %v", err)
	}

	models := map[string]bool{}
	for _, model := range catalog.Models {
		models[model.Slug] = true
		if model.BaseInstructions == "" || model.ContextWindow != 1_000_000 || !model.SupportsReasoningSummaries {
			t.Fatalf("incomplete Codex metadata for %s: %#v", model.Slug, model)
		}
		if model.ApplyPatchToolType != "freeform" {
			t.Fatalf("apply_patch metadata for %s = %q", model.Slug, model.ApplyPatchToolType)
		}
	}
	for _, modelID := range []string{"deepseek/deepseek-v4-flash", "z-ai/glm-5.3-flash"} {
		if !models[modelID] {
			t.Fatalf("Codex model catalog is missing %s", modelID)
		}
	}
}

func TestCodexModelCatalogEndpoint(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/codex-models.json", nil)
	handleCodexModels(recorder, request)

	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("catalog endpoint status=%d headers=%v", recorder.Code, recorder.Header())
	}
	var catalog map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &catalog); err != nil {
		t.Fatalf("catalog endpoint returned invalid JSON: %v", err)
	}
	if models, _ := catalog["models"].([]any); len(models) != 2 {
		t.Fatalf("catalog endpoint models = %#v", catalog["models"])
	}
}
