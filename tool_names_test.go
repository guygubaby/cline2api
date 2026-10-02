package main

import (
	"strings"
	"testing"
)

func TestClampAndRestoreLongToolNames(t *testing.T) {
	original := "namespace_" + strings.Repeat("long_tool_", 8)
	definition := map[string]any{"name": original}
	history := map[string]any{"name": original, "arguments": "{}"}
	choice := map[string]any{"name": original}
	params := map[string]any{
		"tools": []any{map[string]any{"type": "function", "function": definition}},
		"messages": []any{map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"function": history},
		}}},
		"tool_choice": map[string]any{"type": "function", "function": choice},
	}

	names := clampParamsToolNames(params)
	short, _ := definition["name"].(string)
	if len(short) > maxUpstreamToolNameLength || names[short] != original {
		t.Fatalf("clamped name = %q, mapping = %#v", short, names)
	}
	if history["name"] != short || choice["name"] != short {
		t.Fatalf("request names were not clamped consistently")
	}

	function := map[string]any{"name": short, "arguments": "{}"}
	response := map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{"tool_calls": []any{map[string]any{"function": function}}},
	}}}
	restoreToolCallsInResponse(response, names)
	if function["name"] != original {
		t.Fatalf("restored name = %q, want %q", function["name"], original)
	}
}
