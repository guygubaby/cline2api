package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	customProviderProtocolOpenAI    = "openai"
	customProviderProtocolAnthropic = "anthropic"
	customProviderMaxAttempts       = 2
	customProviderRequestTimeout    = 15 * time.Second
	customProvidersPathEnv          = "CLINE_PROVIDERS_PATH"
)

// CustomProviderModel maps one upstream model to the public model ID exposed
// by cline2api. Multiple providers may map to the same PublicID, forming that
// model's load-balancing pool.
type CustomProviderModel struct {
	ID          string `json:"id"`
	PublicID    string `json:"publicId"`
	DisplayName string `json:"displayName,omitempty"`
	Context     int    `json:"context,omitempty"`
	Output      int    `json:"output,omitempty"`
	Enabled     bool   `json:"enabled"`
}

type CustomProvider struct {
	ID        string                `json:"id"`
	Name      string                `json:"name"`
	Protocol  string                `json:"protocol"`
	BaseURL   string                `json:"baseURL"`
	APIKey    string                `json:"apiKey"`
	Enabled   bool                  `json:"enabled"`
	Headers   map[string]string     `json:"headers,omitempty"`
	Models    []CustomProviderModel `json:"models,omitempty"`
	CreatedAt time.Time             `json:"createdAt"`
	UpdatedAt time.Time             `json:"updatedAt"`
}

type customProviderStoreData struct {
	Strategy  string           `json:"strategy"`
	Providers []CustomProvider `json:"providers"`
}

type discoveredProviderModel struct {
	ID          string
	DisplayName string
	Context     int
	Output      int
}

type customProviderAdapter interface {
	ListModels(context.Context, CustomProvider) ([]discoveredProviderModel, error)
	Chat(context.Context, CustomProvider, map[string]any, bool) (*http.Response, error)
}

type openAIProviderAdapter struct{}
type anthropicProviderAdapter struct{}

type customProviderRuntimeState struct {
	LastSuccess time.Time
	LastError   string
	Cooldowns   map[string]time.Time
}

type customProviderRoute struct {
	Provider CustomProvider
	Model    CustomProviderModel
}

var (
	customProvidersPath      string
	customProviderStore      *customProviderStoreData
	customProviderMu         sync.RWMutex
	customProviderSaveMu     sync.Mutex
	customProviderHTTPClient = httpClient

	customProviderRuntimeMu sync.Mutex
	customProviderRuntime   = map[string]*customProviderRuntimeState{}
	customProviderCursors   = map[string]int{}
)

func init() {
	customProvidersPath = configuredCustomProvidersPath()
}

func configuredCustomProvidersPath() string {
	if configured := strings.TrimSpace(os.Getenv(customProvidersPathEnv)); configured != "" {
		return configured
	}
	return resolveDataPath(".cline-providers.json")
}

func defaultCustomProviderStore() *customProviderStoreData {
	return &customProviderStoreData{Strategy: "round_robin", Providers: []CustomProvider{}}
}

func normalizeCustomProviderStore(store *customProviderStoreData) {
	if store.Strategy != "random" && store.Strategy != "fill" {
		store.Strategy = "round_robin"
	}
	if store.Providers == nil {
		store.Providers = []CustomProvider{}
	}
	for providerIndex := range store.Providers {
		provider := &store.Providers[providerIndex]
		provider.BaseURL = strings.TrimRight(strings.TrimSpace(provider.BaseURL), "/")
		if provider.Headers == nil {
			provider.Headers = map[string]string{}
		}
		for modelIndex := range provider.Models {
			model := &provider.Models[modelIndex]
			model.ID = strings.TrimSpace(model.ID)
			model.PublicID = strings.TrimSpace(model.PublicID)
			if model.PublicID == "" {
				model.PublicID = model.ID
			}
		}
	}
}

func loadCustomProviderStore() *customProviderStoreData {
	customProviderMu.Lock()
	defer customProviderMu.Unlock()
	if customProviderStore != nil {
		return customProviderStore
	}
	store := defaultCustomProviderStore()
	if data, err := os.ReadFile(customProvidersPath); err == nil {
		if err := json.Unmarshal(data, store); err != nil {
			log.Printf("custom providers parse failed: %v", err)
		}
	}
	normalizeCustomProviderStore(store)
	customProviderStore = store
	return customProviderStore
}

func saveCustomProviderStoreLocked() error {
	customProviderSaveMu.Lock()
	defer customProviderSaveMu.Unlock()
	data, err := json.MarshalIndent(customProviderStore, "", "  ")
	if err != nil {
		return fmt.Errorf("encode custom providers: %w", err)
	}
	if err := writeFileDurably(customProvidersPath, data, 0600); err != nil {
		return fmt.Errorf("save custom providers: %w", err)
	}
	return nil
}

func cloneCustomProvider(provider CustomProvider) CustomProvider {
	clone := provider
	clone.Headers = make(map[string]string, len(provider.Headers))
	for key, value := range provider.Headers {
		clone.Headers[key] = value
	}
	clone.Models = append([]CustomProviderModel(nil), provider.Models...)
	return clone
}

func snapshotCustomProviders() (string, []CustomProvider) {
	loadCustomProviderStore()
	customProviderMu.RLock()
	defer customProviderMu.RUnlock()
	providers := make([]CustomProvider, 0, len(customProviderStore.Providers))
	for _, provider := range customProviderStore.Providers {
		providers = append(providers, cloneCustomProvider(provider))
	}
	return customProviderStore.Strategy, providers
}

