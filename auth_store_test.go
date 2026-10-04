package main

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestNewAPIKeyUsesUUIDAndStoredHash(t *testing.T) {
	key, err := newAPIKey()
	if err != nil || !strings.HasPrefix(key, "sk-") {
		t.Fatalf("API key generation: %q, %v", key, err)
	}
	id, err := uuid.Parse(strings.TrimPrefix(key, "sk-"))
	if err != nil || id.Version() != 4 {
		t.Fatalf("API key is not a UUIDv4: %q, %v", key, err)
	}
	if tokenHash(key) == key || tokenHash(key) == tokenHash(key+"x") {
		t.Fatal("API key hash is unsafe")
	}
}
