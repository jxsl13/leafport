package kfxconvert

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

func TestFixedLayoutPagesRequiresCapabilityForRasterImages(t *testing.T) {
	book := testImageLayoutBook(testImageNode(1))
	pages, err := book.fixedLayoutPages()
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 0 {
		t.Fatalf("ordinary reflowable image produced %d fixed-layout pages", len(pages))
	}
	book.metadataRaw = fixedLayoutMetadata(1)
	pages, err = book.fixedLayoutPages()
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 || pages[0].ResourceID != 1 || pages[0].SectionID != 10 {
		t.Fatalf("pages = %+v", pages)
	}
}

func TestFixedLayoutPagesRejectsRenderedText(t *testing.T) {
	book := testImageLayoutBook(testImageNode(1), testTextNode())
	book.metadataRaw = fixedLayoutMetadata(1)
	pages, err := book.fixedLayoutPages()
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 0 {
		t.Fatalf("text-bearing publication produced %d image pages", len(pages))
	}
}

func TestFixedLayoutPagesRejectsAmbiguousSectionImages(t *testing.T) {
	for _, nodes := range [][]*ionValue{
		{testImageNode(1), testImageNode(1), testImageNode(1)},
		{testImageNode(1), testBackgroundNode(1)},
	} {
		book := testImageLayoutBook(nodes...)
		book.metadataRaw = fixedLayoutMetadata(1)
		pages, err := book.fixedLayoutPages()
		if err != nil {
			t.Fatal(err)
		}
		if len(pages) != 0 {
			t.Fatalf("ambiguous section produced %d image pages", len(pages))
		}
	}
}

func TestCollectMetadataRetainsNumericCapabilities(t *testing.T) {
	book := &decodedBook{
		metadata:    make(map[string]map[string][]string),
		metadataRaw: make(map[string]map[string][]*ionValue),
	}
	book.collectMetadata(testStruct(testField(491, testList(testStruct(
		testField(495, testString("kindle_capability_metadata")),
		testField(258, testList(testStruct(
			testField(492, testString("yj_fixed_layout")),
			testField(307, &ionValue{kind: ionInt, integer: 3}),
		))),
	)))))
	value := book.metadataValue("kindle_capability_metadata", "yj_fixed_layout")
	if number, ok := ionInteger(value); !ok || number != 3 {
		t.Fatalf("capability = %#v", value)
	}
}

func TestFeatureInventoryContainsOnlySortedSemanticIDs(t *testing.T) {
	book := &decodedBook{
		entities: map[uint32]map[uint32]*ionValue{260: {}, 157: {}},
		styles:   map[uint32]*ionValue{1: testStruct(testField(42, testInteger(1)), testField(11, testString("serif")))},
		storylines: map[uint32]*ionValue{1: testStruct(testField(146, testList(testStruct(
			testField(159, testSymbol(269)), testField(156, testSymbol(323)),
			testField(615, testSymbol(619)), testField(683, testList(testStruct(testField(687, testSymbol(690))))),
		))))},
		sections:  map[uint32]*ionValue{},
		templates: map[uint32]*ionValue{},
		resources: map[uint32]resource{1: {format: 565}, 2: {format: 284}},
	}
	got := book.featureInventory()
	if fmt.Sprint(got.EntityTypes) != "[157 260]" || fmt.Sprint(got.ContentTypes) != "[269]" ||
		fmt.Sprint(got.Layouts) != "[323]" || fmt.Sprint(got.StyleFields) != "[11 42]" ||
		fmt.Sprint(got.AnnotationTypes) != "[690]" || fmt.Sprint(got.Classifications) != "[619]" ||
		fmt.Sprint(got.ResourceFormats) != "[284 565]" {
		t.Fatalf("inventory = %+v", got)
	}
}

func TestDebugResourceExtensionUsesContentSignatures(t *testing.T) {
	for name, test := range map[string]struct {
		data []byte
		want string
	}{
		"pdf":  {[]byte("%PDF-1.7\n"), "pdf"},
		"jpeg": {[]byte{0xff, 0xd8, 0xff, 0xe0}, "jpg"},
		"font": {[]byte("OTTOfont"), "otf"},
		"raw":  {[]byte("opaque"), "bin"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := debugResourceExtension(test.data); got != test.want {
				t.Fatalf("extension = %q, want %q", got, test.want)
			}
		})
	}
}

