package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestAPIKeyModelRulesAndAliases(t *testing.T) {
	visible := []Model{{ID: "model/a"}, {ID: "model/b"}}
	rules, err := normalizeAPIKeyModelRules([]apiKeyModelRule{{ModelID: "model/a", Alias: "client-a"}}, visible)
	if err != nil {
		t.Fatal(err)
	}
	identity := apiKeyIdentity{ModelRules: rules}
	if got, ok := apiKeyRequestModel(identity, "client-a", visible); !ok || got != "model/a" {
		t.Fatalf("alias resolved to %q, %v", got, ok)
	}
	if got, ok := apiKeyRequestModel(identity, "model/a", visible); !ok || got != "model/a" {
		t.Fatalf("canonical model resolved to %q, %v", got, ok)
	}
	if _, ok := apiKeyRequestModel(identity, "model/b", visible); ok {
		t.Fatal("unselected model was allowed")
	}
	listed := apiKeyListedModels(identity, visible)
	if len(listed) != 1 || listed[0].ID != "client-a" || visible[0].ID != "model/a" {
		t.Fatalf("key model list = %#v; public list = %#v", listed, visible)
	}
	if _, ok := apiKeyRequestModel(identity, "client-a", visible[1:]); ok {
		t.Fatal("globally hidden model was allowed")
	}
	legacy := apiKeyIdentity{}
	if got, ok := apiKeyRequestModel(legacy, "legacy-model", visible); !ok || got != "legacy-model" {
		t.Fatal("legacy key behavior changed")
	}
}

func TestAPIKeyModelRuleValidation(t *testing.T) {
	visible := []Model{{ID: "model/a"}, {ID: "model/b"}}
	for _, rules := range [][]apiKeyModelRule{
		{{ModelID: "missing"}},
		{{ModelID: "model/a"}, {ModelID: "model/a"}},
		{{ModelID: "model/a", Alias: "model/b"}},
		{{ModelID: "model/a", Alias: "bad alias"}},
		{{ModelID: "model/a", Alias: "same"}, {ModelID: "model/b", Alias: "same"}},
	} {
		if _, err := normalizeAPIKeyModelRules(rules, visible); err == nil {
			t.Fatalf("accepted invalid rules: %#v", rules)
		}
	}
}

func TestAPIKeyAllModelsTracksPublicList(t *testing.T) {
	for _, body := range []string{
		`{"name":"all"}`,
		`{"name":"all","modelRules":null}`,
		`{"name":"all","modelRules":[]}`,
	} {
		t.Run(body, func(t *testing.T) {
			var input apiKeyInput
			if err := json.Unmarshal([]byte(body), &input); err != nil {
				t.Fatal(err)
			}
			for _, creating := range []bool{true, false} {
				_, _, _, encoded, err := validateAPIKeyInput(input, creating)
				if err != nil {
					t.Fatal(err)
				}
				if string(encoded) != "null" {
					t.Fatalf("all-model rules = %s, want null", encoded)
				}
				var identity apiKeyIdentity
				if err := json.Unmarshal(encoded, &identity.ModelRules); err != nil {
					t.Fatal(err)
				}
				for _, visible := range [][]Model{
					{{ID: "model/a"}},
					{{ID: "model/a"}, {ID: "model/new"}},
					{{ID: "model/new"}},
					{},
				} {
					if listed := apiKeyListedModels(identity, visible); !slices.Equal(listed, visible) {
						t.Fatalf("all-model list = %#v, want %#v", listed, visible)
					}
				}
			}
		})
	}
}

func TestAPIKeyQuotaBlocksNewGeneration(t *testing.T) {
	quotaM := 1.5
	quota, err := apiKeyQuotaTokens(&quotaM)
	if err != nil || quota == nil || *quota != 1_500_000 {
		t.Fatalf("quota conversion = %v, %v", quota, err)
	}
	request := requestWithTenantScope(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), "sk-test", apiKeyIdentity{ID: "key-1", QuotaTokens: quota, UsedTokens: *quota})
	recorder := httptest.NewRecorder()
	if _, ok := authorizeAPIKeyRequest(recorder, request, "model/a", false, false); ok || recorder.Code != http.StatusTooManyRequests || !strings.Contains(recorder.Body.String(), "quota") {
		t.Fatalf("exhausted key response = %d %s", recorder.Code, recorder.Body.String())
	}
	for _, invalid := range []float64{0, -1, 1_000_001} {
		if _, err := apiKeyQuotaTokens(&invalid); err == nil {
			t.Fatalf("accepted invalid quota: %v", invalid)
		}
	}
}

func TestAPIKeyUsageAccountingPostgres(t *testing.T) {
	url := os.Getenv("CLINE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set CLINE_TEST_DATABASE_URL to run PostgreSQL integration test")
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	schema := "keytest_" + randomHex(6)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	defer db.ExecContext(ctx, `DROP SCHEMA `+schema+` CASCADE`)
	for _, statement := range []string{
		`SET search_path TO ` + schema,
		`CREATE TABLE api_keys (id text PRIMARY KEY, used_tokens bigint NOT NULL DEFAULT 0, request_count bigint NOT NULL DEFAULT 0)`,
		`CREATE TABLE api_key_usage (request_id text PRIMARY KEY, key_id text NOT NULL REFERENCES api_keys(id), tokens bigint NOT NULL, completed boolean NOT NULL, created_at timestamptz NOT NULL DEFAULT now())`,
		`INSERT INTO api_keys(id) VALUES('key-1')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	entries := []RequestLog{
		{ID: "request-1", APIKeyID: "key-1", UsageAvailable: true, TotalTokens: 123, Completed: true},
		{ID: "request-2", APIKeyID: "key-1", EstimatedInputTokens: 40, Completed: true},
		{ID: "request-3", APIKeyID: "key-1", Completed: false},
	}
	for _, entry := range entries {
		for range 2 {
			if err := chargeAPIKeyUsageWithDB(ctx, db, entry); err != nil {
				t.Fatal(err)
			}
		}
	}
	var used, count int64
	if err := db.QueryRowContext(ctx, `SELECT used_tokens,request_count FROM api_keys WHERE id='key-1'`).Scan(&used, &count); err != nil {
		t.Fatal(err)
	}
	if used != 163 || count != 2 {
		t.Fatalf("usage accounting = %d tokens / %d requests, want 163 / 2", used, count)
	}
}