func validateCustomProvider(provider CustomProvider, requireKey bool) error {
	provider.Name = strings.TrimSpace(provider.Name)
	if provider.Name == "" {
		return fmt.Errorf("provider name is required")
	}
	if provider.Protocol != customProviderProtocolOpenAI && provider.Protocol != customProviderProtocolAnthropic {
		return fmt.Errorf("unsupported provider protocol %q", provider.Protocol)
	}
	parsed, err := url.Parse(strings.TrimSpace(provider.BaseURL))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("base URL must be an absolute http or https URL")
	}
	if requireKey && strings.TrimSpace(provider.APIKey) == "" {
		return fmt.Errorf("API key is required")
	}
	for key, value := range provider.Headers {
		if strings.TrimSpace(key) == "" || strings.ContainsAny(key, "\r\n") || strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("invalid custom header")
		}
	}
	return nil
}

func upsertCustomProvider(input CustomProvider) (CustomProvider, error) {
	headersProvided := input.Headers != nil
	input.Name = strings.TrimSpace(input.Name)
	input.BaseURL = strings.TrimRight(strings.TrimSpace(input.BaseURL), "/")
	input.APIKey = strings.TrimSpace(input.APIKey)
	if input.Headers == nil {
		input.Headers = map[string]string{}
	}

	loadCustomProviderStore()
	customProviderMu.Lock()
	defer customProviderMu.Unlock()

	now := time.Now()
	if input.ID == "" {
		if err := validateCustomProvider(input, true); err != nil {
			return CustomProvider{}, err
		}
		input.ID = "provider_" + secureRandomHex(8)
		input.CreatedAt = now
		input.UpdatedAt = now
		input.Models = []CustomProviderModel{}
		customProviderStore.Providers = append(customProviderStore.Providers, input)
		if err := saveCustomProviderStoreLocked(); err != nil {
			return CustomProvider{}, err
		}
		return cloneCustomProvider(input), nil
	}

	for index := range customProviderStore.Providers {
		existing := &customProviderStore.Providers[index]
		if existing.ID != input.ID {
			continue
		}
		if input.APIKey == "" {
			input.APIKey = existing.APIKey
		}
		if !headersProvided {
			input.Headers = cloneCustomProvider(*existing).Headers
		}
		if err := validateCustomProvider(input, true); err != nil {
			return CustomProvider{}, err
		}
		input.CreatedAt = existing.CreatedAt
		input.UpdatedAt = now
		input.Models = append([]CustomProviderModel(nil), existing.Models...)
		*existing = input
		if err := saveCustomProviderStoreLocked(); err != nil {
			return CustomProvider{}, err
		}
		return cloneCustomProvider(input), nil
	}
	return CustomProvider{}, fmt.Errorf("provider not found")
}

func deleteCustomProvider(providerID string) error {
	loadCustomProviderStore()
	customProviderMu.Lock()
	defer customProviderMu.Unlock()
	for index, provider := range customProviderStore.Providers {
		if provider.ID != providerID {
			continue
		}
		customProviderStore.Providers = append(customProviderStore.Providers[:index], customProviderStore.Providers[index+1:]...)
		if err := saveCustomProviderStoreLocked(); err != nil {
			return err
		}
		customProviderRuntimeMu.Lock()
		delete(customProviderRuntime, providerID)
		customProviderRuntimeMu.Unlock()
		return nil
	}
	return fmt.Errorf("provider not found")
}

func setCustomProviderStrategy(strategy string) error {
	if strategy != "round_robin" && strategy != "random" && strategy != "fill" {
		return fmt.Errorf("invalid provider strategy")
	}
	loadCustomProviderStore()
	customProviderMu.Lock()
	defer customProviderMu.Unlock()
	customProviderStore.Strategy = strategy
	return saveCustomProviderStoreLocked()
}

func updateCustomProviderModels(providerID string, models []CustomProviderModel) error {
	seen := make(map[string]struct{}, len(models))
	cleaned := make([]CustomProviderModel, 0, len(models))
	for _, model := range models {
		model.ID = strings.TrimSpace(model.ID)
		model.PublicID = strings.TrimSpace(model.PublicID)
		if model.ID == "" || model.PublicID == "" {
			return fmt.Errorf("upstream and public model IDs are required")
		}
		if _, duplicate := seen[model.ID]; duplicate {
			return fmt.Errorf("duplicate upstream model %q", model.ID)
		}
		seen[model.ID] = struct{}{}
		cleaned = append(cleaned, model)
	}

	loadCustomProviderStore()
	customProviderMu.Lock()
	defer customProviderMu.Unlock()
	for index := range customProviderStore.Providers {
		provider := &customProviderStore.Providers[index]
		if provider.ID != providerID {
			continue
		}
		provider.Models = cleaned
		provider.UpdatedAt = time.Now()
		return saveCustomProviderStoreLocked()
	}
	return fmt.Errorf("provider not found")
}

func customProviderAdapterFor(protocol string) (customProviderAdapter, error) {
	switch protocol {
	case customProviderProtocolOpenAI:
		return openAIProviderAdapter{}, nil
	case customProviderProtocolAnthropic:
		return anthropicProviderAdapter{}, nil
	default:
		return nil, fmt.Errorf("unsupported provider protocol %q", protocol)
	}
}

