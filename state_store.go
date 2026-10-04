package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"time"
)

// ponytail: JSON snapshots assume one proxy writer per database; use row-level
// entities or optimistic versions before running multiple replicas.
var stateDB *sql.DB

type legacyStateFile struct {
	name     string
	path     string
	fallback any
}

func legacyStateFiles() []legacyStateFile {
	return []legacyStateFile{
		{name: "pool", path: poolPath, fallback: &AccountPool{Accounts: []*Account{}, Models: []Model{}}},
		{name: "request_logs", path: requestLogsPath, fallback: &[]RequestLog{}},
		{name: "zen_config", path: resolveDataPath(".cline-zen.json"), fallback: defaultZenConfig()},
		{name: "custom_providers", path: customProvidersPath, fallback: defaultCustomProviderStore()},
		{name: "proxy_config", path: proxyConfigPath, fallback: defaultProxyConfig()},
		{name: "cline_proxy", path: clineProxyConfigPath(), fallback: defaultClineProxyConfig()},
		{name: "credentials", path: credentialsPath, fallback: &credentials{}},
	}
}

func stateFileForPath(path string) (legacyStateFile, bool) {
	for _, file := range legacyStateFiles() {
		if file.path == path {
			return file, true
		}
	}
	return legacyStateFile{}, false
}

func legacyStateJSON(file legacyStateFile) ([]byte, error) {
	data, err := os.ReadFile(file.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read legacy %s: %w", file.name, err)
	}
	trimmed := bytes.TrimSpace(data)
	if errors.Is(err, os.ErrNotExist) || len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return json.Marshal(file.fallback)
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("legacy %s is invalid JSON: %s", file.name, file.path)
	}
	if err := validateStateJSON(file, data); err != nil {
		return nil, err
	}
	return data, nil
}

func validateStateJSON(file legacyStateFile, data []byte) error {
	trimmed := bytes.TrimSpace(data)
	want := byte('{')
	if file.name == "request_logs" {
		want = '['
	}
	if len(trimmed) == 0 || trimmed[0] != want {
		return fmt.Errorf("%s must be a JSON %s: %s", file.name, map[byte]string{'{': "object", '[': "array"}[want], file.path)
	}
	value := reflect.New(reflect.TypeOf(file.fallback).Elem()).Interface()
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("decode %s: %w", file.name, err)
	}
	return nil
}

func migrateLegacyState(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(221165031)`); err != nil {
		return err
	}
	for _, file := range legacyStateFiles() {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM app_state WHERE name=$1)`, file.name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		data, err := legacyStateJSON(file)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO app_state(name,data) VALUES($1,$2::jsonb)`, file.name, string(data)); err != nil {
			return fmt.Errorf("migrate legacy %s: %w", file.name, err)
		}
	}
	return tx.Commit()
}

func readStateFile(path string) ([]byte, error) {
	file, managed := stateFileForPath(path)
	if !managed || stateDB == nil {
		return os.ReadFile(path)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var data []byte
	if err := stateDB.QueryRowContext(ctx, `SELECT data::text FROM app_state WHERE name=$1`, file.name).Scan(&data); err != nil {
		return nil, fmt.Errorf("read %s from database: %w", file.name, err)
	}
	return data, nil
}

func writeStateFile(path string, data []byte) (bool, error) {
	file, managed := stateFileForPath(path)
	if !managed || stateDB == nil {
		return false, nil
	}
	if !json.Valid(data) {
		return true, fmt.Errorf("invalid JSON for %s", file.name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := stateDB.ExecContext(ctx, `INSERT INTO app_state(name,data,updated_at) VALUES($1,$2::jsonb,now()) ON CONFLICT(name) DO UPDATE SET data=excluded.data,updated_at=now()`, file.name, string(data))
	if err != nil {
		return true, fmt.Errorf("save %s to database: %w", file.name, err)
	}
	return true, nil
}

func activateStateStore(db *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dataByName := make(map[string][]byte, len(legacyStateFiles()))
	for _, file := range legacyStateFiles() {
		var data []byte
		err := db.QueryRowContext(ctx, `SELECT data::text FROM app_state WHERE name=$1`, file.name).Scan(&data)
		if err == nil {
			err = validateStateJSON(file, data)
		}
		if err != nil {
			return fmt.Errorf("load database state %s: %w", file.name, err)
		}
		dataByName[file.name] = data
	}

	var loadedPool AccountPool
	if err := json.Unmarshal(dataByName["pool"], &loadedPool); err != nil {
		return err
	}
	if loadedPool.Accounts == nil {
		loadedPool.Accounts = []*Account{}
	}
	if loadedPool.Keys == nil {
		loadedPool.Keys = []string{}
	}
	if loadedPool.Models == nil {
		loadedPool.Models = []Model{}
	}
	poolMu.Lock()
	pool = &loadedPool
	poolMu.Unlock()

	var loadedLogs []RequestLog
	if err := json.Unmarshal(dataByName["request_logs"], &loadedLogs); err != nil {
		return err
	}
	requestLogsMu.Lock()
	requestLogs = pruneRequestLogsLocked(loadedLogs)
	requestLogsMu.Unlock()

	loadedProxy := defaultProxyConfig()
	var persistedProxy proxyConfigData
	if err := json.Unmarshal(dataByName["proxy_config"], &persistedProxy); err != nil {
		return err
	}
	if validLoadBalancingStrategy(persistedProxy.Strategy) {
		loadedProxy.Strategy = persistedProxy.Strategy
	}
	for key, value := range persistedProxy.Headers {
		loadedProxy.Headers[key] = value
	}
	proxyConfigMu.Lock()
	proxyConfig = loadedProxy
	proxyConfigMu.Unlock()

	loadedZen := defaultZenConfig()
	if err := json.Unmarshal(dataByName["zen_config"], loadedZen); err != nil {
		return err
	}
	if loadedZen.Key == "" {
		loadedZen.Key = "public"
	}
	if loadedZen.BaseURL == "" {
		loadedZen.BaseURL = zenAPIBase
	}
	if loadedZen.ProxyStrategy != "random" && loadedZen.ProxyStrategy != "fill" {
		loadedZen.ProxyStrategy = "round_robin"
	}
	zenConfigMu.Lock()
	zenConfig = loadedZen
	zenConfigMu.Unlock()

	loadedClineProxy := defaultClineProxyConfig()
	if err := json.Unmarshal(dataByName["cline_proxy"], loadedClineProxy); err != nil {
		return err
	}
	normalizeClineProxyConfig(loadedClineProxy)
	clineProxyConfigMu.Lock()
	clineProxyConfig = loadedClineProxy
	clineProxyConfigMu.Unlock()

	loadedProviders := defaultCustomProviderStore()
	if err := json.Unmarshal(dataByName["custom_providers"], loadedProviders); err != nil {
		return err
	}
	normalizeCustomProviderStore(loadedProviders)
	customProviderMu.Lock()
	customProviderStore = loadedProviders
	rebuildCustomProviderIndexLocked()
	customProviderMu.Unlock()
	stateDB = db
	return nil
}
