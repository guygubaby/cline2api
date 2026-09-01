package main

import (
	"encoding/json"
	"testing"
	"time"
)

func withTestPool(t *testing.T, testPool *AccountPool) {
	t.Helper()
	poolMu.Lock()
	oldPool := pool
	pool = testPool
	poolMu.Unlock()
	t.Cleanup(func() {
		poolMu.Lock()
		pool = oldPool
		poolMu.Unlock()
	})
}

func TestAnthropicThinkingDisabledRemainsDisabled(t *testing.T) {
	withTestPool(t, &AccountPool{AnthropicEffort: "low"})
	request := anthropicReq{
		Model:    "model-1",
		Thinking: json.RawMessage(`{"type":"disabled"}`),
	}

	converted := anthropicToOpenAI(request)
	if converted["reasoning_effort"] != nil {
		t.Fatalf("disabled thinking enabled reasoning effort: %#v", converted)
	}
	thinking, _ := converted["thinking"].(map[string]any)
	if thinking["type"] != "disabled" {
		t.Fatalf("thinking = %#v", converted["thinking"])
	}
}

func TestAnthropicThinkingUsesConfiguredEffortUnlessExplicit(t *testing.T) {
	withTestPool(t, &AccountPool{AnthropicEffort: "low"})
	adaptive := anthropicToOpenAI(anthropicReq{
		Model:    "model-1",
		Thinking: json.RawMessage(`{"type":"adaptive"}`),
	})
	if adaptive["reasoning_effort"] != "low" {
		t.Fatalf("configured effort = %#v", adaptive["reasoning_effort"])
	}

	explicit := anthropicToOpenAI(anthropicReq{
		Model:        "model-1",
		Thinking:     json.RawMessage(`{"type":"enabled","budget_tokens":1024}`),
		OutputConfig: json.RawMessage(`{"effort":"medium"}`),
	})
	if explicit["reasoning_effort"] != "medium" {
		t.Fatalf("explicit effort = %#v", explicit["reasoning_effort"])
	}
}

func TestLatencyEWMAAndSlowChannelDetection(t *testing.T) {
	stat := &ModelLatencyStat{}
	observeModelLatency(stat, time.Second)
	observeModelLatency(stat, 5*time.Second)
	if stat.Samples != 2 || stat.EWMAms != 2_000 {
		t.Fatalf("latency stat = %#v", stat)
	}
	if !anomalouslySlowLatency(12*time.Second, 2_000) {
		t.Fatal("12s channel was not classified as slow against a 2s alternative")
	}
	if anomalouslySlowLatency(8*time.Second, 2_000) {
		t.Fatal("sub-threshold channel was classified as slow")
	}
}

func TestFastestAccountExploresUnknownThenUsesLowestLatency(t *testing.T) {
	knownSlow := &Account{ModelLatencies: map[string]*ModelLatencyStat{
		"model-1": {EWMAms: 8_000, Samples: 3},
	}}
	unknown := &Account{}
	knownFast := &Account{ModelLatencies: map[string]*ModelLatencyStat{
		"model-1": {EWMAms: 1_500, Samples: 3},
	}}
	if got := fastestAccountForModel([]*Account{knownSlow, unknown, knownFast}, "model-1"); got != unknown {
		t.Fatalf("unmeasured account was not explored: %#v", got)
	}
	unknown.ModelLatencies = map[string]*ModelLatencyStat{"model-1": {EWMAms: 3_000, Samples: 1}}
	if got := fastestAccountForModel([]*Account{knownSlow, unknown, knownFast}, "model-1"); got != knownFast {
		t.Fatalf("fastest account was not selected: %#v", got)
	}
}

func TestRequestLogReadsMeasuredUpstreamTTFT(t *testing.T) {
	entry := RequestLog{}
	setRequestLogIsolationMetadata(&entry, map[string]any{proxyUpstreamTTFTParamKey: int64(1_234)})
	if entry.UpstreamTTFTMs != 1_234 {
		t.Fatalf("upstream TTFT = %d", entry.UpstreamTTFTMs)
	}
}

func TestLeastLatencyIsAValidPersistedStrategy(t *testing.T) {
	if !validLoadBalancingStrategy("least_latency") {
		t.Fatal("least_latency strategy was rejected")
	}
	if validLoadBalancingStrategy("fastest_magic") {
		t.Fatal("unknown strategy was accepted")
	}
}
