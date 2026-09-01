package main

import (
	"crypto/sha256"
	"hash/maphash"
	"strings"
	"sync"
	"unicode"
)

const (
	promptEchoErrorCode        = "prompt_echo"
	promptEchoShingleTokens    = 10
	promptEchoRequiredMatches  = 3
	promptEchoMatchSeparation  = 5
	promptEchoMaxSystemTokens  = 50_000
	promptEchoTemplateCacheMax = 8
)

type promptEchoHash [16]byte

// promptEchoGuard retains only hashes of system-prompt token shingles. It can
// detect a model regurgitating protected instructions without retaining the
// prompt itself in request logs or error messages.
type promptEchoGuard struct {
	protected     map[promptEchoHash]struct{}
	window        []string
	current       strings.Builder
	tokenIndex    int
	matchCount    int
	lastMatchAt   int
	lastMatchSeen bool
}

var promptEchoTemplateCache = struct {
	sync.Mutex
	entries map[uint64]map[promptEchoHash]struct{}
	order   []uint64
}{entries: make(map[uint64]map[promptEchoHash]struct{})}

var promptEchoCacheSeed = maphash.MakeSeed()

func promptEchoCacheKey(systemText string) uint64 {
	var hasher maphash.Hash
	hasher.SetSeed(promptEchoCacheSeed)
	hasher.WriteString(systemText)
	return hasher.Sum64()
}

func newPromptEchoGuard(params map[string]any) *promptEchoGuard {
	systemText := systemPromptText(params)
	if len(systemText) < 128 {
		return nil
	}
	cacheKey := promptEchoCacheKey(systemText)
	promptEchoTemplateCache.Lock()
	protected := promptEchoTemplateCache.entries[cacheKey]
	promptEchoTemplateCache.Unlock()
	if protected != nil {
		return &promptEchoGuard{protected: protected, window: make([]string, 0, promptEchoShingleTokens)}
	}

	tokens := strings.Fields(strings.ToLower(systemText))
	if len(tokens) < promptEchoShingleTokens*2 {
		return nil
	}
	if len(tokens) > promptEchoMaxSystemTokens {
		tokens = tokens[:promptEchoMaxSystemTokens]
	}
	protected = make(map[promptEchoHash]struct{}, len(tokens)-promptEchoShingleTokens+1)
	for start := 0; start+promptEchoShingleTokens <= len(tokens); start++ {
		protected[hashPromptEchoTokens(tokens[start:start+promptEchoShingleTokens])] = struct{}{}
	}
	promptEchoTemplateCache.Lock()
	if cached := promptEchoTemplateCache.entries[cacheKey]; cached != nil {
		protected = cached
	} else {
		if len(promptEchoTemplateCache.order) >= promptEchoTemplateCacheMax {
			oldest := promptEchoTemplateCache.order[0]
			delete(promptEchoTemplateCache.entries, oldest)
			promptEchoTemplateCache.order = promptEchoTemplateCache.order[1:]
		}
		promptEchoTemplateCache.entries[cacheKey] = protected
		promptEchoTemplateCache.order = append(promptEchoTemplateCache.order, cacheKey)
	}
	promptEchoTemplateCache.Unlock()
	return &promptEchoGuard{
		protected: protected,
		window:    make([]string, 0, promptEchoShingleTokens),
	}
}

func systemPromptText(params map[string]any) string {
	messages, _ := params["messages"].([]any)
	parts := make([]string, 0, 1)
	for _, rawMessage := range messages {
		message, _ := rawMessage.(map[string]any)
		if message == nil || message["role"] != "system" {
			continue
		}
		if text := messageTextContent(message["content"]); text != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return strings.Join(parts, "\n")
}

func messageTextContent(content any) string {
	if text, ok := content.(string); ok {
		return text
	}
	blocks, _ := content.([]any)
	parts := make([]string, 0, len(blocks))
	for _, rawBlock := range blocks {
		block, _ := rawBlock.(map[string]any)
		if text, _ := block["text"].(string); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func hashPromptEchoTokens(tokens []string) promptEchoHash {
	hasher := sha256.New()
	for _, token := range tokens {
		_, _ = hasher.Write([]byte(token))
		_, _ = hasher.Write([]byte{0})
	}
	sum := hasher.Sum(nil)
	var result promptEchoHash
	copy(result[:], sum[:len(result)])
	return result
}

func (guard *promptEchoGuard) Observe(fragment string) bool {
	if guard == nil || fragment == "" {
		return false
	}
	for _, value := range fragment {
		if unicode.IsSpace(value) {
			if guard.flushToken() {
				return true
			}
			continue
		}
		if guard.current.Len() < 512 {
			guard.current.WriteRune(unicode.ToLower(value))
		}
	}
	return false
}

func (guard *promptEchoGuard) flushToken() bool {
	if guard.current.Len() == 0 {
		return false
	}
	token := guard.current.String()
	guard.current.Reset()
	guard.tokenIndex++
	if len(guard.window) == promptEchoShingleTokens {
		copy(guard.window, guard.window[1:])
		guard.window[len(guard.window)-1] = token
	} else {
		guard.window = append(guard.window, token)
	}
	if len(guard.window) < promptEchoShingleTokens {
		return false
	}
	if _, protected := guard.protected[hashPromptEchoTokens(guard.window)]; !protected {
		return false
	}
	if !guard.lastMatchSeen || guard.tokenIndex-guard.lastMatchAt >= promptEchoMatchSeparation {
		guard.matchCount++
		guard.lastMatchAt = guard.tokenIndex
		guard.lastMatchSeen = true
	}
	return guard.matchCount >= promptEchoRequiredMatches
}

func configurePromptEchoGuard(entry *RequestLog, params map[string]any) {
	if entry != nil {
		entry.promptEchoGuard = newPromptEchoGuard(params)
	}
}

func requestPromptEchoed(entry *RequestLog, fragments ...string) bool {
	if entry == nil || entry.promptEchoGuard == nil {
		return false
	}
	for _, fragment := range fragments {
		if entry.promptEchoGuard.Observe(fragment) {
			return true
		}
	}
	return false
}

func chatResponsePromptEchoed(entry *RequestLog, response map[string]any) bool {
	message, _ := getNested(response, "choices", 0, "message").(map[string]any)
	if message == nil {
		message, _ = getNested(response, "choices", 0, "delta").(map[string]any)
	}
	if message == nil {
		return false
	}
	content, _ := message["content"].(string)
	return requestPromptEchoed(entry, reasoningContent(message), content)
}
