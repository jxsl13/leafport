package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"leafport/internal/exporter"
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
	output := library.PlanOutputs([]library.Book{book}, target)[0].Path + ".epub"
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
	wantParts := []string{
		"[1/1] Existing Book",
		"        ID       B000000001",
		"        Status   skipped (already exists)",
		"        Output   " + string(filepath.Separator),
		"Existing Book.epub",
		"Summary: 0 completed · 1 skipped · 0 failed",
	}
	for _, want := range wantParts {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout does not contain %q:\n%s", want, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "Skipped existing:") {
		t.Fatalf("stdout contains legacy one-line output: %q", stdout.String())
	}
}

func TestRunBatchDoesNotAcceptExistingOutputAsPrivacyClean(t *testing.T) {
	target := t.TempDir()
	book := library.Book{ID: "B000000001", Title: "Existing Book"}
	output := library.PlanOutputs([]library.Book{book}, target)[0].Path + ".epub"
	if err := os.WriteFile(output, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := runBatch(context.Background(), Config{RedactPersonal: true}, Streams{
		Stdout: &stdout, Stderr: &stderr,
	}, target, []library.Book{book})
	if err == nil || !strings.Contains(stdout.String(), "privacy cleanup was not applied") {
		t.Fatalf("err = %v, stdout = %q", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "        Status   failed") ||
		!strings.Contains(stdout.String(), "        Reason   privacy cleanup") {
		t.Fatalf("failure is not structured as a book block: %q", stdout.String())
	}
}

func TestBatchReporterWrapsLongBookInformation(t *testing.T) {
	var output bytes.Buffer
	reporter := batchReporter{output: &output}
	reporter.begin(12, 13,
		"Artificial Intelligence: A Very Long Book Title That Must Stay Visually Separate From Its Details Even On A Narrow Terminal",
		"B012345678")
	reporter.field("Status", "failed")
	reporter.field("Reason", "account secret required; no verified raw credential is available; "+
		"this deliberately long explanation must continue on an aligned line instead of becoming one unwieldy status line")
	reporter.field("Output", "/Users/example/Documents/exports/"+strings.Repeat("very-long-directory-name/", 5)+"book.epub")
	reporter.finish()

	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	for _, line := range lines {
		if width := utf8.RuneCountInString(line); width > batchOutputWidth {
			t.Errorf("line has %d runes, want <= %d: %q", width, batchOutputWidth, line)
		}
	}
	if !strings.HasPrefix(lines[0], "[12/13] Artificial Intelligence") {
		t.Fatalf("book header = %q", lines[0])
	}
	wantParts := []string{
		"\n        ID       B012345678\n",
		"\n        Status   failed\n",
		"\n        Reason   account secret required",
		"\n                 deliberately long explanation",
	}
	for _, want := range wantParts {
		if !strings.Contains(output.String(), want) {
			t.Errorf("output does not contain %q:\n%s", want, output.String())
		}
	}
}

func TestDebugArtifactHelpersPreservePrivateCopies(t *testing.T) {
	target := t.TempDir()
	root, err := prepareDebugRoot(target, time.Date(2026, 8, 23, 12, 0, 0, 1, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(root) != filepath.Join(target, "debug") {
		t.Fatalf("debug root = %q", root)
	}
	if info, err := os.Stat(root); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("debug root permissions = %v, err = %v", info, err)
	}

	source := filepath.Join(t.TempDir(), "book")
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := []byte("encrypted KFX")
	if err := os.WriteFile(filepath.Join(source, "nested", "book.azw"), want, 0o644); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "B000000001", "encrypted")
	if err := preserveEncryptedBundle(source, destination); err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(destination, "nested", "book.azw")
	got, err := os.ReadFile(copied)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("copied data = %q", got)
	}
	if info, err := os.Stat(copied); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("copied permissions = %v, err = %v", info, err)
	}

	archive := filepath.Join(root, "B000000001", "B000000001.kfx-zip")
	if err := preserveDecryptedArchive(archive, exporter.Archive{Data: []byte("decrypted")}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(archive); err != nil || string(data) != "decrypted" {
		t.Fatalf("decrypted archive = %q, err = %v", data, err)
	}
}

func TestWriteBookListCompactAndWide(t *testing.T) {
	book := library.Book{
		ID: "B000000001", Title: strings.Repeat("Langer Titel ", 4),
		Modified: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC), Path: "/library/B000000001",
	}
	var compact bytes.Buffer
	if err := writeBookList(&compact, []library.Book{book}, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(compact.String(), "MODIFIED") || strings.Contains(compact.String(), "PATH") ||
		strings.Contains(compact.String(), book.Path) || !strings.Contains(compact.String(), book.Title) {
		t.Fatalf("compact table = %q", compact.String())
	}

	var wide bytes.Buffer
	if err := writeBookList(&wide, []library.Book{book}, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wide.String(), "MODIFIED") || !strings.Contains(wide.String(), "PATH") ||
		!strings.Contains(wide.String(), book.Title) || !strings.Contains(wide.String(), book.Path) {
		t.Fatalf("wide table = %q", wide.String())
	}
	header := strings.SplitN(wide.String(), "\n", 2)[0]
	if !(strings.Index(header, "ID") < strings.Index(header, "MODIFIED") &&
		strings.Index(header, "MODIFIED") < strings.Index(header, "TITLE") &&
		strings.Index(header, "TITLE") < strings.Index(header, "PATH")) {
		t.Fatalf("wide column order = %q", header)
	}
}
