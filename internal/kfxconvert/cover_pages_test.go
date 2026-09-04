package kfxconvert

import (
	"image/color"
	"testing"
)

func TestFixedLayoutCoverOrderingMovesDeclaredLaterCover(t *testing.T) {
	book := &decodedBook{
		symbols:   []string{"cover"},
		resources: map[uint32]resource{852: {location: "cover.jpg"}},
		metadata: map[string]map[string][]string{
			"kindle_title_metadata": {"cover_image": {"cover"}},
		},
	}
	pages := []Page{
		{ResourceID: 1, Data: []byte("body")},
		{ResourceID: 852, Data: []byte("cover")},
	}
	ordered, cover := fixedLayoutPagesWithCover(book, pages)
	if cover != 0 || len(ordered) != 2 || ordered[0].ResourceID != 852 || ordered[1].ResourceID != 1 {
		t.Fatalf("ordered pages = %+v, cover = %d", ordered, cover)
	}
	if len(pages) != 2 || pages[0].ResourceID != 1 {
		t.Fatal("source page slice was modified")
	}
}

func TestFixedLayoutCoverOrderingAddsCoverOutsideReadingOrder(t *testing.T) {
	book := &decodedBook{
		symbols:   []string{"cover"},
		resources: map[uint32]resource{852: {format: 284, location: "cover.png", width: 12, height: 16}},
		rawMedia:  map[string][]byte{"cover.png": testPNG(t, 12, 16, color.Black)},
		metadata: map[string]map[string][]string{
			"kindle_title_metadata": {"cover_image": {"cover"}},
		},
	}
	ordered, cover := fixedLayoutPagesWithCover(book, []Page{{ResourceID: 1, Data: []byte("body")}})
	if cover != 0 || len(ordered) != 2 || ordered[0].ResourceID != 852 || ordered[0].Location != "cover.png" {
		t.Fatalf("ordered pages = %+v, cover = %d", ordered, cover)
	}
}

func TestFixedLayoutCoverFallbackSkipsEmptyLeadingPage(t *testing.T) {
	ordered, cover := fixedLayoutPagesWithCover(nil, []Page{
		{ResourceID: 1},
		{ResourceID: 2, Data: []byte("first non-empty")},
	})
	if cover != 0 || len(ordered) != 2 || ordered[0].ResourceID != 2 || ordered[1].ResourceID != 1 {
		t.Fatalf("ordered pages = %+v, cover = %d", ordered, cover)
	}
}
