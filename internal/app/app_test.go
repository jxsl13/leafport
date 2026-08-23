package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"leafport/internal/library"
)

func TestPrepareTargetDirectory(t *testing.T) {
	target := filepath.Join(t.TempDir(), "nested", "exports")
	absolute, err := prepareTargetDirectory(target)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(absolute); err != nil || !info.IsDir() {
		t.Fatalf("target was not created: %v", err)
	}
}

func TestRunBatchSkipsExistingWithoutWorkDirectory(t *testing.T) {
	target := t.TempDir()
	book := library.Book{ID: "B000000001", Title: "Existing Book"}
	output := library.PlanOutputs([]library.Book{book}, target)[0].Path
	if err := os.WriteFile(output, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := runBatch(context.Background(), Config{}, Streams{
		Stdout: &stdout, Stderr: &stderr,
	}, target, []library.Book{book})
	if err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(target, ".leafport-work-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("unexpected work directories: %v", matches)
	}
}