func findCustomProvider(providerID string) (CustomProvider, bool) {
	_, providers := snapshotCustomProviders()
	for _, provider := range providers {
		if provider.ID == providerID {
			return provider, true
		}
	}
	return CustomProvider{}, false
}

func syncCustomProviderModels(ctx context.Context, providerID string) (modelSyncResult, error) {
	provider, ok := findCustomProvider(providerID)
	if !ok {
		return modelSyncResult{}, fmt.Errorf("provider not found")
	}
	adapter, err := customProviderAdapterFor(provider.Protocol)
	if err != nil {
		return modelSyncResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, customProviderRequestTimeout)
	defer cancel()
	discovered, err := adapter.ListModels(ctx, provider)
	if err != nil {
		markCustomProviderFailure(provider.ID, "", err, time.Minute)
		return modelSyncResult{}, err
	}
	if len(discovered) == 0 {
		return modelSyncResult{}, fmt.Errorf("provider returned no models")
	}

	oldModels := make(map[string]CustomProviderModel, len(provider.Models))
	for _, model := range provider.Models {
		oldModels[model.ID] = model
	}
	seen := make(map[string]struct{}, len(discovered))
	models := make([]CustomProviderModel, 0, len(discovered))
	result := modelSyncResult{SyncedAt: time.Now().Format(time.RFC3339)}
	for _, item := range discovered {
		item.ID = strings.TrimSpace(item.ID)
		if item.ID == "" {
			continue
		}
		if _, duplicate := seen[item.ID]; duplicate {
			continue
		}
		seen[item.ID] = struct{}{}
		model := CustomProviderModel{
			ID: item.ID, PublicID: item.ID, DisplayName: item.DisplayName,
			Context: item.Context, Output: item.Output, Enabled: true,
		}
		if old, exists := oldModels[item.ID]; exists {
			model.PublicID = old.PublicID
			model.Enabled = old.Enabled
			if model.DisplayName == "" {
				model.DisplayName = old.DisplayName
			}
			if model.Context == 0 {
				model.Context = old.Context
			}
			if model.Output == 0 {
				model.Output = old.Output
			}
		} else {
			result.Added = append(result.Added, item.ID)
		}
		models = append(models, model)
	}
	for modelID := range oldModels {
		if _, exists := seen[modelID]; !exists {
			result.Removed = append(result.Removed, modelID)
		}
	}
	sort.Strings(result.Added)
	sort.Strings(result.Removed)
	result.Total = len(models)
	result.Changed = len(result.Added) > 0 || len(result.Removed) > 0
	if err := updateCustomProviderModels(providerID, models); err != nil {
		return modelSyncResult{}, err
	}
	markCustomProviderSuccess(provider.ID, "")
	return result, nil
}

func providerHeaders(provider CustomProvider, contentType bool) http.Header {
	headers := make(http.Header)
	if contentType {
		headers.Set("Content-Type", "application/json")
	}
	switch provider.Protocol {
	case customProviderProtocolAnthropic:
		headers.Set("x-api-key", provider.APIKey)
		headers.Set("anthropic-version", "2023-06-01")
	default:
		headers.Set("Authorization", "Bearer "+provider.APIKey)
	}
	for key, value := range provider.Headers {
		headers.Set(key, value)
	}
	return headers
}

func decodeDiscoveredModels(response *http.Response) ([]discoveredProviderModel, bool, string, error) {
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil, false, "", fmt.Errorf("decode models response: %w", err)
	}
	items, _ := payload["data"].([]any)
	models := make([]discoveredProviderModel, 0, len(items))
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item == nil {
			continue
		}
		id, _ := item["id"].(string)
		if id == "" {
			continue
		}
		displayName, _ := item["display_name"].(string)
		contextTokens := int(tokenCount(item["max_input_tokens"]))
		if contextTokens == 0 {
			contextTokens = int(tokenCount(item["context_window"]))
		}
		models = append(models, discoveredProviderModel{
			ID: id, DisplayName: displayName, Context: contextTokens,
			Output: int(tokenCount(item["max_tokens"])),
		})
	}
	hasMore, _ := payload["has_more"].(bool)
	lastID, _ := payload["last_id"].(string)
	return models, hasMore, lastID, nil
}

func (openAIProviderAdapter) ListModels(ctx context.Context, provider CustomProvider) ([]discoveredProviderModel, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, provider.BaseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	request.Header = providerHeaders(provider, false)
	response, err := customProviderHTTPClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, newUpstreamHTTPError(response.StatusCode, string(readAllLimited(response.Body, 64<<10)))
	}
	models, _, _, err := decodeDiscoveredModels(response)
	return models, err
}

