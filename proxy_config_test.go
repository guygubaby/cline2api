package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func isolateProxyConfigPersistence(t *testing.T) {
	t.Helper()
	proxyConfigSaveMu.Lock()
	oldPath := proxyConfigPath
	proxyConfigMu.Lock()
	oldConfig := cloneProxyConfig(proxyConfig)
	proxyConfigMu.Unlock()
	proxyConfigPath = filepath.Join(t.TempDir(), proxyConfigFile)
	proxyConfigSaveMu.Unlock()

	t.Cleanup(func() {
		proxyConfigSaveMu.Lock()
		proxyConfigMu.Lock()
		proxyConfig = oldConfig
		proxyConfigMu.Unlock()
		proxyConfigPath = oldPath
		proxyConfigSaveMu.Unlock()
	})
}

func TestProxyConfigPersistsAndReloadsHeaders(t *testing.T) {
	isolateProxyConfigPersistence(t)
	cfg := defaultProxyConfig()
	cfg.Strategy = "random"
	cfg.Headers["X-Test-Header"] = "persisted"

	if err := setProxyConfig(cfg); err != nil {
		t.Fatalf("save proxy config: %v", err)
	}
	loaded := loadProxyConfigFromPath(proxyConfigPath)
	if loaded.Strategy != "random" || loaded.Headers["X-Test-Header"] != "persisted" {
		t.Fatalf("loaded proxy config = %#v", loaded)
	}
	if loaded.Headers["User-Agent"] == "" {
		t.Fatal("persisted proxy config lost default headers")
	}
}

func TestConfiguredProxyConfigPathUsesEnvironment(t *testing.T) {
	want := filepath.Join(t.TempDir(), proxyConfigFile)
	t.Setenv(proxyConfigPathEnv, want)
	if got := configuredProxyConfigPath(); got != want {
		t.Fatalf("configured path = %q, want %q", got, want)
	}
}

func TestDockerComposeProxyConfigStorageDoesNotRequirePrecreatedFile(t *testing.T) {
	compose, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatalf("read docker-compose.yml: %v", err)
	}
	configuration := string(compose)
	if strings.Contains(configuration, "source: ./.cline-config.json") {
		t.Fatal("proxy config storage uses a file bind that fails when the runtime-created file does not exist")
	}
	for _, expected := range []string{
		"source: config-data",
		"target: /app/config-data",
		"CLINE_CONFIG_PATH=/app/config-data/.cline-config.json",
	} {
		if !strings.Contains(configuration, expected) {
			t.Fatalf("docker compose proxy config storage missing %q", expected)
		}
	}
}
