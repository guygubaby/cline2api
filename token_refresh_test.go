package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHandleAdminRefreshAllReportsPartialFailure(t *testing.T) {
	p := loadPool()
	poolMu.Lock()
	oldAccounts := p.Accounts
	p.Accounts = []*Account{
		{AccountID: "ok", Email: "ok@example.com", RefreshToken: "ok", Status: "active"},
		{AccountID: "bad", Email: "bad@example.com", RefreshToken: "bad", Status: "active"},
	}
	poolMu.Unlock()
	t.Cleanup(func() {
		poolMu.Lock()
		p.Accounts = oldAccounts
		poolMu.Unlock()
		savePool()
	})

	oldHTTPClient := httpClient
	httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read refresh request: %v", err)
		}
		if strings.Contains(string(body), `"refreshToken":"bad"`) {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Body:       io.NopCloser(strings.NewReader(`{"error":"invalid refresh token"}`)),
				Header:     make(http.Header),
				Request:    request,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(`{
				"data": {
					"accessToken": "new-access-token",
					"refreshToken": "new-refresh-token",
					"expiresAt": "2099-01-01T00:00:00Z"
				}
			}`)),
			Header:  make(http.Header),
			Request: request,
		}, nil
	})}
	t.Cleanup(func() { httpClient = oldHTTPClient })

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/refresh-all", nil)
	handleAdminRefreshAll(recorder, request)

	if recorder.Code != http.StatusMultiStatus {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusMultiStatus, recorder.Body.String())
	}
	var response apiResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Success {
		t.Fatalf("success = true, want false; response=%#v", response)
	}
	data, ok := response.Data.(map[string]any)
	if !ok {
		t.Fatalf("data = %#v, want object", response.Data)
	}
	if data["refreshed"] != float64(1) || data["failed"] != float64(1) {
		t.Fatalf("refresh counts = %#v, want refreshed=1 failed=1", data)
	}
	if p.Accounts[0].ExpiresAt <= time.Now().UnixMilli() {
		t.Fatalf("successful account expiry was not updated: %d", p.Accounts[0].ExpiresAt)
	}
}

func TestRefreshExpiringAccountTokensRefreshesOnlyNearExpiry(t *testing.T) {
	now := time.Now()
	accounts := []*Account{
		{
			AccountID: "fresh", Email: "fresh@example.com", RefreshToken: "fresh-refresh",
			AccessToken: "fresh-access", ExpiresAt: now.Add(10 * time.Minute).UnixMilli(), Status: "active",
		},
		{
			AccountID: "expiring", Email: "expiring@example.com", RefreshToken: "expiring-refresh",
			AccessToken: "expiring-access", ExpiresAt: now.Add(2 * time.Minute).UnixMilli(), Status: "active",
		},
		{
			AccountID: "inactive", Email: "inactive@example.com", RefreshToken: "inactive-refresh",
			ExpiresAt: now.Add(-time.Minute).UnixMilli(), Status: "expired",
		},
	}

	oldHTTPClient := httpClient
	var requestCount atomic.Int32
	httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestCount.Add(1)
		return successfulRefreshResponse(request), nil
	})}
	t.Cleanup(func() { httpClient = oldHTTPClient })

	summary := refreshExpiringAccountTokens(accounts, now, 5*time.Minute)
	if summary.Refreshed != 1 || summary.Skipped != 2 || summary.Failed != 0 {
		t.Fatalf("summary = %#v, want refreshed=1 skipped=2 failed=0", summary)
	}
	if requestCount.Load() != 1 {
		t.Fatalf("refresh requests = %d, want 1", requestCount.Load())
	}
	if accounts[1].AccessToken != "workos:new-access-token" {
		t.Fatalf("expiring account token = %q, want refreshed token", accounts[1].AccessToken)
	}
}

func TestConcurrentProactiveRefreshUsesRefreshTokenOnce(t *testing.T) {
	now := time.Now()
	account := &Account{
		AccountID: "expiring", Email: "expiring@example.com", RefreshToken: "expiring-refresh",
		AccessToken: "old-access", ExpiresAt: now.Add(time.Minute).UnixMilli(), Status: "active",
	}

	oldHTTPClient := httpClient
	var requestCount atomic.Int32
	httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestCount.Add(1)
		return successfulRefreshResponse(request), nil
	})}
	t.Cleanup(func() { httpClient = oldHTTPClient })

	start := make(chan struct{})
	results := make(chan bool, 2)
	errors := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			refreshed, err := refreshAccountTokenIfExpiring(account, now, 5*time.Minute)
			results <- refreshed
			errors <- err
		}()
	}
	close(start)
	group.Wait()
	close(results)
	close(errors)

	for err := range errors {
		if err != nil {
			t.Fatalf("proactive refresh: %v", err)
		}
	}
	refreshedCount := 0
	for refreshed := range results {
		if refreshed {
			refreshedCount++
		}
	}
	if refreshedCount != 1 {
		t.Fatalf("refreshed callers = %d, want 1", refreshedCount)
	}
	if requestCount.Load() != 1 {
		t.Fatalf("refresh requests = %d, want 1", requestCount.Load())
	}
}

func TestProactiveRefreshFailureKeepsStillValidAccountActive(t *testing.T) {
	now := time.Now()
	account := &Account{
		AccountID: "retry", Email: "retry@example.com", RefreshToken: "retry-refresh",
		AccessToken: "still-valid", ExpiresAt: now.Add(2 * time.Minute).UnixMilli(), Status: "active",
	}

	oldHTTPClient := httpClient
	httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader(`{"error":"temporary unavailable"}`)),
			Header:     make(http.Header),
			Request:    request,
		}, nil
	})}
	t.Cleanup(func() { httpClient = oldHTTPClient })

	summary := refreshExpiringAccountTokens([]*Account{account}, now, 5*time.Minute)
	if summary.Failed != 1 {
		t.Fatalf("summary = %#v, want one failure", summary)
	}
	if account.Status != "active" {
		t.Fatalf("account status = %q, want active while old token is valid", account.Status)
	}
	if account.AccessToken != "still-valid" {
		t.Fatalf("access token changed after failed proactive refresh: %q", account.AccessToken)
	}
}

func TestProactiveRefreshMarksMissingRefreshTokenExpired(t *testing.T) {
	now := time.Now()
	account := &Account{
		AccountID: "missing", Email: "missing@example.com",
		AccessToken: "expiring-access", ExpiresAt: now.Add(time.Minute).UnixMilli(), Status: "active",
	}

	summary := refreshExpiringAccountTokens([]*Account{account}, now, 5*time.Minute)
	if summary.Failed != 1 {
		t.Fatalf("summary = %#v, want one failure", summary)
	}
	if account.Status != "expired" {
		t.Fatalf("account status = %q, want expired", account.Status)
	}
}

func successfulRefreshResponse(request *http.Request) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(`{
			"data": {
				"accessToken": "new-access-token",
				"refreshToken": "new-refresh-token",
				"expiresAt": "2099-01-01T00:00:00Z"
			}
		}`)),
		Header:  make(http.Header),
		Request: request,
	}
}