func (anthropicProviderAdapter) ListModels(ctx context.Context, provider CustomProvider) ([]discoveredProviderModel, error) {
	models := []discoveredProviderModel{}
	afterID := ""
	for page := 0; page < 20; page++ {
		endpoint := provider.BaseURL + "/models?limit=1000"
		if afterID != "" {
			endpoint += "&after_id=" + url.QueryEscape(afterID)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		request.Header = providerHeaders(provider, false)
		response, err := customProviderHTTPClient.Do(request)
		if err != nil {
			return nil, fmt.Errorf("list models: %w", err)
		}
		if response.StatusCode != http.StatusOK {
			body := readAllLimited(response.Body, 64<<10)
			response.Body.Close()
			return nil, newUpstreamHTTPError(response.StatusCode, string(body))
		}
		pageModels, hasMore, lastID, decodeErr := decodeDiscoveredModels(response)
		response.Body.Close()
		if decodeErr != nil {
			return nil, decodeErr
		}
		models = append(models, pageModels...)
		if !hasMore || lastID == "" || lastID == afterID {
			break
		}
		afterID = lastID
	}
	return models, nil
}

func (openAIProviderAdapter) Chat(ctx context.Context, provider CustomProvider, params map[string]any, stream bool) (*http.Response, error) {
	body := make(map[string]any, len(params)+1)
	for key, value := range params {
		if !strings.HasPrefix(key, "_proxy_") {
			body[key] = value
		}
	}
	body["stream"] = stream
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode OpenAI request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.BaseURL+"/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	request.Header = providerHeaders(provider, true)
	response, err := customProviderHTTPClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("provider request: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		body := readAllLimited(response.Body, 64<<10)
		response.Body.Close()
		return nil, newUpstreamHTTPError(response.StatusCode, string(body))
	}
	return response, nil
}

func (anthropicProviderAdapter) Chat(ctx context.Context, provider CustomProvider, params map[string]any, stream bool) (*http.Response, error) {
	body := openAIChatToAnthropicRequest(params, stream)
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode Anthropic request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.BaseURL+"/messages", bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	request.Header = providerHeaders(provider, true)
	response, err := customProviderHTTPClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("provider request: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		responseBody := readAllLimited(response.Body, 64<<10)
		response.Body.Close()
		return nil, newUpstreamHTTPError(response.StatusCode, string(responseBody))
	}
	if stream {
		return anthropicStreamToOpenAIResponse(response), nil
	}
	defer response.Body.Close()
	var anthropicResponse map[string]any
	if err := json.NewDecoder(response.Body).Decode(&anthropicResponse); err != nil {
		return nil, fmt.Errorf("decode Anthropic response: %w", err)
	}
	openAIResponse := anthropicResponseToOpenAI(anthropicResponse)
	responseBytes, err := json.Marshal(openAIResponse)
	if err != nil {
		return nil, err
	}
	response.Body = io.NopCloser(bytes.NewReader(responseBytes))
	response.ContentLength = int64(len(responseBytes))
	response.Header.Set("Content-Type", "application/json")
	return response, nil
}

func customProviderCatalogModels() []Model {
	_, providers := snapshotCustomProviders()
	byID := map[string]Model{}
	order := []string{}
	for _, provider := range providers {
		if !provider.Enabled {
			continue
		}
		for _, providerModel := range provider.Models {
			if !providerModel.Enabled || providerModel.PublicID == "" {
				continue
			}
			model, exists := byID[providerModel.PublicID]
			if !exists {
				model = Model{
					ID: providerModel.PublicID, Provider: provider.Name, Cost: "pass",
					Status: "active", Source: "custom_provider", Context: providerModel.Context,
					Output: providerModel.Output,
				}
				order = append(order, providerModel.PublicID)
			}
			model.ChannelCount++
			if model.Context == 0 {
				model.Context = providerModel.Context
			}
			if model.Output == 0 {
				model.Output = providerModel.Output
			}
			if model.ChannelCount > 1 {
				model.Provider = "multi-channel"
			}
			byID[providerModel.PublicID] = model
		}
	}
	models := make([]Model, 0, len(order))
	for _, modelID := range order {
		models = append(models, byID[modelID])
	}
	return models
}

func customProviderHasModel(publicModelID string) bool {
	_, providers := snapshotCustomProviders()
	for _, provider := range providers {
		if !provider.Enabled {
			continue
		}
		for _, model := range provider.Models {
			if model.Enabled && model.PublicID == publicModelID {
				return true
			}
		}
	}
	return false
}

func customProviderModelCooling(providerID, upstreamModelID string, now time.Time) bool {
	customProviderRuntimeMu.Lock()
	defer customProviderRuntimeMu.Unlock()
	state := customProviderRuntime[providerID]
	if state == nil || state.Cooldowns == nil {
		return false
	}
	until, cooling := state.Cooldowns[upstreamModelID]
	if !cooling {
		return false
	}
	if now.After(until) {
		delete(state.Cooldowns, upstreamModelID)
		return false
	}
	return true
}

func customProviderRoutesForModel(publicModelID string) []customProviderRoute {
	strategy, providers := snapshotCustomProviders()
	now := time.Now()
	routes := []customProviderRoute{}
	for _, provider := range providers {
		if !provider.Enabled {
			continue
		}
		for _, model := range provider.Models {
			if !model.Enabled || model.PublicID != publicModelID || customProviderModelCooling(provider.ID, model.ID, now) {
				continue
			}
			routes = append(routes, customProviderRoute{Provider: provider, Model: model})
		}
	}
	if len(routes) < 2 || strategy == "fill" {
		return routes
	}
	start := 0
	if strategy == "random" {
		start = randIntn(len(routes))
	} else {
		customProviderRuntimeMu.Lock()
		start = customProviderCursors[publicModelID] % len(routes)
		customProviderCursors[publicModelID] = (start + 1) % len(routes)
		customProviderRuntimeMu.Unlock()
	}
	ordered := make([]customProviderRoute, 0, len(routes))
	ordered = append(ordered, routes[start:]...)
	ordered = append(ordered, routes[:start]...)
	return ordered
}

func customProviderStateLocked(providerID string) *customProviderRuntimeState {
	state := customProviderRuntime[providerID]
	if state == nil {
		state = &customProviderRuntimeState{Cooldowns: map[string]time.Time{}}
		customProviderRuntime[providerID] = state
	}
	if state.Cooldowns == nil {
		state.Cooldowns = map[string]time.Time{}
	}
	return state
}

func markCustomProviderFailure(providerID, upstreamModelID string, err error, cooldown time.Duration) {
	customProviderRuntimeMu.Lock()
	defer customProviderRuntimeMu.Unlock()
	state := customProviderStateLocked(providerID)
	state.LastError = truncate(err.Error(), 300)
	if upstreamModelID != "" && cooldown > 0 {
		state.Cooldowns[upstreamModelID] = time.Now().Add(cooldown)
	}
}

func markCustomProviderSuccess(providerID, upstreamModelID string) {
	customProviderRuntimeMu.Lock()
	defer customProviderRuntimeMu.Unlock()
	state := customProviderStateLocked(providerID)
	state.LastSuccess = time.Now()
	state.LastError = ""
	if upstreamModelID != "" {
		delete(state.Cooldowns, upstreamModelID)
	}
}

func customProviderFailureCooldown(err error) time.Duration {
	status := upstreamErrorStatus(err)
	switch {
	case status == http.StatusTooManyRequests:
		return time.Minute
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return 5 * time.Minute
	case status >= http.StatusInternalServerError || status == 0:
		return 30 * time.Second
	default:
		return 0
	}
}

func customProviderRetryable(err error) bool {
	status := upstreamErrorStatus(err)
	return status == 0 || status == http.StatusTooManyRequests || status == http.StatusUnauthorized ||
		status == http.StatusForbidden || status >= http.StatusInternalServerError
}

func customProviderUpstreamLabel(provider *CustomProvider) string {
	if provider == nil {
		return "custom"
	}
	return "custom:" + provider.Name
}

func customProviderKeyPreview(apiKey string) string {
	if apiKey == "" {
		return ""
	}
	if len(apiKey) <= 4 {
		return "••••"
	}
	return "••••" + apiKey[len(apiKey)-4:]
}

func customProviderAdminData() map[string]any {
	strategy, providers := snapshotCustomProviders()
	items := make([]any, 0, len(providers))
	customProviderRuntimeMu.Lock()
	defer customProviderRuntimeMu.Unlock()
	for _, provider := range providers {
		state := customProviderRuntime[provider.ID]
		lastSuccess := ""
		lastError := ""
		cooldowns := map[string]string{}
		if state != nil {
			if !state.LastSuccess.IsZero() {
				lastSuccess = state.LastSuccess.Format(time.RFC3339)
			}
			lastError = state.LastError
			for modelID, until := range state.Cooldowns {
				if time.Now().Before(until) {
					cooldowns[modelID] = until.Format(time.RFC3339)
				}
			}
		}
		items = append(items, map[string]any{
			"id": provider.ID, "name": provider.Name, "protocol": provider.Protocol,
			"baseURL": provider.BaseURL, "enabled": provider.Enabled,
			"hasApiKey": provider.APIKey != "", "keyPreview": customProviderKeyPreview(provider.APIKey),
			"models":    provider.Models,
			"createdAt": provider.CreatedAt, "updatedAt": provider.UpdatedAt,
			"runtime": map[string]any{
				"lastSuccess": lastSuccess, "lastError": lastError, "cooldowns": cooldowns,
			},
		})
	}
	return map[string]any{"strategy": strategy, "providers": items}
}

// callCustomProviderChat is the custom-provider seam. Callers always send and
// receive OpenAI Chat Completions semantics; protocol-specific behavior stays
// behind the selected adapter.
func callCustomProviderChat(ctx context.Context, params map[string]any, stream bool) (*http.Response, *CustomProvider, int, error) {
	publicModelID, _ := params["model"].(string)
	routes := customProviderRoutesForModel(publicModelID)
	if len(routes) == 0 {
		return nil, nil, 0, newUpstreamHTTPError(http.StatusTooManyRequests, "all custom provider channels for this model are cooling down")
	}
	attempts := len(routes)
	if attempts > customProviderMaxAttempts {
		attempts = customProviderMaxAttempts
	}
	var lastErr error
	var lastProvider *CustomProvider
	attempted := 0
	for attempt := 0; attempt < attempts; attempt++ {
		attempted++
		route := routes[attempt]
		provider := route.Provider
		lastProvider = &provider
		adapter, err := customProviderAdapterFor(provider.Protocol)
		if err != nil {
			return nil, lastProvider, attempt, err
		}
		upstreamParams := make(map[string]any, len(params))
		for key, value := range params {
			upstreamParams[key] = value
		}
		upstreamParams["model"] = route.Model.ID
		response, err := adapter.Chat(ctx, provider, upstreamParams, stream)
		if err == nil {
			response, err = rewriteCustomProviderResponseModel(response, publicModelID, stream)
		}
		if err == nil && stream {
			prepared, _, prepareErr := prepareSemanticChatStreamWithTimeout(response, upstreamFirstEventTimeout)
			if prepareErr != nil {
				response.Body.Close()
				err = prepareErr
			} else {
				response = prepared
			}
		}
		if err == nil {
			markCustomProviderSuccess(provider.ID, route.Model.ID)
			return response, lastProvider, attempt, nil
		}
		lastErr = err
		cooldown := customProviderFailureCooldown(err)
		markCustomProviderFailure(provider.ID, route.Model.ID, err, cooldown)
		if !customProviderRetryable(err) {
			break
		}
	}
	return nil, lastProvider, attempted - 1, lastErr
}

func rewriteCustomProviderResponseModel(response *http.Response, publicModelID string, stream bool) (*http.Response, error) {
	if stream {
		originalBody := response.Body
		reader, writer := io.Pipe()
		response.Body = reader
		response.ContentLength = -1
		go func() {
			defer originalBody.Close()
			defer writer.Close()
			buffered := bufio.NewReader(originalBody)
			for {
				line, readErr := buffered.ReadString('\n')
				if line != "" {
					trimmed := strings.TrimRight(line, "\r\n")
					ending := line[len(trimmed):]
					if strings.HasPrefix(trimmed, "data:") {
						payload := strings.TrimSpace(trimmed[5:])
						if payload != "" && payload != "[DONE]" {
							var object map[string]any
							if json.Unmarshal([]byte(payload), &object) == nil {
								object["model"] = publicModelID
								if encoded, err := json.Marshal(object); err == nil {
									trimmed = "data: " + string(encoded)
								}
							}
						}
					}
					if _, err := io.WriteString(writer, trimmed+ending); err != nil {
						return
					}
				}
				if readErr != nil {
					if !errors.Is(readErr, io.EOF) {
						_ = writer.CloseWithError(readErr)
					}
					return
				}
			}
		}()
		return response, nil
	}

	defer response.Body.Close()
	var object map[string]any
	if err := json.NewDecoder(response.Body).Decode(&object); err != nil {
		return nil, fmt.Errorf("decode provider response: %w", err)
	}
	object["model"] = publicModelID
	if data, _ := object["data"].(map[string]any); data != nil {
		data["model"] = publicModelID
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	response.Body = io.NopCloser(bytes.NewReader(encoded))
	response.ContentLength = int64(len(encoded))
	return response, nil
}

func openAIImageToAnthropic(value any) (map[string]any, bool) {
	if rawURL, ok := value.(string); ok {
		value = map[string]any{"url": rawURL}
	}
	image, _ := value.(map[string]any)
	if image == nil {
		return nil, false
	}
	rawURL, _ := image["url"].(string)
	if rawURL == "" {
		rawURL, _ = image["image_url"].(string)
	}
	if rawURL == "" {
		return nil, false
	}
	if strings.HasPrefix(rawURL, "data:") {
		comma := strings.Index(rawURL, ",")
		semicolon := strings.Index(rawURL, ";")
		if comma > 5 && semicolon > 5 && semicolon < comma && strings.Contains(rawURL[semicolon:comma], "base64") {
			mediaType := rawURL[5:semicolon]
			data := rawURL[comma+1:]
			if _, err := base64.StdEncoding.DecodeString(data); err == nil {
				return map[string]any{
					"type":   "image",
					"source": map[string]any{"type": "base64", "media_type": mediaType, "data": data},
				}, true
			}
		}
	}
	return map[string]any{
		"type":   "image",
		"source": map[string]any{"type": "url", "url": rawURL},
	}, true
}

func openAIContentToAnthropic(content any) []any {
	blocks := []any{}
	switch value := content.(type) {
	case string:
		if value != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": value})
		}
	case []any:
		for _, raw := range value {
			block, _ := raw.(map[string]any)
			if block == nil {
				continue
			}
			switch block["type"] {
			case "text", "input_text", "output_text":
				text, _ := block["text"].(string)
				if text != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": text})
				}
			case "image_url", "input_image":
				imageValue := block["image_url"]
				if imageValue == nil {
					imageValue = block
				}
				if image, ok := openAIImageToAnthropic(imageValue); ok {
					blocks = append(blocks, image)
				}
			}
		}
	}
	return blocks
}

