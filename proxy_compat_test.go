package main

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
)

func TestBuildUpstreamBodyDisablesThinkingForSmallAuxiliaryCompletion(t *testing.T) {
	body := buildUpstreamBody(map[string]any{
		"model":      "deepseek/deepseek-v4-flash",
		"max_tokens": float64(64),
		"messages": []any{
			map[string]any{"role": "user", "content": "Return one short title."},
		},
	}, false)

	thinking, _ := body["thinking"].(map[string]any)
	if thinking["type"] != "disabled" {
		t.Fatalf("small auxiliary completion should disable thinking: %#v", body)
	}
	if _, ok := body["reasoning_effort"]; ok {
		t.Fatalf("unspecified reasoning_effort should not be injected: %#v", body)
	}
}

func TestBuildUpstreamBodyPreservesExplicitReasoning(t *testing.T) {
	body := buildUpstreamBody(map[string]any{
		"model":            "deepseek/deepseek-v4-flash",
		"max_tokens":       float64(64),
		"reasoning_effort": "high",
		"messages":         []any{map[string]any{"role": "user", "content": "Think carefully."}},
	}, false)

	if body["reasoning_effort"] != "high" {
		t.Fatalf("explicit reasoning_effort was not preserved: %#v", body)
	}
	if _, ok := body["thinking"]; ok {
		t.Fatalf("explicit reasoning request should not be disabled: %#v", body)
	}
}

func TestLoadOverrideContentKeepsEmptyFileSilent(t *testing.T) {
	tempDir := t.TempDir()
	if err := os.WriteFile(tempDir+"/override.md", nil, 0600); err != nil {
		t.Fatalf("write override: %v", err)
	}
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("change working directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(originalDir) })

	var logs bytes.Buffer
	originalWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(originalWriter) })

	if content := loadOverrideContent(); content != "" {
		t.Fatalf("empty override content = %q", content)
	}
	if strings.Contains(logs.String(), "override.md is empty") {
		t.Fatalf("empty optional override should be silent: %s", logs.String())
	}
}
