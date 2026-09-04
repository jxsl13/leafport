package kfxconvert

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

func TestGroupPDFPages(t *testing.T) {
	first := []byte("first")
	second := []byte("second")
	groups := groupPDFPages([]Page{
		{Location: "a", PageIndex: 0, Data: first},
		{Location: "a", PageIndex: 1, Data: first},
		{Location: "b", PageIndex: 2, Data: second},
		{Location: "a", PageIndex: 3, Data: first},
	})
	if len(groups) != 3 || len(groups[0].pageIndices) != 2 || groups[2].pageIndices[0] != 3 {
		t.Fatalf("unexpected groups: %+v", groups)
	}
}

func TestPDFPageSelection(t *testing.T) {
	if got := pdfPageSelection([]int{0, 2, 2, 4}); got != "1,3,3,5" {
		t.Fatalf("selection = %q", got)
	}
	if !isCompletePDF([]int{0, 1, 2}, 3) || isCompletePDF([]int{0, 2}, 2) {
		t.Fatal("complete-PDF classification failed")
	}
}

func TestPDFProperties(t *testing.T) {
	properties := pdfProperties(Metadata{Title: " Title ", Authors: []string{"Ada", " ", "Bob"}})
	if properties["Title"] != "Title" || properties["Author"] != "Ada, Bob" {
		t.Fatalf("properties = %#v", properties)
	}
}

func TestPDFBookmarksMapKFXNavigationToFirstSectionPage(t *testing.T) {
	book := testPDFNavigationBook()
	bookmarks := pdfBookmarks(book, []Page{{SectionID: 10}, {SectionID: 10}, {SectionID: 20}})
	if len(bookmarks) != 2 || bookmarks[0].PageFrom != 1 || bookmarks[1].PageFrom != 3 {
		t.Fatalf("bookmarks = %+v", bookmarks)
	}
}

