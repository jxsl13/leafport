package exporter

import (
	"bytes"
	"os"
	"testing"
)

func TestFilterRuntimeDiagnostics(t *testing.T) {
	input := "objc[12]: duplicate class warning\n" +
		"2026 [BugsnagPerformance] noise\n" +
		"FastMetricsClientMobile: noise.leafport: useful failure\n" +
		"unexpected diagnostic\n"
	got := filterRuntimeDiagnostics(input)
	want := "leafport: useful failure\nunexpected diagnostic\n"
	if got != want {
		t.Fatalf("filtered diagnostics = %q, want %q", got, want)
	}
}

func TestArchiveSpillWriterKeepsBoundaryInMemory(t *testing.T) {
	writer := &archiveSpillWriter{limit: 8, workRoot: t.TempDir()}
	if _, err := writer.Write([]byte("12345678")); err != nil {
		t.Fatal(err)
	}
	archive, err := writer.finish()
	if err != nil {
		t.Fatal(err)
	}
	if archive.Path != "" || archive.Size != 8 || !bytes.Equal(archive.Data, []byte("12345678")) {
		t.Fatalf("unexpected in-memory archive: %+v", archive)
	}
}

func TestArchiveSpillWriterUsesPrivateFileAboveLimit(t *testing.T) {
	writer := &archiveSpillWriter{limit: 4, workRoot: t.TempDir()}
	for _, value := range []string{"1234", "5678"} {
		if _, err := writer.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	archive, err := writer.finish()
	if err != nil {
		t.Fatal(err)
	}
	if archive.Path == "" || archive.Data != nil || archive.Size != 8 {
		t.Fatalf("unexpected spilled archive: %+v", archive)
	}
	data, err := os.ReadFile(archive.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, []byte("12345678")) {
		t.Fatalf("spilled data = %q", data)
	}
	info, err := os.Stat(archive.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("spill permissions = %o", info.Mode().Perm())
	}
}
