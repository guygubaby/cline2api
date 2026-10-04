package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyStateJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.json")
	file := legacyStateFile{name: "credentials", path: path, fallback: &credentials{}}

	data, err := legacyStateJSON(file)
	if err != nil || string(data) != `{"refreshToken":""}` {
		t.Fatalf("missing file should use defaults: %q, %v", data, err)
	}

	for _, test := range []struct {
		name    string
		content string
		wantErr string
	}{
		{name: "existing", content: `{"refreshToken":"legacy-token","extra":true}`},
		{name: "empty", content: "  \n", wantErr: ""},
		{name: "null", content: "null"},
		{name: "malformed", content: `{"refreshToken":`, wantErr: "invalid JSON"},
		{name: "wrong shape", content: `[]`, wantErr: "JSON object"},
		{name: "wrong field type", content: `{"refreshToken":42}`, wantErr: "decode credentials"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(test.content), 0600); err != nil {
				t.Fatal(err)
			}
			data, err := legacyStateJSON(file)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.name == "existing" && string(data) != test.content {
				t.Fatalf("existing JSON was changed: %s", data)
			}
		})
	}
}

func TestOverrideStaysAFile(t *testing.T) {
	for _, file := range legacyStateFiles() {
		if file.name == "override" || filepath.Base(file.path) == "override.md" {
			t.Fatal("override.md must not be migrated")
		}
	}
}
