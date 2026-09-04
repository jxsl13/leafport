package kfxconvert

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"image/color"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestImageComicBecomesLosslessValidatedCBZ(t *testing.T) {
	first := testPNG(t, 2, 3, testColor(1, 2, 3))
	second := testJPEG(t)
	pages := []Page{
		{ResourceID: 1, Location: "first.png", Data: first},
		{ResourceID: 2, Location: "second.jpg", Data: second},
	}
	destination := os.Getenv("LEAFPORT_CBZ_TEST_OUTPUT")
	if destination == "" {
		destination = filepath.Join(t.TempDir(), "comic.cbz")
	}
	result, err := convertImageLayoutPagesToCBZ(nil, pages, destination, Metadata{
		Title: "Example", Authors: []string{"Ada"}, Language: "en", Publisher: "Press",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Pages != 2 || result.Images != 2 || result.Resources != 2 {
		t.Fatalf("result = %+v", result)
	}
	if info, err := os.Stat(destination); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("CBZ permissions = %v, err = %v", info, err)
	}
	archive, err := zip.OpenReader(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	if len(archive.File) != 3 || archive.File[0].Name != "0001.png" || archive.File[1].Name != "0002.jpg" || archive.File[2].Name != "ComicInfo.xml" {
		t.Fatalf("CBZ entries = %+v", archive.File)
	}
	reader, err := archive.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ioReadAllAndClose(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(first) {
		t.Fatal("first page bytes changed")
	}
	var comment map[string]comicBookInfo
	if err := json.Unmarshal([]byte(archive.Comment), &comment); err != nil {
		t.Fatal(err)
	}
	if comment["ComicBookInfo/1.0"].Title != "Example" {
		t.Fatalf("CBZ comment = %q", archive.Comment)
	}
}

func TestCBZFailureRemovesPartialOutput(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "bad.cbz")
	_, err := convertImageLayoutPagesToCBZ(nil, []Page{{Data: []byte("not an image")}}, destination, Metadata{})
	if err == nil {
		t.Fatal("unsupported image unexpectedly succeeded")
	}
	if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("partial output remains: %v", statErr)
	}
}

func TestCBZMovesDeclaredCoverFoundLaterInReadingOrder(t *testing.T) {
	body := testPNG(t, 2, 3, color.Black)
	cover := testPNG(t, 2, 3, color.White)
	book := &decodedBook{
		symbols:   []string{"cover"},
		resources: map[uint32]resource{852: {format: 284, location: "cover.png"}},
		metadata: map[string]map[string][]string{
			"kindle_title_metadata": {"cover_image": {"cover"}},
		},
	}
	destination := filepath.Join(t.TempDir(), "comic.cbz")
	result, err := convertImageLayoutPagesToCBZ(book, []Page{
		{ResourceID: 1, Location: "body.png", Data: body},
		{ResourceID: 852, Location: "cover.png", Data: cover},
	}, destination, Metadata{Title: "Example"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Pages != 2 {
		t.Fatalf("result = %+v", result)
	}
	archive, err := zip.OpenReader(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	reader, err := archive.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	first, err := ioReadAllAndClose(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, cover) {
		t.Fatal("declared cover is not the first CBZ page")
	}
}

func TestNormalizeCBZPreservesSourceAndComicInfo(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source.cbz")
	destination := filepath.Join(directory, "fixed.cbz")
	if _, err := convertImageLayoutPagesToCBZ(nil, []Page{{
		ResourceID: 1, Location: "page.png", Data: testPNG(t, 2, 3, color.Black),
	}}, source, Metadata{Title: "Example", Authors: []string{"Ada"}, Language: "en"}); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	originalHash := sha256.Sum256(original)
	result, err := NormalizeCBZ(source, destination)
	if err != nil {
		t.Fatal(err)
	}
	if result.Images != 1 || !result.ComicInfo {
		t.Fatalf("normalization result = %+v", result)
	}
	after, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(after) != originalHash {
		t.Fatal("source CBZ was modified")
	}
	if _, err := NormalizeCBZ(source, destination); err == nil {
		t.Fatal("existing destination was overwritten")
	}
}

func TestNormalizeCBZRepairsEntryOrderAndComicInfoCase(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source.cbz")
	destination := filepath.Join(directory, "fixed.cbz")
	file, err := os.OpenFile(source, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	for _, entry := range []struct {
		name string
		data []byte
	}{
		{name: "z.png", data: testPNG(t, 2, 3, color.White)},
		{name: "a.png", data: testPNG(t, 2, 3, color.Black)},
		{name: "comicinfo.xml", data: []byte(`<ComicInfo><Title>Example</Title></ComicInfo>`)},
	} {
		if err := writeZIPBytes(archive, entry.name, entry.data, zip.Store); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateCBZFile(source); err == nil {
		t.Fatal("noncanonical source unexpectedly passed validation")
	}
	result, err := NormalizeCBZ(source, destination)
	if err != nil {
		t.Fatal(err)
	}
	if result.Images != 2 || !result.ComicInfo {
		t.Fatalf("normalization result = %+v", result)
	}
	fixed, err := zip.OpenReader(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer fixed.Close()
	if len(fixed.File) != 3 || fixed.File[0].Name != "0001.png" || fixed.File[1].Name != "0002.png" || fixed.File[2].Name != "ComicInfo.xml" {
		t.Fatalf("normalized entries = %+v", fixed.File)
	}
}

func testColor(red, green, blue uint8) color.RGBA {
	return color.RGBA{R: red, G: green, B: blue, A: 255}
}

func ioReadAllAndClose(reader io.ReadCloser) ([]byte, error) {
	data, readErr := io.ReadAll(reader)
	return data, errors.Join(readErr, reader.Close())
}
