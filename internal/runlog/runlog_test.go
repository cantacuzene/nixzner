package runlog

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPersistMovesLogIntoDir(t *testing.T) {
	l, err := Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(l, "step output")
	dir := filepath.Join(t.TempDir(), "logs")
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	path, err := l.Persist(dir, "web1", now)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "web1-20260924T100000Z.log"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if data, _ := os.ReadFile(path); string(data) != "step output" {
		t.Errorf("log = %q", data)
	}
	if _, err := os.Stat(l.f.Name()); !os.IsNotExist(err) {
		t.Errorf("temporary log still exists: %v", err)
	}
}

func TestDiscardRemovesTemporaryLog(t *testing.T) {
	tmp := t.TempDir()
	l, err := Start(tmp)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(l, "x")
	l.Discard()
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Errorf("temp dir not empty after Discard: %v", entries)
	}
}
