package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
)

const (
	proxyRequestIDParamKey     = "_cline2api_request_id"
	proxyTenantScopeParamKey   = "_cline2api_tenant_scope"
	proxyUpstreamTaskParamKey  = "_cline2api_upstream_task_id"
	proxyUpstreamCountParamKey = "_cline2api_upstream_attempt_count"
	responseAuditPrefixBytes   = 64 << 10
)

type tenantScopeContextKey struct{}

var auditHMACKey = secureRandomBytes(32)

func secureRandomBytes(bytesCount int) []byte {
	value := make([]byte, bytesCount)
	if _, err := rand.Read(value); err != nil {
		panic("secure random source unavailable: " + err.Error())
	}
	return value
}

func secureRandomHex(bytesCount int) string {
	return hex.EncodeToString(secureRandomBytes(bytesCount))
}

func newUpstreamTaskID() string {
	return "sess_" + secureRandomHex(16)
}

func newProxyRequestID() string {
	return "req_" + secureRandomHex(16)
}

func tenantScopeForAPIKey(apiKey string) string {
	if apiKey == "" {
		apiKey = "anonymous-local-access"
	}
	sum := sha256.Sum256([]byte("cline2api-tenant-v1\x00" + apiKey))
	return hex.EncodeToString(sum[:16])
}

func requestWithTenantScope(request *http.Request, apiKey string) *http.Request {
	scope := tenantScopeForAPIKey(apiKey)
	if apiKey == "" {
		// Without authentication there is no stable tenant boundary. Fail closed by
		// disabling cross-request shared state instead of treating all callers as one tenant.
		scope = "anonymous-request-" + secureRandomHex(16)
	}
	return request.WithContext(context.WithValue(request.Context(), tenantScopeContextKey{}, scope))
}

func requestTenantScope(request *http.Request) string {
	if request != nil {
		if scope, _ := request.Context().Value(tenantScopeContextKey{}).(string); scope != "" {
			return scope
		}
	}
	return tenantScopeForAPIKey("")
}

func scopedIdentityHash(purpose, tenantScope, value string) string {
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(purpose + "\x00" + tenantScope + "\x00" + value))
	return hex.EncodeToString(sum[:])
}

func namespaceCompactSessionID(tenantScope, sessionID string) string {
	return scopedIdentityHash("compact-session-v1", tenantScope, sessionID)
}

func namespaceClientRequestIdentity(params map[string]any, tenantScope string) {
	for _, field := range []string{"prompt_cache_key", "user", "safety_identifier"} {
		if value, _ := params[field].(string); value != "" {
			params[field] = scopedIdentityHash("client-identity-"+field, tenantScope, value)
		}
	}
}

func canonicalRequestAuditHash(params map[string]any) string {
	audited := make(map[string]any)
	for _, field := range []string{"model", "messages", "tools", "functions", "tool_choice", "response_format"} {
		if value, exists := params[field]; exists {
			audited[field] = value
		}
	}
	encoded, err := json.Marshal(audited)
	if err != nil {
		return ""
	}
	hasher := hmac.New(sha256.New, auditHMACKey)
	_, _ = hasher.Write(encoded)
	return hex.EncodeToString(hasher.Sum(nil))
}

func requestParamsWithoutInternalMetadata(params map[string]any) map[string]any {
	clean := make(map[string]any, len(params))
	for key, value := range params {
		if strings.HasPrefix(key, "_cline2api_") {
			continue
		}
		clean[key] = value
	}
	return clean
}

func attachRequestIsolation(params map[string]any, requestID, tenantScope string) {
	if requestID == "" {
		requestID = newProxyRequestID()
	}
	params[proxyRequestIDParamKey] = requestID
	params[proxyTenantScopeParamKey] = tenantScope
	namespaceClientRequestIdentity(params, tenantScope)
}

func proxyRequestAuditFields(params map[string]any) (string, string) {
	requestID, _ := params[proxyRequestIDParamKey].(string)
	if requestID == "" {
		requestID = newProxyRequestID()
		params[proxyRequestIDParamKey] = requestID
	}
	return requestID, canonicalRequestAuditHash(params)
}

func recordUpstreamAttempt(params map[string]any, taskID string) {
	count, _ := params[proxyUpstreamCountParamKey].(int)
	params[proxyUpstreamCountParamKey] = count + 1
	params[proxyUpstreamTaskParamKey] = taskID
}

type auditedResponseBody struct {
	body       io.ReadCloser
	hasher     hash.Hash
	remaining  int
	requestID  string
	taskID     string
	statusCode int
	mu         sync.Mutex
	once       sync.Once
}

func (body *auditedResponseBody) Read(buffer []byte) (int, error) {
	read, err := body.body.Read(buffer)
	if read > 0 {
		body.mu.Lock()
		if body.remaining > 0 {
			count := read
			if count > body.remaining {
				count = body.remaining
			}
			_, _ = body.hasher.Write(buffer[:count])
			body.remaining -= count
		}
		body.mu.Unlock()
	}
	return read, err
}

func (body *auditedResponseBody) Close() error {
	err := body.body.Close()
	body.once.Do(func() {
		body.mu.Lock()
		hashValue := hex.EncodeToString(body.hasher.Sum(nil))
		body.mu.Unlock()
		log.Printf("  upstream audit: request=%s task=%s status=%d response_prefix_hmac_sha256=%s",
			body.requestID, body.taskID, body.statusCode, hashValue)
	})
	return err
}

func wrapAuditedResponse(response *http.Response, requestID, taskID string) *http.Response {
	if response == nil || response.Body == nil {
		return response
	}
	response.Body = &auditedResponseBody{
		body: response.Body, hasher: hmac.New(sha256.New, auditHMACKey), remaining: responseAuditPrefixBytes,
		requestID: requestID, taskID: taskID, statusCode: response.StatusCode,
	}
	return response
}
