package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestHTTPTransportUsesHTTPSProxyFromEnvironment(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:8080")
	t.Setenv("NO_PROXY", "")

	req, err := http.NewRequest(http.MethodPost, "https://api.workos.com/user_management/authorize/device", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}

	proxyURL, err := httpTransport.Proxy(req)
	if err != nil {
		t.Fatalf("resolve proxy: %v", err)
	}
	if proxyURL == nil {
		t.Fatal("expected HTTPS_PROXY to be selected")
	}

	want, err := url.Parse("http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("parse expected proxy URL: %v", err)
	}
	if proxyURL.String() != want.String() {
		t.Fatalf("proxy URL = %q, want %q", proxyURL, want)
	}
}

func TestHTTPTransportAllowsLongInferenceTTFT(t *testing.T) {
	if httpTransport.ResponseHeaderTimeout < 5*time.Minute {
		t.Fatalf("response header timeout = %s, want at least 5m for long-context inference", httpTransport.ResponseHeaderTimeout)
	}
}

func TestReadLimitedRequestBodyRejectsOversizedInput(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("12345"))
	response := httptest.NewRecorder()
	if _, err := readLimitedRequestBody(response, request, 4); err == nil || requestBodyErrorStatus(err) != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized request error = %v", err)
	}
}

func TestProxyRequestContextPropagatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	params := map[string]any{}
	attachProxyRequestContext(params, ctx)
	cancel()
	select {
	case <-proxyRequestContext(params).Done():
	default:
		t.Fatal("proxy request context did not propagate cancellation")
	}
}

func TestClineUpstreamStopsWhenDownstreamContextIsCancelled(t *testing.T) {
	oldClient := httpClient
	httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	t.Cleanup(func() { httpClient = oldClient })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	params := map[string]any{"model": "m1", "messages": []any{map[string]any{"role": "user", "content": "hello"}}}
	attachProxyRequestContext(params, ctx)
	account := &Account{Email: "cancel@example.com", AccessToken: "workos:token", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active"}

	_, _, err := callClineAPIWithAccount(account, params, true)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled upstream error = %v", err)
	}
	if account.Status != "active" {
		t.Fatalf("downstream cancellation changed account status to %q", account.Status)
	}
}