func appendAnthropicMessage(messages *[]any, role string, blocks []any) {
	if len(blocks) == 0 {
		return
	}
	if len(*messages) > 0 {
		last, _ := (*messages)[len(*messages)-1].(map[string]any)
		if last != nil && last["role"] == role {
			content, _ := last["content"].([]any)
			last["content"] = append(content, blocks...)
			return
		}
	}
	*messages = append(*messages, map[string]any{"role": role, "content": blocks})
}

func openAIToolsToAnthropic(value any) []any {
	tools, _ := value.([]any)
	converted := make([]any, 0, len(tools))
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		if tool == nil || (tool["type"] != nil && tool["type"] != "function") {
			continue
		}
		function, _ := tool["function"].(map[string]any)
		if function == nil {
			continue
		}
		name, _ := function["name"].(string)
		if name == "" {
			continue
		}
		inputSchema := function["parameters"]
		if inputSchema == nil {
			inputSchema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		convertedTool := map[string]any{"name": name, "input_schema": inputSchema}
		if description, _ := function["description"].(string); description != "" {
			convertedTool["description"] = description
		}
		converted = append(converted, convertedTool)
	}
	return converted
}

func openAIToolChoiceToAnthropic(value any) any {
	switch choice := value.(type) {
	case string:
		switch choice {
		case "auto":
			return map[string]any{"type": "auto"}
		case "required":
			return map[string]any{"type": "any"}
		}
	case map[string]any:
		if function, _ := choice["function"].(map[string]any); function != nil {
			if name, _ := function["name"].(string); name != "" {
				return map[string]any{"type": "tool", "name": name}
			}
		}
	}
	return nil
}

