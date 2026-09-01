package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDockerComposeProviderStorageDoesNotRequirePrecreatedFile(t *testing.T) {
	compose, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatalf("read docker-compose.yml: %v", err)
	}
	configuration := string(compose)
	if strings.Contains(configuration, "source: ./.cline-providers.json") {
		t.Fatal("custom provider storage uses a file bind that fails when the runtime-created file does not exist")
	}
	for _, expected := range []string{
		"target: /app/provider-data",
		"CLINE_PROVIDERS_PATH=/app/provider-data/.cline-providers.json",
	} {
		if !strings.Contains(configuration, expected) {
			t.Fatalf("docker compose provider storage missing %q", expected)
		}
	}
}

func TestConfiguredCustomProvidersPathUsesEnvironment(t *testing.T) {
	want := filepath.Join(t.TempDir(), "providers.json")
	t.Setenv(customProvidersPathEnv, want)
	if got := configuredCustomProvidersPath(); got != want {
		t.Fatalf("configured path = %q, want %q", got, want)
	}
}

func isolateCustomProviderState(t *testing.T) {
	t.Helper()
	customProviderMu.Lock()
	oldPath := customProvidersPath
	oldStore := customProviderStore
	oldHTTPClient := customProviderHTTPClient
	customProvidersPath = filepath.Join(t.TempDir(), ".cline-providers.json")
	customProviderStore = nil
	customProviderHTTPClient = &http.Client{Transport: &http.Transport{}}
	customProviderMu.Unlock()

	customProviderRuntimeMu.Lock()
	oldRuntime := customProviderRuntime
	oldCursors := customProviderCursors
	customProviderRuntime = map[string]*customProviderRuntimeState{}
	customProviderCursors = map[string]int{}
	customProviderRuntimeMu.Unlock()

	t.Cleanup(func() {
		customProviderMu.Lock()
		customProvidersPath = oldPath
		customProviderStore = oldStore
		customProviderHTTPClient = oldHTTPClient
		customProviderMu.Unlock()
		customProviderRuntimeMu.Lock()
		customProviderRuntime = oldRuntime
		customProviderCursors = oldCursors
		customProviderRuntimeMu.Unlock()
	})
}

func addTestCustomProvider(t *testing.T, provider CustomProvider, models []CustomProviderModel) CustomProvider {
	t.Helper()
	saved, err := upsertCustomProvider(provider)
	if err != nil {
		t.Fatalf("save provider: %v", err)
	}
	if err := updateCustomProviderModels(saved.ID, models); err != nil {
		t.Fatalf("save provider models: %v", err)
	}
	saved.Models = models
	return saved
}

func TestSyncOpenAIProviderModelsPreservesMappings(t *testing.T) {
	isolateCustomProviderState(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("authorization = %q", got)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"object": "list",
			"data": []any{
				map[string]any{"id": "model-a", "owned_by": "test"},
				map[string]any{"id": "model-b", "owned_by": "test"},
			},
		})
	}))
	defer server.Close()

	provider := addTestCustomProvider(t, CustomProvider{
		Name: "test-openai", Protocol: customProviderProtocolOpenAI,
		BaseURL: server.URL + "/v1", APIKey: "secret", Enabled: true,
	}, []CustomProviderModel{{ID: "model-a", PublicID: "public-a", Enabled: false}})

	result, err := syncCustomProviderModels(context.Background(), provider.ID)
	if err != nil {
		t.Fatalf("sync models: %v", err)
	}
	if result.Total != 2 || len(result.Added) != 1 || result.Added[0] != "model-b" {
		t.Fatalf("sync result = %#v", result)
	}
	updated, ok := findCustomProvider(provider.ID)
	if !ok || len(updated.Models) != 2 {
		t.Fatalf("updated provider = %#v, %v", updated, ok)
	}
	if updated.Models[0].PublicID != "public-a" || updated.Models[0].Enabled {
		t.Fatalf("existing mapping was not preserved: %#v", updated.Models[0])
	}
	if updated.Models[1].PublicID != "model-b" || !updated.Models[1].Enabled {
		t.Fatalf("new mapping defaults = %#v", updated.Models[1])
	}
}

