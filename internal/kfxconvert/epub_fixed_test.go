package kfxconvert

import (
	"archive/zip"
	"bytes"
	"image/color"
	"os"
	"path/filepath"
	"testing"
)

func TestImagePagesBecomeValidatedFixedLayoutComicEPUB(t *testing.T) {
	destination := os.Getenv("LEAFPORT_FIXED_EPUB_TEST_OUTPUT")
	if destination == "" {
		destination = filepath.Join(t.TempDir(), "comic.epub")
	}
	pages := []Page{
		{ResourceID: 1, Location: "cover.png", Data: testPNG(t, 600, 800, color.Black)},
		{ResourceID: 2, Location: "page.png", Data: testPNG(t, 12, 16, color.White)},
	}
	result, err := convertImageLayoutPagesToEPUB(nil, pages, destination, Metadata{
		Identifier: "fixed-test", Title: "Fixed test", Authors: []string{"Ada"}, Language: "en",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Pages != 2 || result.Images != 2 {
		t.Fatalf("result = %+v", result)
	}
	validated, err := ValidateEPUBFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !validated.CoverFirst || validated.SpineItems != 2 || validated.Images != 2 {
		t.Fatalf("validation = %+v", validated)
	}
}

func TestImagePagesBecomeValidatedFixedLayoutEPUB(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "fixed.epub")
	pages := []Page{
		{ResourceID: 1, Location: "cover.png", Data: testPNG(t, 600, 800, color.Black)},
		{ResourceID: 2, Location: "page.png", Data: testPNG(t, 12, 16, color.White)},
	}
	if _, err := convertImageLayoutPagesToEPUB(nil, pages, destination, Metadata{
		Identifier: "fixed-test", Title: "Fixed test", Language: "en",
	}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateEPUBFile(destination); err != nil {
		t.Fatal(err)
	}
	archive, err := zip.OpenReader(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	for _, entry := range archive.File {
		if entry.Name != "OEBPS/content.opf" {
			continue
		}
		data, readErr := readZipFile(entry)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !bytes.Contains(data, []byte(`rendition:layout">pre-paginated`)) || bytes.Contains(data, []byte(`name="book-type"`)) {
			t.Fatalf("unexpected fixed-layout metadata: %s", data)
		}
		if !bytes.Contains(data, []byte(`page-spread-right rendition:page-spread-right`)) {
			t.Fatalf("fixed-layout EPUB is missing compatible spread hints: %s", data)
		}
		return
	}
	t.Fatal("fixed-layout EPUB has no package document")
}
