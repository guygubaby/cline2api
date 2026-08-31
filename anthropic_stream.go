package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	semanticEmptyCircuitTTL        = 2 * time.Minute
	semanticEmptyCircuitMaxEntries = 1024
	semanticEmptyErrorCode         = "semantic_empty"
	semanticEmptyClientHint        = "upstream returned no usable text or tool call after retry; compact this conversation with /compact or start a new session with /clear"
)

type semanticStreamDiagnostic struct {
	ReasoningChars int
	FinishReason   string
	Usage          tokenUsage
}

type semanticEmptyCircuitEntry struct {
	diagnostic semanticStreamDiagnostic
	expiresAt  time.Time
}

var (
	semanticEmptyCircuits   = map[string]semanticEmptyCircuitEntry{}
	semanticEmptyCircuitsMu sync.Mutex
)

func pruneSemanticEmptyCircuitsLocked(now time.Time) {
	for key, entry := range semanticEmptyCircuits {
		if !now.Before(entry.expiresAt) {
			delete(semanticEmptyCircuits, key)
		}
	}
}

func semanticRequestFingerprint(params map[string]any) string {
	hash := sha256.New()
	encoder := json.NewEncoder(hash)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(params); err != nil {
		return ""
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func activeSemanticEmptyCircuit(fingerprint string, now time.Time) (semanticStreamDiagnostic, bool) {
	if fingerprint == "" {
		return semanticStreamDiagnostic{}, false
	}
	semanticEmptyCircuitsMu.Lock()
	defer semanticEmptyCircuitsMu.Unlock()

	pruneSemanticEmptyCircuitsLocked(now)
	entry, ok := semanticEmptyCircuits[fingerprint]
	if !ok {
		return semanticStreamDiagnostic{}, false
	}
	return entry.diagnostic, true
}

func rememberSemanticEmptyCircuit(fingerprint string, diagnostic semanticStreamDiagnostic, now time.Time) {
	if fingerprint == "" {
		return
	}
	semanticEmptyCircuitsMu.Lock()
	pruneSemanticEmptyCircuitsLocked(now)
	if len(semanticEmptyCircuits) >= semanticEmptyCircuitMaxEntries {
		var oldestKey string
		var oldestExpiry time.Time
		for key, entry := range semanticEmptyCircuits {
			if oldestKey == "" || entry.expiresAt.Before(oldestExpiry) {
				oldestKey = key
				oldestExpiry = entry.expiresAt
			}
		}
		delete(semanticEmptyCircuits, oldestKey)
	}
	semanticEmptyCircuits[fingerprint] = semanticEmptyCircuitEntry{
		diagnostic: diagnostic,
		expiresAt:  now.Add(semanticEmptyCircuitTTL),
	}
	semanticEmptyCircuitsMu.Unlock()
}

type replayReadCloser struct {
	reader io.Reader
	closer io.Closer
}

func (body *replayReadCloser) Read(buffer []byte) (int, error) {
	return body.reader.Read(buffer)
}

func (body *replayReadCloser) Close() error {
	return body.closer.Close()
}

func proxyThinkingSignature(messageID, thinking string) string {
	digest := sha256.Sum256([]byte(messageID + "\x00" + thinking))
	return "proxy_" + hex.EncodeToString(digest[:])
}

func reasoningContent(message map[string]any) string {
	if message == nil {
		return ""
	}
	if reasoning, _ := message["reasoning_content"].(string); reasoning != "" {
		return reasoning
	}
	reasoning, _ := message["reasoning"].(string)
	return reasoning
}

func inspectSemanticChatEvent(event map[string]any, diagnostic *semanticStreamDiagnostic) (bool, error) {
	event = unwrapDataEnvelope(event)
	if errorPayload := event["error"]; errorPayload != nil {
		return false, streamError(errorPayload)
	}
	if usage := parseTokenUsage(event["usage"]); usage.Valid {
		diagnostic.Usage = mergeTokenUsage(diagnostic.Usage, usage)
	}

	choices, _ := event["choices"].([]any)
	for _, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		if choice == nil {
			continue
		}
		if finishReason, _ := choice["finish_reason"].(string); finishReason != "" {
			diagnostic.FinishReason = finishReason
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			delta, _ = choice["message"].(map[string]any)
		}
		if delta == nil {
			continue
		}
		diagnostic.ReasoningChars += len([]rune(reasoningContent(delta)))
		if content, _ := delta["content"].(string); strings.TrimSpace(content) != "" {
			return true, nil
		}
		if toolCalls, _ := delta["tool_calls"].([]any); len(toolCalls) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// prepareSemanticChatStream buffers only until the upstream produces visible
// content or a tool call. This keeps successful requests streaming while
// allowing reasoning-only/empty streams to be retried before client SSE
// headers are committed.
func prepareSemanticChatStream(response *http.Response) (*http.Response, semanticStreamDiagnostic, error) {
	var diagnostic semanticStreamDiagnostic
	if response == nil || response.Body == nil {
		return nil, diagnostic, fmt.Errorf("upstream response body is missing")
	}

	originalBody := response.Body
	reader := bufio.NewReader(originalBody)
	var replay bytes.Buffer
	for {
		line, readErr := reader.ReadString('\n')
		if line != "" {
			replay.WriteString(line)
			trimmed := strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(trimmed, "data:") {
				payload := strings.TrimSpace(trimmed[5:])
				if payload == "[DONE]" {
					_ = originalBody.Close()
					return nil, diagnostic, errEmptyResponseContent
				}
				if payload != "" {
					var event map[string]any
					if err := json.Unmarshal([]byte(payload), &event); err != nil {
						_ = originalBody.Close()
						return nil, diagnostic, fmt.Errorf("decode upstream SSE event: %w", err)
					}
					semantic, err := inspectSemanticChatEvent(event, &diagnostic)
					if err != nil {
						_ = originalBody.Close()
						return nil, diagnostic, err
					}
					if semantic {
						response.Body = &replayReadCloser{
							reader: io.MultiReader(bytes.NewReader(replay.Bytes()), reader),
							closer: originalBody,
						}
						return response, diagnostic, nil
					}
				}
			}
		}
		if readErr != nil {
			_ = originalBody.Close()
			if errors.Is(readErr, io.EOF) {
				return nil, diagnostic, errEmptyResponseContent
			}
			return nil, diagnostic, fmt.Errorf("read upstream SSE: %w", readErr)
		}
	}
}

func callClineAnthropicStream(params map[string]any) (*http.Response, *Account, semanticStreamDiagnostic, error) {
	response, account, err := callClineAPI(params, true)
	if err != nil {
		return nil, account, semanticStreamDiagnostic{}, err
	}
	prepared, diagnostic, prepareErr := prepareSemanticChatStream(response)
	if prepareErr == nil {
		return prepared, account, diagnostic, nil
	}
	if !isEmptyResponseError(prepareErr) {
		return nil, account, diagnostic, prepareErr
	}

	model, _ := params["model"].(string)
	alternative := pickAlternativeAccountForModel(model, account)
	if alternative == nil {
		return nil, account, diagnostic, prepareErr
	}
	log.Printf("  anthropic empty stream: finish=%s reasoning_chars=%d thinking_tokens=%d; retrying once with another account",
		diagnostic.FinishReason, diagnostic.ReasoningChars, diagnostic.Usage.Reasoning)

	retryResponse, retryAccount, retryErr := callClineAPIWithAccount(alternative, params, true)
	if retryErr != nil {
		return nil, retryAccount, diagnostic, retryErr
	}
	retryPrepared, retryDiagnostic, retryPrepareErr := prepareSemanticChatStream(retryResponse)
	if retryPrepareErr != nil {
		return nil, retryAccount, retryDiagnostic, retryPrepareErr
	}
	return retryPrepared, retryAccount, retryDiagnostic, nil
}