func TestCustomProviderRoundRobinAndRetry(t *testing.T) {
	isolateCustomProviderState(t)
	var firstCalls atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "limited"})
	}))
	defer first.Close()
	var secondCalls atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if request["model"] != "upstream-two" {
			t.Fatalf("upstream model = %#v", request["model"])
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "chat-1", "model": "upstream-two",
			"choices": []any{map[string]any{
				"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop",
			}},
		})
	}))
	defer second.Close()

	addTestCustomProvider(t, CustomProvider{
		Name: "first", Protocol: customProviderProtocolOpenAI,
		BaseURL: first.URL, APIKey: "key-1", Enabled: true,
	}, []CustomProviderModel{{ID: "upstream-one", PublicID: "shared-model", Enabled: true}})
	addTestCustomProvider(t, CustomProvider{
		Name: "second", Protocol: customProviderProtocolOpenAI,
		BaseURL: second.URL, APIKey: "key-2", Enabled: true,
	}, []CustomProviderModel{{ID: "upstream-two", PublicID: "shared-model", Enabled: true}})

	response, provider, retries, err := callCustomProviderChat(context.Background(), map[string]any{
		"model": "shared-model", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, false)
	if err != nil {
		t.Fatalf("call provider: %v", err)
	}
	defer response.Body.Close()
	if provider == nil || provider.Name != "second" || retries != 1 {
		t.Fatalf("provider=%#v retries=%d", provider, retries)
	}
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["model"] != "shared-model" {
		t.Fatalf("public model = %#v", body["model"])
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Fatalf("calls first=%d second=%d", firstCalls.Load(), secondCalls.Load())
	}
}

func TestCustomProviderCatalogAggregatesChannelsAndMasksKeys(t *testing.T) {
	isolateCustomProviderState(t)
	for _, name := range []string{"channel-one", "channel-two"} {
		addTestCustomProvider(t, CustomProvider{
			Name: name, Protocol: customProviderProtocolOpenAI,
			BaseURL: "https://example.com/v1", APIKey: "top-secret-1234", Enabled: true,
		}, []CustomProviderModel{{ID: name + "-model", PublicID: "shared-public", Enabled: true}})
	}
	models := customProviderCatalogModels()
	if len(models) != 1 || models[0].ID != "shared-public" || models[0].ChannelCount != 2 {
		t.Fatalf("catalog = %#v", models)
	}
	merged := mergeModelCatalogs([]Model{{ID: "shared-public", Provider: "cline", Source: "remote"}}, models)
	if len(merged) != 1 || merged[0].Source != "custom_provider" || merged[0].ChannelCount != 2 {
		t.Fatalf("merged catalog = %#v", merged)
	}
	encoded, err := json.Marshal(customProviderAdminData())
	if err != nil {
		t.Fatalf("encode admin data: %v", err)
	}
	if strings.Contains(string(encoded), "top-secret-1234") || !strings.Contains(string(encoded), "••••1234") {
		t.Fatalf("API key masking failed: %s", encoded)
	}
}

func TestAnthropicProviderAdapterNormalizesNonStreamAndStream(t *testing.T) {
	isolateCustomProviderState(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "anthropic-secret" || r.Header.Get("anthropic-version") == "" {
			t.Fatalf("anthropic headers missing: %#v", r.Header)
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if request["model"] != "claude-upstream" {
			t.Fatalf("model = %#v", request["model"])
		}
		stream, _ := request["stream"].(bool)
		if !stream {
			writeJSON(w, http.StatusOK, map[string]any{
				"id": "msg_1", "type": "message", "model": "claude-upstream", "stop_reason": "end_turn",
				"content": []any{
					map[string]any{"type": "thinking", "thinking": "reason"},
					map[string]any{"type": "text", "text": "answer"},
				},
				"usage": map[string]any{"input_tokens": 10, "output_tokens": 4},
			})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_2\",\"model\":\"claude-upstream\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n")
		io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"streamed\"}}\n\n")
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n")
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()

	addTestCustomProvider(t, CustomProvider{
		Name: "anthropic", Protocol: customProviderProtocolAnthropic,
		BaseURL: server.URL, APIKey: "anthropic-secret", Enabled: true,
	}, []CustomProviderModel{{ID: "claude-upstream", PublicID: "claude-public", Enabled: true}})
	params := map[string]any{
		"model": "claude-public", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}

	response, _, _, err := callCustomProviderChat(context.Background(), params, false)
	if err != nil {
		t.Fatalf("non-stream call: %v", err)
	}
	var nonStream map[string]any
	if err := json.NewDecoder(response.Body).Decode(&nonStream); err != nil {
		t.Fatalf("decode non-stream: %v", err)
	}
	response.Body.Close()
	if nonStream["model"] != "claude-public" || getNested(nonStream, "choices", 0, "message", "content") != "answer" {
		t.Fatalf("normalized non-stream = %#v", nonStream)
	}
	if getNested(nonStream, "choices", 0, "message", "reasoning_content") != "reason" {
		t.Fatalf("reasoning missing: %#v", nonStream)
	}

	streamResponse, _, _, err := callCustomProviderChat(context.Background(), params, true)
	if err != nil {
		t.Fatalf("stream call: %v", err)
	}
	streamBody, err := io.ReadAll(streamResponse.Body)
	streamResponse.Body.Close()
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	streamText := string(streamBody)
	for _, expected := range []string{"streamed", "claude-public", "\"finish_reason\":\"stop\"", "data: [DONE]"} {
		if !strings.Contains(streamText, expected) {
			t.Fatalf("stream missing %q: %s", expected, streamText)
		}
	}
}
