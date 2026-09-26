package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func withIsolatedPool(t *testing.T) *AccountPool {
	t.Helper()
	poolMu.Lock()
	oldPool := pool
	oldPath := poolPath
	isolated := &AccountPool{Accounts: []*Account{}, Keys: []string{}, Models: []Model{}}
	pool = isolated
	poolPath = t.TempDir() + "/accounts.json"
	poolMu.Unlock()
	t.Cleanup(func() {
		poolMu.Lock()
		pool = oldPool
		poolPath = oldPath
		poolMu.Unlock()
	})
	return isolated
}

func TestAddAccountIfUniqueIsAtomicAndTrimsTokens(t *testing.T) {
	isolated := withIsolatedPool(t)
	var added atomic.Int64
	var group sync.WaitGroup
	for index := 0; index < 12; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			_, ok := addAccountIfUnique(&Account{AccountID: "candidate", RefreshToken: "  shared-refresh-token  "})
			if ok {
				added.Add(1)
			}
		}(index)
	}
	group.Wait()
	if added.Load() != 1 {
		t.Fatalf("added %d duplicate accounts, want exactly 1", added.Load())
	}
	poolMu.Lock()
	defer poolMu.Unlock()
	if len(isolated.Accounts) != 1 || isolated.Accounts[0].RefreshToken != "shared-refresh-token" {
		t.Fatalf("isolated accounts = %#v", isolated.Accounts)
	}
}

func TestBuildUpstreamBodyUsesClineMinimumTokenBudget(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  int
	}{
		{name: "zero means default", value: float64(0), want: defaultMaxTokens},
		{name: "tiny float", value: float64(1), want: minClineUpstreamMaxTokens},
		{name: "tiny int", value: 8, want: minClineUpstreamMaxTokens},
		{name: "boundary", value: int64(16), want: 16},
		{name: "normal", value: float64(4096), want: 4096},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := buildUpstreamBody(map[string]any{"model": "test/model", "max_tokens": test.value}, false, "session")
			if got := body["max_tokens"]; got != test.want {
				t.Fatalf("max_tokens = %#v, want %d", got, test.want)
			}
		})
	}
	body := buildUpstreamBody(map[string]any{"model": "test/model", "max_completion_tokens": float64(7)}, false, "session")
	if got := body["max_tokens"]; got != minClineUpstreamMaxTokens {
		t.Fatalf("max_completion_tokens clamp = %#v", got)
	}
}

func TestCurrentFreeModelChainUsesSynchronizedCatalog(t *testing.T) {
	isolated := withIsolatedPool(t)
	poolMu.Lock()
	isolated.Models = []Model{
		{ID: "new/free", Source: "remote", Cost: "free", Status: "active"},
		{ID: freeModelFallback, Source: "remote", Cost: "free", Status: "active"},
		{ID: freeModelPrimary, Source: "remote", Cost: "pass", Status: "active"},
		{ID: freeModelLastResort, Source: "remote", Cost: "free", Status: "inactive"},
	}
	poolMu.Unlock()
	remoteModelsEnabledMu.Lock()
	oldEnabled := remoteModelsEnabled
	remoteModelsEnabled = true
	remoteModelsEnabledMu.Unlock()
	t.Cleanup(func() {
		remoteModelsEnabledMu.Lock()
		remoteModelsEnabled = oldEnabled
		remoteModelsEnabledMu.Unlock()
	})
	got := strings.Join(currentFreeModelChain(), ",")
	want := freeModelFallback + ",new/free"
	if got != want {
		t.Fatalf("free chain = %q, want %q", got, want)
	}
}

func TestPreserveLockedModelMetadataIncludesExplicitZero(t *testing.T) {
	models := []Model{{ID: "locked", Context: 200000, Output: 32768}, {ID: "unlocked", Context: 10, Output: 5}}
	preserveLockedModelMetadata(models, map[string]Model{
		"locked":   {ID: "locked", Context: 0, Output: 0, MetaLocked: true},
		"unlocked": {ID: "unlocked", Context: 99, Output: 88},
	})
	if models[0].Context != 0 || models[0].Output != 0 || !models[0].MetaLocked {
		t.Fatalf("locked metadata was not preserved: %#v", models[0])
	}
	if models[1].Context != 10 || models[1].Output != 5 || models[1].MetaLocked {
		t.Fatalf("unlocked metadata changed: %#v", models[1])
	}
}

