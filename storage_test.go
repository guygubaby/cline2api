package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWriteFileDurablyFallsBackWhenBindMountRejectsRename(t *testing.T) {
	target := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(target, []byte("old"), 0600); err != nil {
		t.Fatalf("write initial target: %v", err)
	}

	originalRename := renameFile
	renameFile = func(_, _ string) error { return syscall.EBUSY }
	t.Cleanup(func() { renameFile = originalRename })

	if err := writeFileDurably(target, []byte("new"), 0600); err != nil {
		t.Fatalf("durable write: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(data) != "new" {
		t.Fatalf("target content = %q, want new", data)
	}
	if _, err := os.Stat(target + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary file was not cleaned up: %v", err)
	}
}

func TestDeferredDurableWriteCoalescesLatestValue(t *testing.T) {
	target := filepath.Join(t.TempDir(), "deferred.json")
	queueDurableWrite(target, []byte("first"), 0600)
	queueDurableWrite(target, []byte("latest"), 0600)
	flushDeferredWrites()

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read deferred write: %v", err)
	}
	if string(data) != "latest" {
		t.Fatalf("deferred write = %q, want latest", data)
	}
}