func TestPDFBookmarksWithRepeatedTitlesKeepDistinctPages(t *testing.T) {
	imageData := testPNG(t, 2, 2, color.Black)
	var source bytes.Buffer
	if err := api.ImportImages(nil, &source, []io.Reader{
		bytes.NewReader(imageData), bytes.NewReader(imageData),
	}, nil, pdfConfiguration()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "repeated-titles.pdf")
	if err := os.WriteFile(path, source.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	want := []pdfcpu.Bookmark{
		{Title: "Repeated heading", PageFrom: 1},
		{Title: "Repeated heading", PageFrom: 2},
	}
	if err := writePDFBookmarks(path, want); err != nil {
		t.Fatal(err)
	}
	if err := validatePDFBookmarks(path, want); err != nil {
		t.Fatal(err)
	}
}

func testPDFNavigationBook() *decodedBook {
	symbol := func(id uint64) *ionValue { return &ionValue{kind: ionSymbol, unsigned: id} }
	field := func(id uint64, value *ionValue) ionField { return ionField{id: id, value: value} }
	list := func(values ...*ionValue) *ionValue { return &ionValue{kind: ionList, children: values} }
	structure := func(fields ...ionField) *ionValue { return &ionValue{kind: ionStruct, fields: fields} }
	entry := func(label string, target uint64, children ...*ionValue) *ionValue {
		return structure(
			field(241, structure(field(244, &ionValue{kind: ionString, text: label}))),
			field(246, structure(field(155, symbol(target)))), field(247, list(children...)),
		)
	}
	document := structure(field(169, list(structure(field(170, list(symbol(10), symbol(20)))))))
	toc := list(structure(field(392, list(structure(
		field(235, symbol(212)),
		field(247, list(entry("First", 10), entry("Second", 20))),
	)))))
	section := func(storyID uint64) *ionValue {
		storyReference := structure(field(176, symbol(storyID)))
		return structure(field(141, storyReference))
	}
	story := func(nodeID uint64) *ionValue {
		node := structure(field(155, symbol(nodeID)))
		return structure(field(146, node))
	}
	return &decodedBook{
		document: document,
		sections: map[uint32]*ionValue{
			10: section(100),
			20: section(200),
		},
		storylines: map[uint32]*ionValue{
			100: story(1000),
			200: story(2000),
		},
		navigation: toc,
	}
}

func TestCompleteEmbeddedPDFReceivesKFXNavigation(t *testing.T) {
	imageData := testPNG(t, 2, 2, color.Black)
	var source bytes.Buffer
	if err := api.ImportImages(nil, &source, []io.Reader{bytes.NewReader(imageData)}, nil, pdfConfiguration()); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "enhanced.pdf")
	result, err := convertFixedLayoutPagesToPDF(testPDFNavigationBook(), []Page{{
		ResourceID: 1, SectionID: 10, Format: kfxPDFFormat, Location: "source.pdf", PageIndex: 0, Data: source.Bytes(),
	}}, destination, Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExactCopy {
		t.Fatal("PDF with KFX navigation was incorrectly reported as an exact copy")
	}
	file, err := os.Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	bookmarks, err := api.Bookmarks(file, pdfConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	if len(bookmarks) != 1 || bookmarks[0].Title != "First" || bookmarks[0].PageFrom != 1 {
		t.Fatalf("written bookmarks = %+v", bookmarks)
	}
}

func TestKFXTextOverlayLinksBecomePDFAnnotations(t *testing.T) {
	book := testPDFNavigationBook()
	book.anchors = map[uint32]anchor{300: {externalURL: "https://example.com/reference"}}
	event := func(width int64, link uint64) *ionValue {
		fields := []ionField{testField(56, testInteger(width))}
		if link != 0 {
			fields = append(fields, testField(179, testSymbol(link)))
		}
		return testStruct(fields...)
	}
	textNode := testStruct(
		testField(56, testInteger(400)), testField(57, testInteger(100)),
		testField(58, testInteger(500)), testField(59, testInteger(200)),
		testField(142, testList(event(100, 0), event(200, 300), event(100, 0))),
		testField(155, testSymbol(1000)), testField(159, testSymbol(269)),
	)
	book.storylines[100] = testStruct(testField(146, testList(testStruct(
		testField(66, testInteger(1000)), testField(67, testInteger(2000)),
		testField(146, testList(textNode)),
	))))

	imageData := testPNG(t, 10, 20, color.Black)
	var source bytes.Buffer
	if err := api.ImportImages(nil, &source, []io.Reader{bytes.NewReader(imageData)}, nil, pdfConfiguration()); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "linked.pdf")
	_, err := convertFixedLayoutPagesToPDF(book, []Page{{
		ResourceID: 1, SectionID: 10, Format: kfxPDFFormat, Location: "source.pdf", PageIndex: 0, Data: source.Bytes(),
	}}, destination, Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	annotations, err := api.Annotations(file, nil, pdfConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	links := annotations[1][model.AnnLink].Map
	if len(links) != 1 {
		t.Fatalf("link annotations = %+v", links)
	}
	for _, link := range links {
		if link.ContentString() != "https://example.com/reference" || !strings.HasPrefix(link.ID(), "leafport-link-") {
			t.Fatalf("link annotation = id %q, content %q", link.ID(), link.ContentString())
		}
	}
}

func TestPDFPageSelectionIsNotMistakenForExactCopyAndAddsMetadata(t *testing.T) {
	var imageData bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.Black)
	if err := png.Encode(&imageData, img); err != nil {
		t.Fatal(err)
	}
	var source bytes.Buffer
	if err := api.ImportImages(nil, &source, []io.Reader{
		bytes.NewReader(imageData.Bytes()), bytes.NewReader(imageData.Bytes()),
	}, nil, pdfConfiguration()); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "selected.pdf")
	result, err := convertFixedLayoutPagesToPDF(testPDFNavigationBook(), []Page{{
		ResourceID: 1, SectionID: 10, Format: kfxPDFFormat, Location: "source.pdf", PageIndex: 0, Data: source.Bytes(),
	}}, destination, Metadata{Title: "Selected page", Authors: []string{"Ada"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExactCopy || result.Pages != 1 {
		t.Fatalf("result = %+v", result)
	}
	file, err := os.Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if pages, err := api.PageCount(file, pdfConfiguration()); err != nil || pages != 1 {
		t.Fatalf("page count = %d, err = %v", pages, err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	context, err := api.ReadValidateAndOptimize(file, pdfConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	if context.Title != "Selected page" || context.Author != "Ada" {
		t.Fatalf("metadata = title %q, author %q", context.Title, context.Author)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	bookmarks, err := api.Bookmarks(file, pdfConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	if len(bookmarks) != 1 || bookmarks[0].Title != "First" || bookmarks[0].PageFrom != 1 {
		t.Fatalf("written bookmarks = %+v", bookmarks)
	}
}

func TestEmbeddedPDFNonfunctionalLinksAreRemovedSelectively(t *testing.T) {
	imageData := testPNG(t, 2, 2, color.Black)
	var source bytes.Buffer
	if err := api.ImportImages(nil, &source, []io.Reader{bytes.NewReader(imageData)}, nil, pdfConfiguration()); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	plainPath := filepath.Join(directory, "plain.pdf")
	validPath := filepath.Join(directory, "valid.pdf")
	brokenPath := filepath.Join(directory, "broken.pdf")
	if err := os.WriteFile(plainPath, source.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	brokenCandidate := model.NewLinkAnnotation(
		*types.NewRectangle(0, 0, 1, 1), 0, "", "broken", "", 0, nil,
		nil, "https://example.com", nil, false, 0, model.BSSolid,
	)
	validLink := model.NewLinkAnnotation(
		*types.NewRectangle(1, 1, 2, 2), 0, "", "valid", "", 0, nil,
		nil, "https://example.org", nil, false, 0, model.BSSolid,
	)
	if err := api.AddAnnotationsMapFile(plainPath, validPath, map[int][]model.AnnotationRenderer{
		1: {brokenCandidate, validLink},
	}, pdfConfiguration(), false); err != nil {
		t.Fatal(err)
	}
	valid, err := os.Open(validPath)
	if err != nil {
		t.Fatal(err)
	}
	context, err := api.ReadValidateAndOptimize(valid, pdfConfiguration())
	if closeErr := valid.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	page, _, _, err := context.PageDict(1, false)
	if err != nil {
		t.Fatal(err)
	}
	rawAnnotations, err := context.DereferenceArray(page["Annots"])
	if err != nil || len(rawAnnotations) != 2 {
		t.Fatalf("annotations = %v, err = %v", rawAnnotations, err)
	}
	changed := false
	for _, rawAnnotation := range rawAnnotations {
		annotation, err := context.DereferenceDict(rawAnnotation)
		if err != nil {
			t.Fatal(err)
		}
		if id := annotation.StringEntry("NM"); id != nil && *id == "broken" {
			annotation["A"] = types.Dict{"S": types.Name("GoTo")}
			changed = true
		}
	}
	if !changed {
		t.Fatal("did not locate the broken-link test annotation")
	}
	if err := api.WriteContextFile(context, brokenPath); err != nil {
		t.Fatal(err)
	}
	brokenData, err := os.ReadFile(brokenPath)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(directory, "clean.pdf")
	result, err := convertFixedLayoutPagesToPDF(nil, []Page{{
		ResourceID: 1, Format: kfxPDFFormat, Location: "broken.pdf", PageIndex: 0, Data: brokenData,
	}}, destination, Metadata{Title: "Clean sample"})
	if err != nil {
		t.Fatal(err)
	}
	if result.BrokenLinksRemoved != 1 || result.ExactCopy {
		t.Fatalf("result = %+v", result)
	}
	file, err := os.Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	finalAnnotations, err := api.Annotations(file, nil, pdfConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	if len(finalAnnotations[1][model.AnnLink].Map) != 1 {
		t.Fatalf("link annotations = %+v", finalAnnotations[1][model.AnnLink].Map)
	}
	for _, annotation := range finalAnnotations[1][model.AnnLink].Map {
		if annotation.ID() != "valid" || annotation.ContentString() != "https://example.org" {
			t.Fatalf("retained link = id %q, content %q", annotation.ID(), annotation.ContentString())
		}
	}
}

func TestNormalizePDFRemovesHiddenSignatureWidgetAndActiveState(t *testing.T) {
	imageData := testPNG(t, 2, 2, color.Black)
	var source bytes.Buffer
	if err := api.ImportImages(nil, &source, []io.Reader{bytes.NewReader(imageData)}, nil, pdfConfiguration()); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	plainPath := filepath.Join(directory, "plain.pdf")
	interactivePath := filepath.Join(directory, "interactive.pdf")
	destination := filepath.Join(directory, "normalized.pdf")
	if err := os.WriteFile(plainPath, source.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	plain, err := os.Open(plainPath)
	if err != nil {
		t.Fatal(err)
	}
	context, err := api.ReadValidateAndOptimize(plain, pdfConfiguration())
	if closeErr := plain.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	root, err := context.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	root["AcroForm"] = types.Dict{"Fields": types.Array{}, "SigFlags": types.Integer(3)}
	root["OpenAction"] = types.Dict{"S": types.Name("JavaScript"), "JS": types.StringLiteral("app.alert('x')")}
	page, _, _, err := context.PageDict(1, false)
	if err != nil {
		t.Fatal(err)
	}
	page["Annots"] = types.Array{types.Dict{
		"Type": types.Name("Annot"), "Subtype": types.Name("Widget"), "FT": types.Name("Sig"),
		"Rect": types.Array{types.Integer(-10), types.Integer(-10), types.Integer(-10), types.Integer(-10)},
	}}
	if err := api.WriteContextFile(context, interactivePath); err != nil {
		t.Fatal(err)
	}
	result, err := NormalizePDF(interactivePath, destination)
	if err != nil {
		t.Fatal(err)
	}
	if result.ActionsRemoved == 0 || result.AnnotationsRemoved != 1 || result.Pages != 1 {
		t.Fatalf("normalization result = %+v", result)
	}
	if err := validateKindlePDF(destination); err != nil {
		t.Fatal(err)
	}
	normalized, err := os.Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	normalizedContext, err := api.ReadValidateAndOptimize(normalized, strictPDFConfiguration())
	closeErr := normalized.Close()
	if err != nil || closeErr != nil {
		t.Fatal(errors.Join(err, closeErr))
	}
	if normalizedContext.HeaderVersion == nil || *normalizedContext.HeaderVersion != model.V17 ||
		normalizedContext.RootVersion != nil || normalizedContext.Read.UsingXRefStreams ||
		normalizedContext.Read.UsingObjectStreams || normalizedContext.Encrypt != nil {
		t.Fatalf("normalized PDF profile = %s", normalizedContext)
	}
	if _, err := NormalizePDF(interactivePath, destination); err == nil {
		t.Fatal("existing destination was overwritten")
	}
}

func TestRepairPDFFontMetricsUsesDescriptorBoundingBox(t *testing.T) {
	descriptor := types.Dict{
		"Type":      types.Name("FontDescriptor"),
		"FontBBox":  types.Array{types.Integer(-20), types.Integer(-210), types.Integer(1010), types.Integer(894)},
		"Ascent":    types.Integer(0),
		"CapHeight": types.Integer(0),
		"Descent":   types.Integer(-206),
	}
	context := &model.Context{XRefTable: &model.XRefTable{Table: map[int]*model.XRefTableEntry{
		7: model.NewXRefTableEntryGen0(descriptor),
	}}}
	repaired, err := repairPDFFontMetrics(context)
	if err != nil {
		t.Fatal(err)
	}
	if repaired != 1 || descriptor.IntEntry("Ascent") == nil || *descriptor.IntEntry("Ascent") != 894 ||
		descriptor.IntEntry("CapHeight") == nil || *descriptor.IntEntry("CapHeight") != 894 {
		t.Fatalf("repaired descriptor = %v, count = %d", descriptor, repaired)
	}
	if err := validatePDFFontMetrics(context); err != nil {
		t.Fatal(err)
	}
}

func TestValidateKindlePDFRejectsActiveNameTrees(t *testing.T) {
	imageData := testPNG(t, 2, 2, color.Black)
	var source bytes.Buffer
	if err := api.ImportImages(nil, &source, []io.Reader{bytes.NewReader(imageData)}, nil, pdfCompatibilityConfiguration()); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	plainPath := filepath.Join(directory, "plain.pdf")
	activePath := filepath.Join(directory, "active.pdf")
	if err := os.WriteFile(plainPath, source.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	plain, err := os.Open(plainPath)
	if err != nil {
		t.Fatal(err)
	}
	context, err := api.ReadValidateAndOptimize(plain, pdfCompatibilityConfiguration())
	closeErr := plain.Close()
	if err != nil || closeErr != nil {
		t.Fatal(errors.Join(err, closeErr))
	}
	root, err := context.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	root["Names"] = types.Dict{"JavaScript": types.Dict{"Names": types.Array{
		types.StringLiteral("script"), types.Dict{
			"S": types.Name("JavaScript"), "JS": types.StringLiteral("app.alert('x')"),
		},
	}}}
	if err := api.WriteContextFile(context, activePath); err != nil {
		t.Fatal(err)
	}
	err = validateKindlePDF(activePath)
	if err == nil || !strings.Contains(err.Error(), "JavaScript name tree") {
		t.Fatalf("active name tree validation error = %v", err)
	}
}

func TestImageFixedLayoutBecomesValidatedPDF(t *testing.T) {
	page := func(width, height int) []byte {
		var data bytes.Buffer
		if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
			t.Fatal(err)
		}
		return data.Bytes()
	}
	destination := os.Getenv("LEAFPORT_PDF_TEST_OUTPUT")
	if destination == "" {
		destination = filepath.Join(t.TempDir(), "images.pdf")
	}
	pages := []Page{
		{ResourceID: 1, SectionID: 10, Format: 284, Location: "portrait.png", Data: page(20, 30)},
		{ResourceID: 2, SectionID: 20, Format: 284, Location: "landscape.png", Data: page(40, 10)},
	}
	result, err := convertFixedLayoutPagesToPDF(testPDFNavigationBook(), pages, destination,
		Metadata{Title: "Image book", Authors: []string{"Ada"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Pages != 2 || result.Resources != 2 || result.ExactCopy {
		t.Fatalf("result = %+v", result)
	}
	file, err := os.Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if count, err := api.PageCount(file, pdfConfiguration()); err != nil || count != 2 {
		t.Fatalf("page count = %d, err = %v", count, err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	context, err := api.ReadValidateAndOptimize(file, pdfConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	if context.Title != "Image book" || context.Author != "Ada" {
		t.Fatalf("metadata = title %q, author %q", context.Title, context.Author)
	}
}

func TestMixedImageAndEmbeddedPDFPreservesPageOrderWithoutArtifacts(t *testing.T) {
	imageData := testPNG(t, 6, 4, color.RGBA{G: 255, A: 255})
	var embedded bytes.Buffer
	if err := api.ImportImages(nil, &embedded, []io.Reader{bytes.NewReader(imageData)}, nil, pdfConfiguration()); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	destination := filepath.Join(directory, "mixed.pdf")
	pages := []Page{
		{ResourceID: 1, SectionID: 10, Format: 284, Location: "image.png", Data: imageData},
		{ResourceID: 2, SectionID: 20, Format: kfxPDFFormat, Location: "embedded.pdf", Data: embedded.Bytes()},
	}
	result, err := convertFixedLayoutPagesToPDF(testPDFNavigationBook(), pages, destination, Metadata{Title: "Mixed"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Pages != 2 || result.Resources != 2 || result.ExactCopy {
		t.Fatalf("result = %+v", result)
	}
	file, err := os.Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	if count, countErr := api.PageCount(file, pdfConfiguration()); countErr != nil || count != 2 {
		_ = file.Close()
		t.Fatalf("page count = %d, err = %v", count, countErr)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "mixed.pdf" {
		t.Fatalf("target artifacts = %v", entries)
	}
}

func TestImageFixedLayoutPreservesRightToLeftDirection(t *testing.T) {
	book := testPDFNavigationBook()
	book.document.fields = append(book.document.fields, testField(192, testSymbol(375)))
	destination := filepath.Join(t.TempDir(), "rtl.pdf")
	_, err := convertFixedLayoutPagesToPDF(book, []Page{{
		ResourceID: 1, SectionID: 10, Format: 284, Location: "page.png",
		Data: testPNG(t, 4, 6, color.Black),
	}}, destination, Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	preferences, err := api.ViewerPreferencesFile(destination, false, pdfConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	if preferences.Direction == nil || preferences.Direction.String() != "R2L" {
		t.Fatalf("viewer preferences = %+v", preferences)
	}
}