func TestAdminModelContextLocksAndUnlocksMetadata(t *testing.T) {
	isolated := withIsolatedPool(t)
	poolMu.Lock()
	isolated.Models = []Model{{ID: "remote/model", Source: "remote", Context: 200000, Output: 32000}}
	poolMu.Unlock()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/admin/api/models/context", strings.NewReader(`{"id":"remote/model","context":1000000,"output":128000}`))
	handleAdminModelContext(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("lock response = %d %s", recorder.Code, recorder.Body.String())
	}
	poolMu.Lock()
	locked := isolated.Models[0]
	poolMu.Unlock()
	if !locked.MetaLocked || locked.Context != 1000000 || locked.Output != 128000 {
		t.Fatalf("locked model = %#v", locked)
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/admin/api/models/context", strings.NewReader(`{"id":"remote/model","context":0,"output":0}`))
	handleAdminModelContext(recorder, request)
	poolMu.Lock()
	unlocked := isolated.Models[0]
	poolMu.Unlock()
	if recorder.Code != http.StatusOK || unlocked.MetaLocked {
		t.Fatalf("unlock response=%d model=%#v", recorder.Code, unlocked)
	}
}

func TestClineProxyOnlyTargetsClineAndWorkOS(t *testing.T) {
	if !isClineProxyTargetHost("api.cline.bot") || !isClineProxyTargetHost("api.workos.com") {
		t.Fatal("expected Cline and WorkOS hosts to be proxy targets")
	}
	if isClineProxyTargetHost("cline.bot.evil.example") || isClineProxyTargetHost("api.openai.com") {
		t.Fatal("unrelated hosts must not use the Cline proxy")
	}

	clineProxyConfigMu.Lock()
	oldConfig := clineProxyConfig
	clineProxyConfig = &clineProxyConfigData{Proxies: []string{"socks5://127.0.0.1:1080"}, ProxyStrategy: "round_robin"}
	clineProxyConfigMu.Unlock()
	oldEnvProxy := clineEnvProxy
	fallback, _ := url.Parse("http://127.0.0.1:8888")
	clineEnvProxy = func(*http.Request) (*url.URL, error) { return fallback, nil }
	t.Cleanup(func() {
		clineProxyConfigMu.Lock()
		clineProxyConfig = oldConfig
		clineProxyConfigMu.Unlock()
		clineEnvProxy = oldEnvProxy
	})

	clineRequest := httptest.NewRequest(http.MethodGet, "https://api.cline.bot/api/v1/models", nil)
	if selected, err := clineOutboundProxy(clineRequest); err != nil || selected == nil || selected.String() != "socks5://127.0.0.1:1080" {
		t.Fatalf("application Cline proxy = %v, %v", selected, err)
	}
	customRequest := httptest.NewRequest(http.MethodGet, "https://provider.example/v1/models", nil)
	if selected, err := clineOutboundProxy(customRequest); err != nil || selected == nil || selected.String() != fallback.String() {
		t.Fatalf("custom provider env proxy = %v, %v", selected, err)
	}
}

func TestClineProxyRoundRobinIsSelectedPerRequest(t *testing.T) {
	clineProxyConfigMu.Lock()
	oldConfig := clineProxyConfig
	clineProxyConfig = &clineProxyConfigData{
		Proxies:       []string{"http://127.0.0.1:8001", "socks5://127.0.0.1:8002"},
		ProxyStrategy: "round_robin",
	}
	clineProxyConfigMu.Unlock()
	oldCounter := clineProxyCounter.Load()
	clineProxyCounter.Store(0)
	t.Cleanup(func() {
		clineProxyConfigMu.Lock()
		clineProxyConfig = oldConfig
		clineProxyConfigMu.Unlock()
		clineProxyCounter.Store(oldCounter)
	})
	request := httptest.NewRequest(http.MethodGet, "https://api.cline.bot/api/v1/models", nil)
	first, err := clineOutboundProxy(request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := clineOutboundProxy(request)
	if err != nil {
		t.Fatal(err)
	}
	if first.String() != "http://127.0.0.1:8001" || second.String() != "socks5://127.0.0.1:8002" {
		t.Fatalf("round robin = %q then %q", first, second)
	}
}

func TestClineProxyAdminDoesNotRoundTripCredentials(t *testing.T) {
	t.Setenv(clineEgressProxyPathEnv, t.TempDir()+"/cline-proxy.json")
	clineProxyConfigMu.Lock()
	oldConfig := clineProxyConfig
	clineProxyConfig = nil
	clineProxyConfigMu.Unlock()
	t.Cleanup(func() {
		clineProxyConfigMu.Lock()
		clineProxyConfig = oldConfig
		clineProxyConfigMu.Unlock()
	})
	secretProxy := "http://user:super-secret@proxy.example:8080/?token=query-secret"
	if err := setClineProxyConfig(&clineProxyConfigData{Proxies: []string{secretProxy}, ProxyStrategy: "round_robin"}); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	handleClineProxyConfig(recorder, httptest.NewRequest(http.MethodGet, "/admin/api/cline-proxy/config", nil))
	if strings.Contains(recorder.Body.String(), "super-secret") || strings.Contains(recorder.Body.String(), "query-secret") {
		t.Fatalf("GET response leaked proxy credentials: %s", recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	handleClineProxyConfigUpdate(recorder, httptest.NewRequest(http.MethodPost, "/admin/api/cline-proxy/config/update", strings.NewReader(`{"proxyStrategy":"fill"}`)))
	config := getClineProxyConfig()
	if config.ProxyStrategy != "fill" || len(config.Proxies) != 1 || config.Proxies[0] != secretProxy {
		t.Fatalf("strategy-only update replaced secret proxy config: %#v", config)
	}
}

func TestZenRetryDelayAndCancelOnClose(t *testing.T) {
	for _, delay := range []time.Duration{time.Second, 10 * time.Minute} {
		if got := cappedZenRetryDelay(delay); got <= 0 || got > zenMaxRetryWait {
			t.Fatalf("capped delay for %s = %s", delay, got)
		}
	}
	cancelled := make(chan struct{})
	var once sync.Once
	response := &http.Response{Body: io.NopCloser(strings.NewReader("ok"))}
	response = withCancelOnClose(response, func() { once.Do(func() { close(cancelled) }) })
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-context.Background().Done():
		t.Fatal("unreachable")
	case <-time.After(time.Second):
		t.Fatal("closing response body did not cancel request context")
	}
}
