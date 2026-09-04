package kfxconvert

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"image/color"
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeEPUBMovesExistingCoverWithoutChangingSource(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source.epub")
	destination := filepath.Join(directory, "fixed.epub")
	writeNormalizeTestEPUB(t, source, true, false)
	original, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	originalHash := sha256.Sum256(original)
	if _, err := ValidateEPUBFile(source); err == nil {
		t.Fatal("source with a late cover passed the compatibility profile")
	}
	result, err := NormalizeEPUB(source, destination)
	if err != nil {
		t.Fatal(err)
	}
	if !result.CoverRepaired || result.SpineItems != 2 {
		t.Fatalf("normalization result = %+v", result)
	}
	validated, err := ValidateEPUBFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !validated.CoverFirst || validated.CoverWidth != 600 || validated.CoverHeight != 800 {
		t.Fatalf("validation result = %+v", validated)
	}
	archive, err := zip.OpenReader(destination)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range archive.File {
		if filepath.Base(entry.Name) == "leafport-cover.xhtml" {
			archive.Close()
			t.Fatal("normalization added duplicate HTML cover")
		}
		if entry.Name == "OEBPS/fonts/unused.otf" {
			archive.Close()
			t.Fatal("normalization retained an unused CFF font")
		}
		if entry.Name == "OEBPS/styles.css" {
			data, readErr := readZipFile(entry)
			if readErr != nil {
				archive.Close()
				t.Fatal(readErr)
			}
			if bytes.Contains(data, []byte("@font-face")) || bytes.Contains(data, []byte("unused.otf")) {
				archive.Close()
				t.Fatal("normalization retained the unused CFF font-face")
			}
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(after) != originalHash {
		t.Fatal("source EPUB was modified")
	}
	if _, err := NormalizeEPUB(source, destination); err == nil {
		t.Fatal("existing destination was overwritten")
	}
}

func TestNormalizeEPUBFallbackUsesFirstNonemptyTextPage(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source.epub")
	destination := os.Getenv("LEAFPORT_NORMALIZED_EPUB_TEST_OUTPUT")
	if destination == "" {
		destination = filepath.Join(directory, "fixed.epub")
	}
	writeNormalizeTestEPUB(t, source, false, false)
	result, err := NormalizeEPUB(source, destination)
	if err != nil {
		t.Fatal(err)
	}
	if !result.CoverRepaired || result.SpineItems != 2 {
		t.Fatalf("normalization result = %+v", result)
	}
	validated, err := ValidateEPUBFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !validated.CoverFirst || validated.CoverWidth != 1200 || validated.CoverHeight != 1600 {
		t.Fatalf("fallback validation result = %+v", validated)
	}
}

func TestNormalizeEPUBRetainsUsedCFFFont(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source.epub")
	destination := filepath.Join(directory, "fixed.epub")
	writeNormalizeTestEPUB(t, source, false, true)
	if _, err := NormalizeEPUB(source, destination); err != nil {
		t.Fatal(err)
	}
	archive, err := zip.OpenReader(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	for _, entry := range archive.File {
		if entry.Name == "OEBPS/fonts/unused.otf" {
			return
		}
	}
	t.Fatal("normalization removed a used CFF font")
}

func writeNormalizeTestEPUB(t *testing.T, path string, declaredCover, usedFont bool) {
	t.Helper()
	coverProperties := ""
	landmarks := ""
	firstPage := `<p>Text</p>`
	realCoverManifest := ""
	if declaredCover {
		coverProperties = ` properties="cover-image"`
		landmarks = `<nav epub:type="landmarks"><ol><li><a epub:type="cover" href="text/one.xhtml">Cover</a></li></ol></nav>`
		firstPage = `<img src="../images/real-cover.png" alt="Cover"/>`
		realCoverManifest = `<item id="real-cover" href="images/real-cover.png" media-type="image/png"/>`
	}
	fontUse := ""
	if usedFont {
		fontUse = ` body {font-family:"Unused CFF",serif}`
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	entries := []struct {
		name, data string
		method     uint16
	}{
		{"mimetype", "application/epub+zip", zip.Store},
		{"META-INF/container.xml", `<?xml version="1.0"?><container xmlns="urn:oasis:names:tc:opendocument:xmlns:container" version="1.0"><rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles></container>`, zip.Deflate},
		{"OEBPS/content.opf", `<?xml version="1.0"?><package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="id"><metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:identifier id="id">example</dc:identifier><dc:title>Example</dc:title><dc:language>en</dc:language><meta property="dcterms:modified">2026-01-01T00:00:00Z</meta></metadata><manifest><item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/><item id="css" href="styles.css" media-type="text/css"/><item id="one" href="text/one.xhtml" media-type="application/xhtml+xml"/><item id="two" href="text/two.xhtml" media-type="application/xhtml+xml"/>` + realCoverManifest + `<item id="cover" href="images/cover.png" media-type="image/png"` + coverProperties + `/><item id="unused-font" href="fonts/unused.otf" media-type="font/otf"/></manifest><spine><itemref idref="one"/><itemref idref="two"/></spine></package>`, zip.Deflate},
		{"OEBPS/nav.xhtml", `<?xml version="1.0"?><html xmlns="http://www.w3.org/1999/xhtml" xmlns:epub="http://www.idpf.org/2007/ops"><head><title>Navigation</title></head><body><nav epub:type="toc"><ol><li><a href="text/one.xhtml">One</a></li></ol></nav>` + landmarks + `</body></html>`, zip.Deflate},
		{"OEBPS/styles.css", `@font-face {font-family:"Unused CFF";src:url("fonts/unused.otf");} body {font-family:serif}` + fontUse, zip.Deflate},
		{"OEBPS/fonts/unused.otf", "OTTO-not-a-real-test-font", zip.Deflate},
		{"OEBPS/text/one.xhtml", `<?xml version="1.0"?><html xmlns="http://www.w3.org/1999/xhtml"><head><title>One</title><link rel="stylesheet" href="../styles.css"/></head><body>` + firstPage + `</body></html>`, zip.Deflate},
		{"OEBPS/text/two.xhtml", `<?xml version="1.0"?><html xmlns="http://www.w3.org/1999/xhtml"><head><title>Two</title></head><body><img src="../images/cover.png" alt="Cover"/></body></html>`, zip.Deflate},
	}
	for _, entry := range entries {
		if err := writeZIPEntry(archive, entry.name, entry.data, entry.method); err != nil {
			t.Fatal(err)
		}
	}
	coverWidth, coverHeight := 600, 800
	if declaredCover {
		coverWidth, coverHeight = 200, 300
		if err := writeZIPBytes(archive, "OEBPS/images/real-cover.png", testPNG(t, 600, 800, color.Black), zip.Deflate); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeZIPBytes(archive, "OEBPS/images/cover.png", testPNG(t, coverWidth, coverHeight, color.Black), zip.Deflate); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