func TestEPUBFeatureValidationRejectsUnknownStyleProperty(t *testing.T) {
	book := &decodedBook{styles: map[uint32]*ionValue{1: testStruct(testField(999, testInteger(1)))}}
	err := validateEPUBFeatureSupport(book)
	if err == nil || !strings.Contains(err.Error(), "style property $999") {
		t.Fatalf("validation error = %v", err)
	}
}

func TestResolveResourceUsesLargerVariant(t *testing.T) {
	base := testPNG(t, 10, 10, color.Black)
	high := testPNG(t, 20, 20, color.White)
	book := &decodedBook{
		resources: map[uint32]resource{
			1: {format: 284, location: "base.png", width: 10, height: 10, variants: []uint32{2}},
			2: {format: 284, location: "high.png", width: 20, height: 20},
		},
		rawMedia: map[string][]byte{"base.png": base, "high.png": high},
	}
	metadata, data, ok := book.resolveResource(1)
	if !ok || metadata.location != "high.png" || metadata.width != 20 || !bytes.Equal(data, high) {
		t.Fatalf("resolved metadata = %+v, ok = %t", metadata, ok)
	}
}

func TestResolveResourceCombinesTilesLosslessly(t *testing.T) {
	book := &decodedBook{
		resources: map[uint32]resource{
			1: {
				format: 285, width: 4, height: 2, tileWidth: 2, tileHeight: 2,
				tiles: [][]string{{"left.png", "right.png"}},
			},
		},
		rawMedia: map[string][]byte{
			"left.png":  testPNG(t, 2, 2, color.RGBA{R: 255, A: 255}),
			"right.png": testPNG(t, 2, 2, color.RGBA{B: 255, A: 255}),
		},
	}
	metadata, data, ok := book.resolveResource(1)
	if !ok || metadata.format != 284 || metadata.location == "" {
		t.Fatalf("resolved metadata = %+v, ok = %t", metadata, ok)
	}
	combined, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if combined.Bounds() != image.Rect(0, 0, 4, 2) {
		t.Fatalf("bounds = %v", combined.Bounds())
	}
	if r, _, b, _ := combined.At(0, 0).RGBA(); r == 0 || b != 0 {
		t.Fatalf("left pixel = %#v", combined.At(0, 0))
	}
	if r, _, b, _ := combined.At(3, 0).RGBA(); r != 0 || b == 0 {
		t.Fatalf("right pixel = %#v", combined.At(3, 0))
	}
}

func testImageLayoutBook(nodes ...*ionValue) *decodedBook {
	return &decodedBook{
		document: testStruct(testField(169, testList(testStruct(
			testField(170, testList(testSymbol(10))),
		)))),
		sections: map[uint32]*ionValue{
			10: testStruct(testField(141, testStruct(testField(176, testSymbol(100))))),
		},
		storylines: map[uint32]*ionValue{
			100: testStruct(testField(146, testList(nodes...))),
		},
		resources: map[uint32]resource{
			1: {format: 285, location: "page.jpg"},
		},
		rawMedia:    map[string][]byte{"page.jpg": {0xff, 0xd8, 0xff}},
		metadataRaw: make(map[string]map[string][]*ionValue),
	}
}

func fixedLayoutMetadata(value int64) map[string]map[string][]*ionValue {
	return map[string]map[string][]*ionValue{
		"kindle_capability_metadata": {
			"yj_fixed_layout": {{kind: ionInt, integer: value}},
		},
	}
}

func testImageNode(resourceID uint64) *ionValue {
	return testStruct(testField(159, testSymbol(kfxImageType)), testField(175, testSymbol(resourceID)))
}

func testBackgroundNode(resourceID uint64) *ionValue {
	return testStruct(testField(479, testSymbol(resourceID)))
}

func testTextNode() *ionValue {
	return testStruct(testField(159, testSymbol(kfxTextType)), testField(145, testString("text")))
}

func testStruct(fields ...ionField) *ionValue {
	return &ionValue{kind: ionStruct, fields: fields}
}

func testList(values ...*ionValue) *ionValue {
	return &ionValue{kind: ionList, children: values}
}

func testField(id uint64, value *ionValue) ionField {
	return ionField{id: id, value: value}
}

func testSymbol(id uint64) *ionValue {
	return &ionValue{kind: ionSymbol, unsigned: id}
}

func testInteger(value int64) *ionValue {
	return &ionValue{kind: ionInt, integer: value, unsigned: uint64(value)}
}

func testString(value string) *ionValue {
	return &ionValue{kind: ionString, text: value}
}

func testPNG(t *testing.T, width, height int, fill color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, fill)
		}
	}
	var data bytes.Buffer
	if err := png.Encode(&data, img); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}
