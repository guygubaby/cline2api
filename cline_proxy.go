package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	clineEgressProxyPathEnv = "CLINE_EGRESS_PROXY_CONFIG_PATH"
	clineEgressProxyFile    = ".cline-proxy.json"
)

type clineProxyConfigData struct {
	Proxies       []string `json:"proxies"`
	ProxyStrategy string   `json:"proxyStrategy"`
}

var (
	clineProxyConfigMu sync.Mutex
	clineProxySaveMu   sync.Mutex
	clineProxyConfig   *clineProxyConfigData
	clineProxyCounter  atomic.Uint64
	clineEnvProxy      = http.ProxyFromEnvironment
)

func clineProxyConfigPath() string {
	if configured := strings.TrimSpace(os.Getenv(clineEgressProxyPathEnv)); configured != "" {
		return configured
	}
	return resolveDataPath(clineEgressProxyFile)
}

func defaultClineProxyConfig() *clineProxyConfigData {
	return &clineProxyConfigData{Proxies: []string{}, ProxyStrategy: "round_robin"}
}

func normalizeClineProxyConfig(config *clineProxyConfigData) {
	if config == nil {
		return
	}
	cleaned := make([]string, 0, len(config.Proxies))
	seen := make(map[string]struct{}, len(config.Proxies))
	for _, proxyURL := range config.Proxies {
		proxyURL = strings.TrimSpace(proxyURL)
		if proxyURL == "" {
			continue
		}
		if _, exists := seen[proxyURL]; exists {
			continue
		}
		seen[proxyURL] = struct{}{}
		cleaned = append(cleaned, proxyURL)
	}
	config.Proxies = cleaned
	switch config.ProxyStrategy {
	case "random", "fill", "round_robin":
	default:
		config.ProxyStrategy = "round_robin"
	}
}

func cloneClineProxyConfig(config *clineProxyConfigData) *clineProxyConfigData {
	if config == nil {
		return defaultClineProxyConfig()
	}
	return &clineProxyConfigData{
		Proxies:       append([]string(nil), config.Proxies...),
		ProxyStrategy: config.ProxyStrategy,
	}
}

func loadClineProxyConfig() *clineProxyConfigData {
	config := defaultClineProxyConfig()
	data, err := os.ReadFile(clineProxyConfigPath())
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("cline proxy config read failed: %v", err)
		}
		return config
	}
	if err := json.Unmarshal(data, config); err != nil {
		log.Printf("cline proxy config parse failed: %v", err)
		return defaultClineProxyConfig()
	}
	normalizeClineProxyConfig(config)
	return config
}

func getClineProxyConfig() *clineProxyConfigData {
	clineProxyConfigMu.Lock()
	defer clineProxyConfigMu.Unlock()
	if clineProxyConfig == nil {
		clineProxyConfig = loadClineProxyConfig()
	}
	return cloneClineProxyConfig(clineProxyConfig)
}

func setClineProxyConfig(config *clineProxyConfigData) error {
	clineProxySaveMu.Lock()
	defer clineProxySaveMu.Unlock()
	updated := cloneClineProxyConfig(config)
	normalizeClineProxyConfig(updated)
	if err := validateProxyList(updated.Proxies); err != nil {
		return err
	}
	data, err := json.MarshalIndent(updated, "", "  ")
	if err != nil {
		return fmt.Errorf("encode cline proxy config: %w", err)
	}
	if err := writeFileDurably(clineProxyConfigPath(), data, 0600); err != nil {
		return fmt.Errorf("save cline proxy config: %w", err)
	}
	clineProxyConfigMu.Lock()
	clineProxyConfig = updated
	clineProxyConfigMu.Unlock()
	httpTransport.CloseIdleConnections()
	return nil
}

func pickClineProxy(config *clineProxyConfigData) string {
	if config == nil || len(config.Proxies) == 0 {
		return ""
	}
	index := int(clineProxyCounter.Add(1)-1) % len(config.Proxies)
	switch config.ProxyStrategy {
	case "random":
		index = randIntn(len(config.Proxies))
	case "fill":
		index = 0
	}
	return config.Proxies[index]
}

func isClineProxyTargetHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	return host == "cline.bot" || strings.HasSuffix(host, ".cline.bot") ||
		host == "workos.com" || strings.HasSuffix(host, ".workos.com")
}

func maskClineProxyForDisplay(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "***"
	}
	display := &url.URL{Scheme: parsed.Scheme, Host: parsed.Host}
	if parsed.User != nil {
		display.User = url.User("hidden")
	}
	return strings.ReplaceAll(display.String(), "hidden@", "***@")
}

// clineOutboundProxy leaves non-Cline traffic on the normal environment proxy
// path. Returning the selected application proxy here makes it part of the
// transport connection-pool key, so round-robin remains effective even when
// keep-alive is enabled.
func clineOutboundProxy(request *http.Request) (*url.URL, error) {
	if request != nil && request.URL != nil && isClineProxyTargetHost(request.URL.Hostname()) {
		if selected := pickClineProxy(getClineProxyConfig()); selected != "" {
			parsed, err := url.Parse(selected)
			if err != nil {
				return nil, fmt.Errorf("parse Cline proxy URL: %w", err)
			}
			return parsed, nil
		}
	}
	return clineEnvProxy(request)
}

func clineDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return dialer.DialContext(ctx, network, addr)
}
