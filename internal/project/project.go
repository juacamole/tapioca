// Package project loads per-worktree context for the system prompt:
// committed instruction files (AGENTS.md, TAPIOCA.md) and personal memory
// added via /remember. Memory lives in the data dir keyed by worktree path —
// nothing is ever written into the project itself.
package project

import (
	"crypto/sha1"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"tapioca/internal/config"
	"tapioca/internal/textenc"
)

const (
	maxInstructions = 80_000
	maxMemory       = 40_000
)

var instructionFiles = []string{"AGENTS.md", "TAPIOCA.md"}

// Instructions returns the concatenated instruction files found in cwd.
func Instructions(cwd string) string {
	var parts []string
	for _, name := range instructionFiles {
		data, err := os.ReadFile(filepath.Join(cwd, name))
		if err != nil {
			continue
		}
		text, ok := textenc.Decode(data)
		if !ok || strings.TrimSpace(text) == "" {
			continue
		}
		if len(text) > maxInstructions {
			text = text[:maxInstructions] + "\n[truncated]"
		}
		parts = append(parts, fmt.Sprintf("[%s]\n%s", name, strings.TrimSpace(text)))
	}
	return strings.Join(parts, "\n\n")
}

// MemoryPath returns where /remember facts for cwd are stored.
func MemoryPath(cwd string) string {
	sum := sha1.Sum([]byte(cwd))
	return filepath.Join(config.DataDir(), "memory", fmt.Sprintf("%x.md", sum[:8]))
}

// Memory returns the remembered facts for cwd, if any.
func Memory(cwd string) string {
	data, err := os.ReadFile(MemoryPath(cwd))
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(data))
	if len(s) > maxMemory {
		s = s[len(s)-maxMemory:]
	}
	return s
}

// Remember appends a fact to cwd's memory.
func Remember(cwd, fact string) error {
	p := MemoryPath(cwd)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "- %s\n", strings.TrimSpace(fact))
	return err
}

// ClearMemory removes cwd's memory file.
func ClearMemory(cwd string) error {
	err := os.Remove(MemoryPath(cwd))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
