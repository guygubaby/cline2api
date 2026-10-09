package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// A nil rule list allows all models and follows the current public model list.
type apiKeyModelRule struct {
	ModelID string `json:"modelId"`
	Alias   string `json:"alias"`
}

var apiKeyAliasPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,127}$`)

func normalizeAPIKeyModelRules(rules []apiKeyModelRule, visible []Model) ([]apiKeyModelRule, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	visibleIDs := make(map[string]bool, len(visible))
	for _, model := range visible {
		visibleIDs[model.ID] = true
	}
	selected := make(map[string]bool, len(rules))
	aliases := make(map[string]bool, len(rules))
	normalized := make([]apiKeyModelRule, 0, len(rules))
	for _, rule := range rules {
		modelID := strings.TrimSpace(rule.ModelID)
		alias := strings.TrimSpace(rule.Alias)
		if !visibleIDs[modelID] {
			return nil, fmt.Errorf("model %q is not in the public model list", modelID)
		}
		if selected[modelID] {
			return nil, fmt.Errorf("model %q is selected more than once", modelID)
		}
		selected[modelID] = true
		if alias == modelID {
			alias = ""
		}
		if alias != "" {
			if !apiKeyAliasPattern.MatchString(alias) {
				return nil, fmt.Errorf("invalid alias %q", alias)
			}
			if visibleIDs[alias] || aliases[alias] {
				return nil, fmt.Errorf("alias %q conflicts with another model", alias)
			}
			aliases[alias] = true
		}
		normalized = append(normalized, apiKeyModelRule{ModelID: modelID, Alias: alias})
	}
	return normalized, nil
}

func apiKeyQuotaTokens(quotaM *float64) (*int64, error) {
	if quotaM == nil {
		return nil, nil
	}
	if math.IsNaN(*quotaM) || math.IsInf(*quotaM, 0) || *quotaM < 0.000001 || *quotaM > 1_000_000 {
		return nil, errors.New("quotaM must be between 0.000001 and 1000000 M")
	}
	tokens := int64(math.Round(*quotaM * 1_000_000))
	return &tokens, nil
}

func apiKeyRequestModel(identity apiKeyIdentity, requested string, visible []Model) (string, bool) {
	if identity.ModelRules == nil {
		return requested, true
	}
	visibleIDs := make(map[string]bool, len(visible))
	for _, model := range visible {
		visibleIDs[model.ID] = true
	}
	for _, rule := range identity.ModelRules {
		if (requested == rule.ModelID || rule.Alias != "" && requested == rule.Alias) && visibleIDs[rule.ModelID] {
			return rule.ModelID, true
		}
	}
	return "", false
}

func apiKeyListedModels(identity apiKeyIdentity, visible []Model) []Model {
	if identity.ModelRules == nil {
		return visible
	}
	rules := make(map[string]apiKeyModelRule, len(identity.ModelRules))
	for _, rule := range identity.ModelRules {
		rules[rule.ModelID] = rule
	}
	listed := make([]Model, 0, len(identity.ModelRules))
	for _, model := range visible {
		if rule, ok := rules[model.ID]; ok {
			if rule.Alias != "" {
				model.ID = rule.Alias
			}
			listed = append(listed, model)
		}
	}
	return listed
}

func authorizeAPIKeyRequest(w http.ResponseWriter, r *http.Request, model string, anthropic bool, countTokens bool) (string, bool) {
	identity := requestAPIKeyIdentity(r)
	if !countTokens && identity.QuotaTokens != nil && identity.UsedTokens >= *identity.QuotaTokens {
		if anthropic {
			writeAnthropicError(w, http.StatusTooManyRequests, "rate_limit_error", "API key token quota exhausted")
		} else {
			writeOpenAIError(w, http.StatusTooManyRequests, "insufficient_quota", "API key token quota exhausted")
		}
		return "", false
	}
	canonical, ok := apiKeyRequestModel(identity, model, getListedModels())
	if !ok {
		if anthropic {
			writeAnthropicError(w, http.StatusForbidden, "permission_error", "model is not allowed for this API key")
		} else {
			writeOpenAIError(w, http.StatusForbidden, "permission_error", "model is not allowed for this API key")
		}
		return "", false
	}
	return canonical, true
}

func chargeAPIKeyUsage(entry RequestLog) {
	if authDB == nil || entry.APIKeyID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := chargeAPIKeyUsageWithDB(ctx, authDB, entry); err != nil {
		log.Printf("Failed to account API key usage for request %s: %v", entry.ID, err)
	}
}

func chargeAPIKeyUsageWithDB(ctx context.Context, db *sql.DB, entry RequestLog) error {
	tokens := int64(0)
	if entry.UsageAvailable {
		tokens = entry.TotalTokens
	} else if entry.Completed && entry.EstimatedInputTokens > 0 {
		// Some upstreams omit usage; account for at least the estimated input.
		tokens = int64(entry.EstimatedInputTokens)
	}
	if tokens < 0 {
		tokens = 0
	}
	if tokens == 0 {
		return nil
	}
	_, err := db.ExecContext(ctx, `WITH charged AS (
		INSERT INTO api_key_usage(request_id,key_id,tokens,completed)
		SELECT $1,$2,$3,$4 WHERE EXISTS(SELECT 1 FROM api_keys WHERE id=$2)
		ON CONFLICT(request_id) DO NOTHING
		RETURNING key_id,tokens
	) UPDATE api_keys SET used_tokens=api_keys.used_tokens+charged.tokens,
		request_count=api_keys.request_count+1 FROM charged WHERE api_keys.id=charged.key_id`,
		entry.ID, entry.APIKeyID, tokens, entry.Completed)
	return err
}

func backfillAPIKeyUsage(ctx context.Context, db *sql.DB) error {
	requestLogsMu.Lock()
	entries := append([]RequestLog(nil), requestLogs...)
	requestLogsMu.Unlock()
	for _, entry := range entries {
		if entry.APIKeyID == "" {
			continue
		}
		if err := chargeAPIKeyUsageWithDB(ctx, db, entry); err != nil {
			return fmt.Errorf("backfill API key usage: %w", err)
		}
	}
	return nil
}

func encodeAPIKeyModelRules(rules []apiKeyModelRule) ([]byte, error) {
	return json.Marshal(rules)
}
