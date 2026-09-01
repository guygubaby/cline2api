package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func withFreeModelTestPool(t *testing.T, accounts ...*Account) {
	t.Helper()
	currentPool := loadPool()
	poolMu.Lock()
	oldAccounts, oldIndex := currentPool.Accounts, currentPool.CurrentIdx
	currentPool.Accounts = accounts
	currentPool.CurrentIdx = 0
	poolMu.Unlock()
	t.Cleanup(func() {
		poolMu.Lock()
		currentPool.Accounts = oldAccounts
		currentPool.CurrentIdx = oldIndex
		poolMu.Unlock()
		savePool()
	})
}

func freeModelTestAccount(id string) *Account {
	return &Account{
		AccountID: id, Email: id + "@example.com", AccessToken: "workos:" + id,
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active", ModelCooldowns: map[string]time.Time{},
	}
}

func TestCline401RetryReplaysOriginalRequestBody(t *testing.T) {
	account := freeModelTestAccount("refresh")
	account.RefreshToken = "refresh-token"
	withFreeModelTestPool(t, account)

	oldHTTPClient := httpClient
	var requestBodies [][]byte
	refreshCalls := 0
	httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/api/v1/auth/refresh":
			refreshCalls++
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: io.NopCloser(strings.NewReader(
					`{"data":{"accessToken":"new-access-token","refreshToken":"new-refresh-token","expiresAt":4102444800000}}`,
				)),
				Header: http.Header{}, Request: request,
			}, nil
		case "/api/v1/chat/completions":
			body, err := io.ReadAll(request.Body)
			if err != nil {
				return nil, err
			}
			requestBodies = append(requestBodies, body)
			status := http.StatusUnauthorized
			responseBody := `{"error":"unauthorized"}`
			if len(requestBodies) == 2 {
				status = http.StatusOK
				responseBody = `{"id":"ok","choices":[]}`
			}
			return &http.Response{
				StatusCode: status, Body: io.NopCloser(strings.NewReader(responseBody)),
				Header: http.Header{}, Request: request,
			}, nil
		default:
			return nil, fmt.Errorf("unexpected request path %s", request.URL.Path)
		}
	})}
	t.Cleanup(func() { httpClient = oldHTTPClient })

	response, _, err := callClineAPIWithAccount(account, map[string]any{
		"model":    "z-ai/glm-5.3-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, false)
	if err != nil {
		t.Fatalf("401 retry: %v", err)
	}
	defer response.Body.Close()
	if refreshCalls != 1 || len(requestBodies) != 2 {
		t.Fatalf("refresh calls=%d request bodies=%d", refreshCalls, len(requestBodies))
	}
	if len(requestBodies[0]) == 0 || string(requestBodies[0]) != string(requestBodies[1]) {
		t.Fatalf("retry request body was not replayed: first=%q second=%q", requestBodies[0], requestBodies[1])
	}
}

func TestVirtualFreeModelUsesBoundedFallbackChain(t *testing.T) {
	withFreeModelTestPool(t, freeModelTestAccount("one"), freeModelTestAccount("two"))

	oldHTTPClient := httpClient
	var models []string
	httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			return nil, err
		}
		model, _ := body["model"].(string)
		models = append(models, model)
		status := http.StatusTooManyRequests
		responseBody := `{"error":"quota","message":"Try again in 1h"}`
		if model == "deepseek/deepseek-v4-flash" {
			status = http.StatusOK
			responseBody = `{"id":"ok","choices":[]}`
		}
		return &http.Response{
			StatusCode: status, Body: io.NopCloser(strings.NewReader(responseBody)),
			Header: http.Header{}, Request: request,
		}, nil
	})}
	t.Cleanup(func() { httpClient = oldHTTPClient })

	params := map[string]any{
		"model":    virtualFreeModel,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	response, _, err := callClineAPI(params, false)
	if err != nil {
		t.Fatalf("free fallback: %v", err)
	}
	defer response.Body.Close()
	want := []string{"z-ai/glm-5.3-flash", "z-ai/glm-5.3-flash", "deepseek/deepseek-v4-flash"}
	if strings.Join(models, ",") != strings.Join(want, ",") {
		t.Fatalf("fallback models = %v, want %v", models, want)
	}
	if params["model"] != "deepseek/deepseek-v4-flash" {
		t.Fatalf("effective model = %#v", params["model"])
	}
}

func TestVirtualFreeModelAttemptsAtMostTwoAccountsPerModel(t *testing.T) {
	withFreeModelTestPool(t,
		freeModelTestAccount("one"), freeModelTestAccount("two"), freeModelTestAccount("three"),
	)

	oldHTTPClient := httpClient
	var models []string
	httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			return nil, err
		}
		models = append(models, body["model"].(string))
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Body:       io.NopCloser(strings.NewReader(`{"error":"quota","message":"Try again in 1h"}`)),
			Header:     http.Header{}, Request: request,
		}, nil
	})}
	t.Cleanup(func() { httpClient = oldHTTPClient })

	params := map[string]any{"model": virtualFreeModel, "messages": []any{map[string]any{"role": "user", "content": "hello"}}}
	_, _, err := callClineAPI(params, false)
	if upstreamErrorStatus(err) != http.StatusTooManyRequests {
		t.Fatalf("free exhaustion error = %v", err)
	}
	if len(models) != len(freeModelChain)*freeModelAttemptsPerModel {
		t.Fatalf("attempts = %d, want %d: %v", len(models), len(freeModelChain)*freeModelAttemptsPerModel, models)
	}
	for chainIndex, model := range freeModelChain {
		start := chainIndex * freeModelAttemptsPerModel
		for _, attempted := range models[start : start+freeModelAttemptsPerModel] {
			if attempted != model {
				t.Fatalf("attempted model = %q, want %q; all=%v", attempted, model, models)
			}
		}
	}
}

func TestOfflineFallbackModelsAreCurrent(t *testing.T) {
	if fallbackDefaultModel != "z-ai/glm-5.3-flash" {
		t.Fatalf("fallback default = %q", fallbackDefaultModel)
	}
	want := map[string]bool{"z-ai/glm-5.3-flash": false, "cline-free/longcat-2.0": false}
	for _, model := range builtinModels {
		if _, exists := want[model.ID]; exists {
			want[model.ID] = true
		}
		if model.ID == "cline-free/glm-5.2" {
			t.Fatal("delisted cline-free/glm-5.2 remains in offline fallbacks")
		}
	}
	for model, found := range want {
		if !found {
			t.Fatalf("offline fallback %q is missing", model)
		}
	}
}

func TestVirtualFreeEffectiveModelAndAnthropicRateLimitMetadata(t *testing.T) {
	entry := &RequestLog{StartedAt: time.Now(), Model: virtualFreeModel, Protocol: "anthropic"}
	setRequestLogEffectiveModel(entry, map[string]any{"model": freeModelLastResort})
	if entry.Model != freeModelLastResort {
		t.Fatalf("effective request log model = %q", entry.Model)
	}

	status, errorType := anthropicUpstreamErrorDetails(
		newUpstreamHTTPError(http.StatusTooManyRequests, "free model pool exhausted"),
	)
	if status != http.StatusTooManyRequests || errorType != "rate_limit_error" {
		t.Fatalf("Anthropic upstream error = status %d type %q", status, errorType)
	}
}
