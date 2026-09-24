// Package runlog keeps a command's full output in a temporary file, so it
// can be saved when the command fails and thrown away when it succeeds.
package runlog

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Log struct {
	f *os.File
}

// Start creates the temporary log in tempDir ("" means os.TempDir()).
func Start(tempDir string) (*Log, error) {
	f, err := os.CreateTemp(tempDir, "nixzner-*.log")
	if err != nil {
		return nil, err
	}
	return &Log{f: f}, nil
}

func (l *Log) Write(p []byte) (int, error) { return l.f.Write(p) }

// Name is the temporary log's path.
func (l *Log) Name() string { return l.f.Name() }

// Discard closes and deletes the temporary log.
func (l *Log) Discard() {
	l.f.Close()
	os.Remove(l.f.Name())
}

// Persist closes the temporary log and moves it to dir as
// <name>-<UTC timestamp>.log, returning the new path.
func (l *Log) Persist(dir, name string, now time.Time) (string, error) {
	if err := l.f.Close(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	data, err := os.ReadFile(l.f.Name())
	if err != nil {
		return "", err
	}
	dst := filepath.Join(dir, fmt.Sprintf("%s-%s.log", name, now.UTC().Format("20060102T150405Z")))
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return "", err
	}
	os.Remove(l.f.Name())
	return dst, nil
}
