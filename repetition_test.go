package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func repetitiveChatSSE(t *testing.T, count int) string {
	t.Helper()
	lines := make([]string, 0, count+2)
	for i := 0; i < count; i++ {
		chunk, err := json.Marshal(map[string]any{
			"model": "m1",
			"choices": []any{map[string]any{
				"delta": map[string]any{"content": "<total_tokens>15000000 tokens left</total_tokens> "},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, fmt.Sprintf("data: %s", chunk))
	}
	return strings.Join(append(lines, "data: [DONE]", ""), "\n")
}

func TestOutputRepetitionGuardIgnoresSSEChunkBoundaries(t *testing.T) {
	pattern := "<total_tokens>15000000 tokens left</total_tokens> "
	input := strings.Repeat(pattern, 12)
	guard := outputRepetitionGuard{}
	chunkSizes := []int{1, 7, 3, 19, 2, 31, 5, 11}
	detected := false
	for offset, chunkIndex := 0, 0; offset < len(input); chunkIndex++ {
		size := chunkSizes[chunkIndex%len(chunkSizes)]
		end := min(len(input), offset+size)
		if guard.Observe(input[offset:end]) {
			detected = true
			break
		}
		offset = end
	}
	if !detected {
		t.Fatal("repetition was not detected across arbitrary stream chunks")
	}
}

func TestOutputRepetitionGuardAllowsLongNonPeriodicText(t *testing.T) {
	guard := outputRepetitionGuard{}
	for i := 0; i < 200; i++ {
		fragment := strings.Repeat(" ", i%5) + "line " + string(rune('A'+i%26)) + " contains changing prose and a counter"
		encoded, _ := json.Marshal(i)
		if guard.Observe(fragment + string(encoded) + "\n") {
			t.Fatalf("ordinary output was rejected at fragment %d", i)
		}
	}
}
