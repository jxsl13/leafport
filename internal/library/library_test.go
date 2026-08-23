package library

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
)

func TestDiscoverAndSelectBooks(t *testing.T) {
	root := t.TempDir()
	older := createBundle(t, root, "ASIN-OLD", "uuid-old")
	newer := createBundle(t, root, "ASIN-NEW", "uuid-new")
	oldTime := time.Now().Add(-time.Hour)
	newTime := time.Now()
	if err := os.Chtimes(filepath.Join(older, "main.azw8"), oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(newer, "main.azw8"), newTime, newTime); err != nil {
		t.Fatal(err)
	}

	books, _, err := DiscoverBooks(context.Background(), []Root{{Path: root}})
	if err != nil {
		t.Fatal(err)
	}
	if len(books) != 2 {
		t.Fatalf("found %d books, want 2", len(books))
	}
	if books[0].ID != "ASIN-NEW" {
		t.Fatalf("selected %q, want newest ASIN-NEW", books[0].ID)
	}
	selected, err := Filter(books, `(?i)^ASIN-OLD$`)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0].Path != older {
		t.Fatalf("selected books %+v, want %q", selected, older)
	}
}

func TestIncompleteBundleIsIgnored(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "ASIN", "uuid")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "main.azw8"), []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	books, _, err := DiscoverBooks(context.Background(), []Root{{Path: root}})
	if err != nil {
		t.Fatal(err)
	}
	if len(books) != 0 {
		t.Fatalf("found %d books, want none", len(books))
	}
}

func TestSafeName(t *testing.T) {
	if got := SafeName("B0/ CJ:123"); got != "B0CJ123" {
		t.Fatalf("safeName = %q", got)
	}
}

func TestSafeFilename(t *testing.T) {
	got := SafeFilename("  Künstliche  Intelligenz: Ein/Ansatz  ")
	if got != "Künstliche Intelligenz - Ein - Ansatz" {
		t.Fatalf("safeFilename = %q", got)
	}
}

func TestFilterBooks(t *testing.T) {
	books := []Book{
		{ID: "B000000001", Title: "Alpha und Omega"},
		{ID: "B000000002", Title: "Künstliche Intelligenz"},
	}
	all, err := Filter(books, ".*")
	if err != nil || len(all) != 2 {
		t.Fatalf("unfiltered books = %+v, %v", all, err)
	}
	matched, err := Filter(books, `(?i)künstliche|B000000001`)
	if err != nil {
		t.Fatal(err)
	}
	if len(matched) != 2 {
		t.Fatalf("matched %d books, want 2", len(matched))
	}
	if _, err := Filter(books, "["); err == nil {
		t.Fatal("invalid regular expression was accepted")
	}
	if _, err := Filter(books, "does-not-match"); err == nil {
		t.Fatal("empty match set was accepted")
	}
}

func TestPlanExportsDisambiguatesDuplicateTitles(t *testing.T) {
	books := []Book{
		{ID: "B000000001", Title: "Same Title"},
		{ID: "B000000002", Title: "Same Title"},
	}
	jobs := PlanOutputs(books, "/target")
	if jobs[0].Path == jobs[1].Path {
		t.Fatalf("duplicate output paths: %q", jobs[0].Path)
	}
	for _, job := range jobs {
		if filepath.Ext(job.Path) != ".kfx-zip" {
			t.Fatalf("unexpected output path: %q", job.Path)
		}
	}
}

func TestRenameBookPDFs(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "B0CJGD6G7L.automated.pdf")
	if err := os.WriteFile(source, []byte("pdf"), 0o600); err != nil {
		t.Fatal(err)
	}
	books := []Book{{
		ID: "B0CJGD6G7L", Title: "Künstliche Intelligenz: Ein moderner Ansatz",
	}}
	count, err := RenamePDFs(directory, books, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("renamed %d files, want 1", count)
	}
	destination := filepath.Join(directory,
		"Künstliche Intelligenz - Ein moderner Ansatz.automated.pdf")
	if _, err := os.Stat(destination); err != nil {
		t.Fatal(err)
	}
}

func TestAutomaticLibraryDiscovery(t *testing.T) {
	home := t.TempDir()
	kindleRoot := filepath.Join(home, "Library/Containers/com.amazon.Lassen/Data/Library/eBooks")
	otherRoot := filepath.Join(home, "Library/Containers/com.example.Reader/Data/Library/eBooks")
	for _, root := range []string{kindleRoot, otherRoot} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	preferences := filepath.Join(home,
		"Library/Containers/com.amazon.Lassen/Data/Library/Preferences/com.amazon.Lassen.plist")
	if err := os.MkdirAll(filepath.Dir(preferences), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(preferences, []byte("plist"), 0o600); err != nil {
		t.Fatal(err)
	}
	roots, err := DiscoverRoots(home, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 || roots[0].Path != kindleRoot || roots[0].Preferences != preferences {
		t.Fatalf("unexpected automatic roots: %+v", roots)
	}
}

func TestInferBookID(t *testing.T) {
	if got := inferBookID(filepath.Join("B0CJGD6G7L", "uuid"), "/tmp/B0CJGD6G7L/uuid"); got != "B0CJGD6G7L" {
		t.Fatalf("book ID = %q", got)
	}
}

func FuzzSafeFilename(f *testing.F) {
	f.Add("Künstliche Intelligenz: Ein/Ansatz")
	f.Add("../\x00/..")
	f.Fuzz(func(t *testing.T, input string) {
		name := SafeFilename(input)
		if name == "" || len(name) > 200 || !utf8.ValidString(name) {
			t.Fatalf("unsafe result %q", name)
		}
		if strings.ContainsAny(name, "/:\\") {
			t.Fatalf("result contains a separator: %q", name)
		}
		for _, character := range name {
			if unicode.IsControl(character) {
				t.Fatalf("result contains control character: %q", name)
			}
		}
	})
}

func createBundle(t *testing.T, root, asin, uuid string) string {
	t.Helper()
	directory := filepath.Join(root, asin, uuid)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"main.azw8", "book.voucher"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}
