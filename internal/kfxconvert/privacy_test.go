package kfxconvert

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"io"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/pdfcpu/pdfcpu/pkg/api"
)

func TestValidatePrivacyOptions(t *testing.T) {
	for _, pattern := range []string{"", "(", ".*", "^"} {
		if err := ValidatePrivacyOptions(PrivacyOptions{Patterns: []string{pattern}}); err == nil {
			t.Fatalf("pattern %q was accepted", pattern)
		}
	}
	if err := ValidatePrivacyOptions(PrivacyOptions{Patterns: []string{`(?i)jane@example\.com`}}); err != nil {
		t.Fatal(err)
	}
}

func TestRedactTextCleansExternalTitles(t *testing.T) {
	output, err := RedactText("Licensed to Jane Doe — jane@example.com", PrivacyOptions{
		DetectPersonal: true,
		Patterns:       []string{"Jane Doe"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, "Jane") || strings.Contains(output, "example.com") {
		t.Fatalf("output = %q", output)
	}
}

func TestPrivacyDetectsOwnerMetadataAndRedactsText(t *testing.T) {
	owner := &ionValue{kind: ionString, text: "Jane Doe"}
	description := &ionValue{kind: ionString, text: "Delivered to jane@example.com"}
	book := &decodedBook{
		contents: map[uint32][]string{1: {"Licensed to Jane Doe (jane@example.com)"}},
		metadata: map[string]map[string][]string{
			"kindle_title_metadata":   {"title": {"Example"}, "description": {description.text}},
			"kindle_account_metadata": {"owner_name": {owner.text}},
		},
		metadataRaw: map[string]map[string][]*ionValue{
			"kindle_title_metadata":   {"description": {description}},
			"kindle_account_metadata": {"owner_name": {owner}},
		},
		anchors: map[uint32]anchor{1: {externalURL: "mailto:jane@example.com"}},
	}
	report, err := applyPrivacy(book, PrivacyOptions{DetectPersonal: true})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Enabled || report.AutomaticValues != 2 || report.MetadataFields != 1 {
		t.Fatalf("report = %+v", report)
	}
	if _, ok := book.metadata["kindle_account_metadata"]["owner_name"]; ok {
		t.Fatal("owner metadata was retained")
	}
	for _, value := range []string{book.contents[1][0], description.text} {
		if strings.Contains(strings.ToLower(value), "jane") || strings.Contains(value, "example.com") {
			t.Fatalf("personal value was retained in %q", value)
		}
	}
	if book.anchors[1].externalURL != "" {
		t.Fatalf("personal link was retained: %q", book.anchors[1].externalURL)
	}
}

func TestPrivacyDoesNotTreatPublicationContactAsOwner(t *testing.T) {
	description := &ionValue{kind: ionString, text: "Questions: publisher@example.com"}
	book := &decodedBook{
		contents: map[uint32][]string{1: {"Contact publisher@example.com for updates."}},
		metadata: map[string]map[string][]string{
			"kindle_title_metadata": {
				"author":      {"Jane Doe"},
				"description": {description.text},
			},
		},
		metadataRaw: map[string]map[string][]*ionValue{
			"kindle_title_metadata": {"description": {description}},
		},
		anchors: map[uint32]anchor{1: {externalURL: "mailto:publisher@example.com"}},
	}
	report, err := applyPrivacy(book, PrivacyOptions{DetectPersonal: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.AutomaticValues != 0 || report.MetadataFields != 0 {
		t.Fatalf("ordinary publication contact was classified as personal: %+v", report)
	}
	for _, value := range []string{book.contents[1][0], description.text, book.anchors[1].externalURL} {
		if !strings.Contains(value, "publisher@example.com") {
			t.Fatalf("publication contact was redacted: %q", value)
		}
	}
}

func TestPrivacyReplacementPreservesRuneOffsetsAndWhitespace(t *testing.T) {
	redactor := &personalRedactor{patterns: []*regexp.Regexp{regexp.MustCompile(`Jöhn Doe`)}}
	input := "Hi Jöhn Doe!"
	output := redactor.redact(input)
	if utf8.RuneCountInString(input) != utf8.RuneCountInString(output) {
		t.Fatalf("rune count changed: %q -> %q", input, output)
	}
	if output != "Hi ████ ███!" {
		t.Fatalf("output = %q", output)
	}
}

func TestPrivacyRedactsNavigationLabels(t *testing.T) {
	book := testPDFNavigationBook()
	if _, err := applyPrivacy(book, PrivacyOptions{Patterns: []string{"First"}}); err != nil {
		t.Fatal(err)
	}
	builder := epubBuilder{book: book}
	builder.indexSections(readingOrderSections(book.document))
	items := builder.navigationItems()
	if len(items) != 2 || items[0].label != "█████" {
		t.Fatalf("navigation = %+v", items)
	}
}

func TestImageMetadataStrippersPreserveDecodability(t *testing.T) {
	pngData := testPNG(t, 2, 2, color.Black)
	pngData = insertPNGChunk(t, pngData, "tEXt", []byte("Owner\x00Jane Doe"))
	cleanPNG, err := stripPNGMetadata(pngData)
	if err != nil || bytes.Contains(cleanPNG, []byte("Jane Doe")) {
		t.Fatalf("PNG cleanup error = %v, data contains owner = %v", err, bytes.Contains(cleanPNG, []byte("Jane Doe")))
	}
	if _, _, err := image.Decode(bytes.NewReader(cleanPNG)); err != nil {
		t.Fatalf("clean PNG is invalid: %v", err)
	}

	jpegData := testJPEG(t)
	segment := append([]byte{0xff, 0xe1, 0, byte(len("Jane Doe") + 2)}, []byte("Jane Doe")...)
	jpegData = append(append(append([]byte(nil), jpegData[:2]...), segment...), jpegData[2:]...)
	cleanJPEG, err := stripJPEGMetadata(jpegData)
	if err != nil || bytes.Contains(cleanJPEG, []byte("Jane Doe")) {
		t.Fatalf("JPEG cleanup error = %v", err)
	}
	if _, _, err := image.Decode(bytes.NewReader(cleanJPEG)); err != nil {
		t.Fatalf("clean JPEG is invalid: %v", err)
	}

	gifData := testGIF(t)
	comment := []byte{0x21, 0xfe, 8, 'J', 'a', 'n', 'e', ' ', 'D', 'o', 'e', 0}
	gifData = append(append(append([]byte(nil), gifData[:len(gifData)-1]...), comment...), 0x3b)
	cleanGIF, err := stripGIFMetadata(gifData)
	if err != nil || bytes.Contains(cleanGIF, []byte("Jane Doe")) {
		t.Fatalf("GIF cleanup error = %v", err)
	}
	if _, err := gif.DecodeAll(bytes.NewReader(cleanGIF)); err != nil {
		t.Fatalf("clean GIF is invalid: %v", err)
	}
}

func TestWebPAndMediaMetadataStrippers(t *testing.T) {
	webp := riffFile("WEBP", riffChunk("VP8X", append([]byte{0x0c}, make([]byte, 9)...)), riffChunk("EXIF", []byte("Jane Doe")))
	cleanWebP, err := stripWebPMetadata(webp)
	if err != nil || bytes.Contains(cleanWebP, []byte("Jane Doe")) || cleanWebP[20]&0x0c != 0 {
		t.Fatalf("WebP cleanup failed: %v, %x", err, cleanWebP)
	}

	mp3 := append([]byte("ID3\x04\x00\x00\x00\x00\x00\x08Jane Doe"), []byte{0xff, 0xfb, 0x90, 0x64}...)
	cleanMP3, err := stripMP3Metadata(mp3)
	if err != nil || !bytes.Equal(cleanMP3, []byte{0xff, 0xfb, 0x90, 0x64}) {
		t.Fatalf("MP3 cleanup = %x, err = %v", cleanMP3, err)
	}

	wav := riffFile("WAVE", riffChunk("fmt ", make([]byte, 16)), riffChunk("LIST", append([]byte("INFO"), []byte("Jane Doe")...)), riffChunk("data", []byte{1, 2, 3, 4}))
	cleanWAV, err := stripWAVMetadata(wav)
	if err != nil || bytes.Contains(cleanWAV, []byte("Jane Doe")) || !bytes.Contains(cleanWAV, []byte("data")) {
		t.Fatalf("WAV cleanup failed: %v, %q", err, cleanWAV)
	}

	mp4 := append(mp4Atom("ftyp", []byte("isom\x00\x00\x00\x00isom")),
		mp4Atom("moov", append(mp4Atom("udta", []byte("Jane Doe")), mp4Atom("trak", []byte{})...))...)
	mp4 = append(mp4, mp4Atom("mdat", []byte{1, 2, 3, 4})...)
	cleanMP4, err := stripMP4Metadata(mp4)
	if err != nil || len(cleanMP4) != len(mp4) || bytes.Contains(cleanMP4, []byte("Jane Doe")) || !bytes.Contains(cleanMP4, []byte("mdat")) {
		t.Fatalf("MP4 cleanup failed: %v", err)
	}
}

func TestSanitizePDFBytesRemovesPropertiesAndKeepsPages(t *testing.T) {
	imageData := testPNG(t, 2, 2, color.Black)
	var source bytes.Buffer
	if err := api.ImportImages(nil, &source, []io.Reader{bytes.NewReader(imageData)}, nil, pdfConfiguration()); err != nil {
		t.Fatal(err)
	}
	var personalized bytes.Buffer
	if err := api.AddProperties(bytes.NewReader(source.Bytes()), &personalized,
		map[string]string{"Author": "Jane Doe", "OwnerEmail": "jane@example.com"}, pdfConfiguration()); err != nil {
		t.Fatal(err)
	}
	clean, err := sanitizePDFBytes(personalized.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(clean, []byte("Jane Doe")) || bytes.Contains(clean, []byte("jane@example.com")) {
		t.Fatal("personal PDF metadata remains in output bytes")
	}
	info, err := api.PDFInfo(bytes.NewReader(clean), "", nil, false, pdfConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	if info.Author != "" || info.Title != "" || info.Subject != "" || info.Creator != "" || len(info.Keywords) != 0 {
		t.Fatalf("standard PDF metadata remains: %+v", info)
	}
	if pages, err := api.PageCount(bytes.NewReader(clean), pdfConfiguration()); err != nil || pages != 1 {
		t.Fatalf("page count = %d, err = %v", pages, err)
	}
}

func insertPNGChunk(t *testing.T, data []byte, name string, payload []byte) []byte {
	t.Helper()
	offset := bytes.LastIndex(data, []byte("IEND")) - 4
	if offset < 8 {
		t.Fatal("test PNG has no IEND")
	}
	chunk := make([]byte, 12+len(payload))
	binary.BigEndian.PutUint32(chunk[:4], uint32(len(payload)))
	copy(chunk[4:8], name)
	copy(chunk[8:], payload)
	binary.BigEndian.PutUint32(chunk[len(chunk)-4:], crc32.ChecksumIEEE(chunk[4:len(chunk)-4]))
	return append(append(append([]byte(nil), data[:offset]...), chunk...), data[offset:]...)
}

func testJPEG(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	if err := jpeg.Encode(&output, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func testGIF(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	if err := gif.Encode(&output, image.NewPaletted(image.Rect(0, 0, 2, 2), color.Palette{color.Black}), nil); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func riffChunk(name string, payload []byte) []byte {
	result := make([]byte, 8+len(payload)+len(payload)%2)
	copy(result[:4], name)
	binary.LittleEndian.PutUint32(result[4:8], uint32(len(payload)))
	copy(result[8:], payload)
	return result
}

func riffFile(kind string, chunks ...[]byte) []byte {
	result := []byte("RIFF\x00\x00\x00\x00" + kind)
	for _, chunk := range chunks {
		result = append(result, chunk...)
	}
	binary.LittleEndian.PutUint32(result[4:8], uint32(len(result)-8))
	return result
}

func mp4Atom(name string, payload []byte) []byte {
	result := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(result[:4], uint32(len(result)))
	copy(result[4:8], name)
	copy(result[8:], payload)
	return result
}
