package kfxconvert

import (
	"archive/zip"
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
	second := []byte{0xff, 0xd8, 0xff, 0xd9}
	pages := []Page{
		{ResourceID: 1, Location: "first.png", Data: first},
		{ResourceID: 2, Location: "second.jpg", Data: second},
	}
	destination := filepath.Join(t.TempDir(), "comic.cbz")
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
	if len(archive.File) != 2 || archive.File[0].Name != "0001.png" || archive.File[1].Name != "0002.jpg" {
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

func testColor(red, green, blue uint8) color.RGBA {
	return color.RGBA{R: red, G: green, B: blue, A: 255}
}

func ioReadAllAndClose(reader io.ReadCloser) ([]byte, error) {
	data, readErr := io.ReadAll(reader)
	return data, errors.Join(readErr, reader.Close())
}
