package main

import (
	"fmt"
	"os"
	"sync"
	"time"
)

var (
	renameFile         = os.Rename
	directWritePaths   sync.Map
	deferredWritesMu   sync.Mutex
	deferredWrites     = map[string]deferredWrite{}
	deferredWriteWake  = make(chan struct{}, 1)
	deferredWriterOnce sync.Once
)

const deferredWriteDelay = 250 * time.Millisecond

type deferredWrite struct {
	data []byte
	mode os.FileMode
}

func startDeferredWriter() {
	deferredWriterOnce.Do(func() {
		go func() {
			for range deferredWriteWake {
				time.Sleep(deferredWriteDelay)
				flushDeferredWrites()
			}
		}()
	})
}

func queueDurableWrite(path string, data []byte, mode os.FileMode) {
	startDeferredWriter()
	deferredWritesMu.Lock()
	deferredWrites[path] = deferredWrite{data: append([]byte(nil), data...), mode: mode}
	deferredWritesMu.Unlock()
	select {
	case deferredWriteWake <- struct{}{}:
	default:
	}
}

func flushDeferredWrites() {
	deferredWritesMu.Lock()
	writes := deferredWrites
	deferredWrites = map[string]deferredWrite{}
	deferredWritesMu.Unlock()
	for path, write := range writes {
		if err := writeFileDurably(path, write.data, write.mode); err != nil {
			fmt.Printf("deferred write failed for %s: %v\n", path, err)
		}
	}
}

func flushRuntimeState() {
	flushDeferredWrites()
	savePool()
	flushRequestLogs(requestLogsPath)
}

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
