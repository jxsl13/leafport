package kfxconvert

import "testing"

func TestChooseFormatPreservesPDFBackedPublication(t *testing.T) {
	decision := chooseFormat(nil, []Page{
		{Format: kfxPDFFormat, Data: []byte("%PDF-1.7\nfirst")},
		{Format: kfxPDFFormat, Data: []byte("%PDF-1.7\nsecond")},
	})
	if decision.Format != "PDF" {
		t.Fatalf("format = %q, want PDF", decision.Format)
	}
}

func TestChooseFormatUsesEPUBOnlyWithoutValidatedFixedLayoutPages(t *testing.T) {
	decision := chooseFormat(nil, nil)
	if decision.Format != "EPUB" {
		t.Fatalf("format = %q, want EPUB", decision.Format)
	}
}

func TestChooseFormatUsesPDFForImageBasedFixedLayout(t *testing.T) {
	for _, pages := range [][]Page{
		{{Format: 285, Data: []byte("image")}},
		{{Format: kfxPDFFormat, Data: []byte("not a PDF")}},
		{{Format: kfxPDFFormat, Data: []byte("%PDF-1.7")}, {Format: 285, Data: []byte("image")}},
	} {
		if decision := chooseFormat(nil, pages); decision.Format != "PDF" {
			t.Fatalf("format = %q, want PDF", decision.Format)
		}
	}
}

func TestChooseFormatUsesCBZOnlyForExplicitImageComic(t *testing.T) {
	book := &decodedBook{
		metadataRaw: map[string]map[string][]*ionValue{
			"kindle_capability_metadata": {
				"yj_fixed_layout":     {testInteger(1)},
				"yj_publisher_panels": {testInteger(1)},
			},
		},
	}
	pages := []Page{{Format: 285, Data: []byte{0xff, 0xd8, 0xff, 0x00}}}
	if decision := chooseFormat(book, pages); decision.Format != "CBZ" {
		t.Fatalf("format = %q, want CBZ", decision.Format)
	}
	delete(book.metadataRaw["kindle_capability_metadata"], "yj_publisher_panels")
	if decision := chooseFormat(book, pages); decision.Format != "PDF" {
		t.Fatalf("fixed layout without a comic marker = %q, want PDF", decision.Format)
	}
}

func TestChooseFormatKeepsPDFBackedComicAsPDF(t *testing.T) {
	book := &decodedBook{document: testStruct(testField(665, testSymbol(666)))}
	decision := chooseFormat(book, []Page{{Format: kfxPDFFormat, Data: []byte("%PDF-1.7")}})
	if decision.Format != "PDF" {
		t.Fatalf("format = %q, want PDF", decision.Format)
	}
}
