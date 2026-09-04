package kfxconvert

import (
	"bytes"
	"image"
	"image/color"
	"testing"
)

func TestFallbackCoverUsesFirstNonemptySpinePage(t *testing.T) {
	sections := []epubSection{
		{path: "text/empty.xhtml", data: []byte(`<html><head><title>Not body content</title></head><body>   </body></html>`)},
		{path: "text/first.xhtml", data: []byte(`<html><body><p>First real page</p><img src="../images/page.jpg"/></body></html>`)},
		{path: "text/later.xhtml", data: []byte(`<html><body><p>Later page</p></body></html>`)},
	}
	index, text, err := firstNonemptyEPUBSection(sections)
	if err != nil {
		t.Fatal(err)
	}
	if index != 1 || text != "First real page" {
		t.Fatalf("fallback page = %d, %q", index, text)
	}
	pageImage := &epubAsset{path: "images/page.jpg", mediaType: "image/png", data: testPNG(t, 600, 800, color.Black), image: true}
	builder := epubBuilder{assets: make(map[uint32]*epubAsset), assetOrder: []*epubAsset{pageImage}}
	cover, section, err := builder.ensureMetadataCover(sections, Metadata{Title: "Example"})
	if err != nil {
		t.Fatal(err)
	}
	if cover != pageImage || section != 1 || !pageImage.coverImage || len(builder.assetOrder) != 1 {
		t.Fatalf("cover = %+v, assets = %+v", cover, builder.assetOrder)
	}
}

func TestFallbackCoverRasterizesFirstNonemptyTextPage(t *testing.T) {
	sections := []epubSection{
		{path: "text/empty.xhtml", data: []byte(`<html><body></body></html>`)},
		{path: "text/first.xhtml", data: []byte(`<html><body><h1>Chapter one</h1><p>Opening text.</p></body></html>`)},
	}
	builder := epubBuilder{assets: make(map[uint32]*epubAsset)}
	cover, section, err := builder.ensureMetadataCover(sections, Metadata{Title: "Example"})
	if err != nil {
		t.Fatal(err)
	}
	decoded, format, err := image.Decode(bytes.NewReader(cover.data))
	if err != nil {
		t.Fatal(err)
	}
	if format != "jpeg" || decoded.Bounds().Dx() != 1200 || decoded.Bounds().Dy() != 1600 || !cover.coverImage || section != 1 {
		t.Fatalf("fallback cover format=%s bounds=%v asset=%+v", format, decoded.Bounds(), cover)
	}
}