func openAIChatToAnthropicRequest(params map[string]any, stream bool) map[string]any {
	model, _ := params["model"].(string)
	maxTokens := int(tokenCount(params["max_tokens"]))
	if maxTokens == 0 {
		maxTokens = int(tokenCount(params["max_completion_tokens"]))
	}
	if maxTokens == 0 {
		maxTokens = 4096
	}
	body := map[string]any{"model": model, "max_tokens": maxTokens, "stream": stream}
	systemBlocks := []any{}
	messages := []any{}
	openAIMessages, _ := params["messages"].([]any)
	for _, raw := range openAIMessages {
		message, _ := raw.(map[string]any)
		if message == nil {
			continue
		}
		role, _ := message["role"].(string)
		switch role {
		case "system", "developer":
			systemBlocks = append(systemBlocks, openAIContentToAnthropic(message["content"])...)
		case "tool":
			toolCallID, _ := message["tool_call_id"].(string)
			if toolCallID == "" {
				continue
			}
			content := extractStringContentValue(message["content"])
			appendAnthropicMessage(&messages, "user", []any{map[string]any{
				"type": "tool_result", "tool_use_id": toolCallID, "content": content,
			}})
		case "assistant":
			blocks := openAIContentToAnthropic(message["content"])
			toolCalls, _ := message["tool_calls"].([]any)
			for _, rawToolCall := range toolCalls {
				toolCall, _ := rawToolCall.(map[string]any)
				function, _ := toolCall["function"].(map[string]any)
				if toolCall == nil || function == nil {
					continue
				}
				arguments := any(map[string]any{})
				if rawArguments, _ := function["arguments"].(string); rawArguments != "" {
					if json.Unmarshal([]byte(rawArguments), &arguments) != nil {
						arguments = map[string]any{"value": rawArguments}
					}
				}
				blocks = append(blocks, map[string]any{
					"type": "tool_use", "id": toolCall["id"], "name": function["name"], "input": arguments,
				})
			}
			appendAnthropicMessage(&messages, "assistant", blocks)
		default:
			appendAnthropicMessage(&messages, "user", openAIContentToAnthropic(message["content"]))
		}
	}
	if len(systemBlocks) > 0 {
		body["system"] = systemBlocks
	}
	if len(messages) == 0 {
		messages = append(messages, map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": ""}}})
	}
	body["messages"] = messages
	for _, key := range []string{"temperature", "top_p", "top_k"} {
		if value, exists := params[key]; exists {
			body[key] = value
		}
	}
	if stop, exists := params["stop"]; exists {
		if stopString, ok := stop.(string); ok {
			body["stop_sequences"] = []any{stopString}
		} else {
			body["stop_sequences"] = stop
		}
	}
	if tools := openAIToolsToAnthropic(params["tools"]); len(tools) > 0 {
		body["tools"] = tools
		if choice := openAIToolChoiceToAnthropic(params["tool_choice"]); choice != nil {
			body["tool_choice"] = choice
		}
	}
	if thinking, ok := params["thinking"].(map[string]any); ok {
		body["thinking"] = thinking
	} else if effort, _ := params["reasoning_effort"].(string); effort != "" {
		body["output_config"] = map[string]any{"effort": effort}
	}
	if userID, _ := params["user"].(string); userID != "" {
		body["metadata"] = map[string]any{"user_id": userID}
	}
	return body
}

func extractStringContentValue(value any) string {
	switch content := value.(type) {
	case string:
		return content
	case []any:
		parts := []string{}
		for _, raw := range content {
			block, _ := raw.(map[string]any)
			if block == nil {
				continue
			}
			if text, _ := block["text"].(string); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		encoded, _ := json.Marshal(value)
		return string(encoded)
	}
}

func anthropicUsageToOpenAI(value any) map[string]any {
	usage, _ := value.(map[string]any)
	if usage == nil {
		return nil
	}
	inputTokens := tokenCount(usage["input_tokens"])
	cacheRead := tokenCount(usage["cache_read_input_tokens"])
	cacheWrite := tokenCount(usage["cache_creation_input_tokens"])
	outputTokens := tokenCount(usage["output_tokens"])
	thinkingTokens := tokenCount(getNested(usage, "output_tokens_details", "thinking_tokens"))
	promptTokens := inputTokens + cacheRead + cacheWrite
	return map[string]any{
		"prompt_tokens": promptTokens, "completion_tokens": outputTokens,
		"total_tokens": promptTokens + outputTokens,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": cacheRead, "cache_write_tokens": cacheWrite,
		},
		"completion_tokens_details": map[string]any{"reasoning_tokens": thinkingTokens},
	}
}

func anthropicStopReasonToOpenAI(reason string) string {
	switch reason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}

func anthropicResponseToOpenAI(response map[string]any) map[string]any {
	content, _ := response["content"].([]any)
	messageContent, toolCalls, _, reasoning := anthropicContentToOpenAI(content)
	message := map[string]any{"role": "assistant", "content": messageContent}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	stopReason, _ := response["stop_reason"].(string)
	return map[string]any{
		"id": response["id"], "object": "chat.completion", "created": time.Now().Unix(),
		"model": response["model"],
		"choices": []any{map[string]any{
			"index": 0, "message": message, "finish_reason": anthropicStopReasonToOpenAI(stopReason),
		}},
		"usage": anthropicUsageToOpenAI(response["usage"]),
	}
}

type anthropicStreamBlock struct {
	Type      string
	ToolIndex int
}

func mergeAnthropicUsage(current map[string]any, next any) map[string]any {
	if current == nil {
		current = map[string]any{}
	}
	update, _ := next.(map[string]any)
	for key, value := range update {
		if details, ok := value.(map[string]any); ok {
			existing, _ := current[key].(map[string]any)
			if existing == nil {
				existing = map[string]any{}
			}
			for detailKey, detailValue := range details {
				existing[detailKey] = detailValue
			}
			current[key] = existing
			continue
		}
		current[key] = value
	}
	return current
}

func anthropicStreamToOpenAIResponse(response *http.Response) *http.Response {
	originalBody := response.Body
	reader, writer := io.Pipe()
	response.Body = reader
	response.ContentLength = -1
	response.Header = response.Header.Clone()
	response.Header.Set("Content-Type", "text/event-stream")
	go convertAnthropicStreamToOpenAI(originalBody, writer)
	return response
}

func convertAnthropicStreamToOpenAI(source io.ReadCloser, destination *io.PipeWriter) {
	defer source.Close()
	defer destination.Close()

	messageID := "chatcmpl_" + secureRandomHex(12)
	model := ""
	created := time.Now().Unix()
	blocks := map[int]anthropicStreamBlock{}
	nextToolIndex := 0
	usage := map[string]any{}
	finishSent := false
	doneSent := false

	emit := func(delta map[string]any, finishReason any, includeUsage bool) error {
		chunk := map[string]any{
			"id": messageID, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason}},
		}
		if includeUsage {
			chunk["usage"] = anthropicUsageToOpenAI(usage)
		}
		encoded, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(destination, "data: %s\n\n", encoded)
		return err
	}
	emitDone := func() error {
		if doneSent {
			return nil
		}
		doneSent = true
		_, err := io.WriteString(destination, "data: [DONE]\n\n")
		return err
	}

	buffered := bufio.NewReader(source)
	for {
		line, readErr := buffered.ReadString('\n')
		if line != "" {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "data:") {
				payload := strings.TrimSpace(trimmed[5:])
				if payload != "" {
					var event map[string]any
					if err := json.Unmarshal([]byte(payload), &event); err == nil {
						eventType, _ := event["type"].(string)
						switch eventType {
						case "message_start":
							message, _ := event["message"].(map[string]any)
							if id, _ := message["id"].(string); id != "" {
								messageID = id
							}
							model, _ = message["model"].(string)
							usage = mergeAnthropicUsage(usage, message["usage"])
							if err := emit(map[string]any{"role": "assistant"}, nil, false); err != nil {
								return
							}
						case "content_block_start":
							index := int(tokenCount(event["index"]))
							block, _ := event["content_block"].(map[string]any)
							blockType, _ := block["type"].(string)
							state := anthropicStreamBlock{Type: blockType, ToolIndex: -1}
							switch blockType {
							case "text":
								if text, _ := block["text"].(string); text != "" {
									if err := emit(map[string]any{"content": text}, nil, false); err != nil {
										return
									}
								}
							case "thinking":
								if thinking, _ := block["thinking"].(string); thinking != "" {
									if err := emit(map[string]any{"reasoning_content": thinking}, nil, false); err != nil {
										return
									}
								}
							case "tool_use":
								state.ToolIndex = nextToolIndex
								nextToolIndex++
								arguments := ""
								if input := block["input"]; input != nil {
									if encoded, err := json.Marshal(input); err == nil && string(encoded) != "{}" {
										arguments = string(encoded)
									}
								}
								toolCall := map[string]any{
									"index": state.ToolIndex, "id": block["id"], "type": "function",
									"function": map[string]any{"name": block["name"], "arguments": arguments},
								}
								if err := emit(map[string]any{"tool_calls": []any{toolCall}}, nil, false); err != nil {
									return
								}
							}
							blocks[index] = state
						case "content_block_delta":
							index := int(tokenCount(event["index"]))
							state := blocks[index]
							delta, _ := event["delta"].(map[string]any)
							switch delta["type"] {
							case "text_delta":
								if text, _ := delta["text"].(string); text != "" {
									if err := emit(map[string]any{"content": text}, nil, false); err != nil {
										return
									}
								}
							case "thinking_delta":
								if thinking, _ := delta["thinking"].(string); thinking != "" {
									if err := emit(map[string]any{"reasoning_content": thinking}, nil, false); err != nil {
										return
									}
								}
							case "input_json_delta":
								arguments, _ := delta["partial_json"].(string)
								toolCall := map[string]any{
									"index": state.ToolIndex, "function": map[string]any{"arguments": arguments},
								}
								if err := emit(map[string]any{"tool_calls": []any{toolCall}}, nil, false); err != nil {
									return
								}
							}
						case "message_delta":
							usage = mergeAnthropicUsage(usage, event["usage"])
							delta, _ := event["delta"].(map[string]any)
							stopReason, _ := delta["stop_reason"].(string)
							if stopReason != "" {
								finishSent = true
								if err := emit(map[string]any{}, anthropicStopReasonToOpenAI(stopReason), true); err != nil {
									return
								}
							}
						case "message_stop":
							if !finishSent {
								if err := emit(map[string]any{}, "stop", true); err != nil {
									return
								}
								finishSent = true
							}
							if err := emitDone(); err != nil {
								return
							}
						case "error":
							encoded, _ := json.Marshal(map[string]any{"error": event["error"]})
							_, _ = fmt.Fprintf(destination, "data: %s\n\n", encoded)
							return
						}
					}
				}
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				_ = destination.CloseWithError(readErr)
			}
			return
		}
	}
}
