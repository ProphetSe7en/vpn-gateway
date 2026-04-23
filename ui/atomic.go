package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
)

// atomicWriteFile writes data to path atomically via a random-suffixed
// temp file + rename. Mirrors the auth package's sessions-file pattern —
// two concurrent writers cannot truncate each other's tmp file because
// each gets its own random suffix. Mode is applied via both WriteFile and
// an explicit Chmod: umask can strip bits from WriteFile's mode arg (e.g.
// 0077 umask turns 0644 into 0600), so Chmod restores the intent.
//
// Chmod failure is logged but non-fatal — the data is already written,
// and refusing to rename would lose it. Callers relying on strict mode
// enforcement should verify via os.Stat.
//
// On rename failure the tmp file is best-effort removed to avoid leaving
// stale half-written files lying around.
func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return fmt.Errorf("atomic: random suffix: %w", err)
	}
	tmp := path + ".tmp." + hex.EncodeToString(suffix)
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return fmt.Errorf("atomic: write tmp: %w", err)
	}
	if err := os.Chmod(tmp, mode); err != nil {
		log.Printf("atomic: chmod %q: %v (proceeding with rename; data preserved)", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("atomic: rename: %w", err)
	}
	return nil
}
