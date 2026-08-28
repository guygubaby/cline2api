package main

import (
	"fmt"
	"os"
	"sync"
)

var (
	renameFile       = os.Rename
	directWritePaths sync.Map
)

func writeAndSyncFile(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// writeFileDurably prefers an atomic temp-file replacement. Docker cannot
// rename over a file bind mount, so that path falls back to a synced direct
// write and remembers the limitation for subsequent saves.
func writeFileDurably(path string, data []byte, mode os.FileMode) error {
	if _, direct := directWritePaths.Load(path); direct {
		return writeAndSyncFile(path, data, mode)
	}

	temporaryPath := path + ".tmp"
	if err := writeAndSyncFile(temporaryPath, data, mode); err != nil {
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := renameFile(temporaryPath, path); err == nil {
		return nil
	} else if directErr := writeAndSyncFile(path, data, mode); directErr != nil {
		return fmt.Errorf("replace file: %v; direct-write fallback: %w", err, directErr)
	}

	directWritePaths.Store(path, true)
	_ = os.Remove(temporaryPath)
	return nil
}
