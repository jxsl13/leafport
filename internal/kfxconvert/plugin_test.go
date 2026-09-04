package kfxconvert

import (
	"archive/zip"
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecodePluginManifestPreservesTypeAndValues(t *testing.T) {
	pluginType, manifest, err := decodePluginManifest([]byte(`audio::{facets:{media:{uri:"kfx://sound"}},properties:{enabled:true}}`))
	if err != nil {
		t.Fatal(err)
	}
	if pluginType != "audio" {
		t.Fatalf("plugin type = %q", pluginType)
	}
	facets, ok := pluginMap(manifest["facets"])
	if !ok {
		t.Fatalf("facets = %#v", manifest["facets"])
	}
	media, ok := pluginMap(facets["media"])
	uri, uriOK := pluginString(media["uri"])
	if !ok || !uriOK || uri != "kfx://sound" {
		t.Fatalf("media = %#v", facets["media"])
	}
}

func TestRenderAudioPluginEmbedsMedia(t *testing.T) {
	builder := pluginTestBuilder(`audio::{facets:{media:{uri:"kfx://sound"},player:{}},properties:{}}`, map[uint32]resource{
		2: {format: 999, location: "sound", mime: "audio/mpeg"},
	}, map[string][]byte{"sound": []byte("ID3audio")})
	node := testStruct(testField(155, testInteger(45)), testField(159, testSymbol(274)), testField(175, testSymbol(1)), testField(584, testString("Spoken text")))
	got, _, err := builder.renderValues(node, 1, map[uint32]bool{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, wanted := range []string{`<audio id="kfx-node-45" controls=""`, `src="../media/plugin-0001.mp3"`, `Spoken text</audio>`} {
		if !strings.Contains(got, wanted) {
			t.Fatalf("audio output lacks %q: %s", wanted, got)
		}
	}
	if len(builder.assetOrder) != 1 || builder.assetOrder[0].mediaType != "audio/mpeg" || builder.assetOrder[0].image {
		t.Fatalf("audio assets = %+v", builder.assetOrder)
	}
}

func TestRenderVideoPluginPreservesPlaybackOptionsAndPoster(t *testing.T) {
	mp4 := make([]byte, 16)
	copy(mp4[4:8], "ftyp")
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...)
	builder := pluginTestBuilder(`video::{
		facets:{media:{uri:"kfx://movie"},poster:{uri:"kfx://poster"}},
		properties:{user_interaction:enabled,play_context:{loop_count:-1}},
		events:{enter_view:{name:start}}
	}`, map[uint32]resource{
		2: {format: 999, location: "movie", mime: "video/mp4"},
		3: {format: 284, location: "poster", mime: "image/png"},
	}, map[string][]byte{"movie": mp4, "poster": png})
	node := testStruct(testField(159, testSymbol(274)), testField(175, testSymbol(1)))
	got, _, err := builder.renderValues(node, 1, map[uint32]bool{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, wanted := range []string{`<video controls="" autoplay="" loop=""`, `poster="../images/plugin-0002.png"`, `src="../media/plugin-0001.mp4"`} {
		if !strings.Contains(got, wanted) {
			t.Fatalf("video output lacks %q: %s", wanted, got)
		}
	}
	if len(builder.assetOrder) != 2 || builder.imageCount() != 1 {
		t.Fatalf("video assets = %+v", builder.assetOrder)
	}
}

func TestRenderImageSequenceDeduplicatesRepeatedResources(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...)
	builder := pluginTestBuilder(`image_sequence::{facets:{images:[{uri:"kfx://frame"},{uri:"kfx://frame"}]},properties:{}}`, map[uint32]resource{
		2: {format: 284, location: "frame", mime: "image/png"},
	}, map[string][]byte{"frame": png})
	node := testStruct(testField(159, testSymbol(274)), testField(175, testSymbol(1)), testField(584, testString("Frame")))
	got, _, err := builder.renderValues(node, 1, map[uint32]bool{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(got, `src="../images/plugin-0001.png"`) != 2 || len(builder.assetOrder) != 1 {
		t.Fatalf("image sequence = %s; assets = %+v", got, builder.assetOrder)
	}
}

func TestRenderNestedSlideshowPreservesBoundsAndVisibility(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...)
	builder := pluginTestBuilder(`slideshow::{
		facets:{children:[{uri:"kfx://child",bounds:{x:{value:10,unit:percent},y:{value:20,unit:px},w:{value:50,unit:percent}}}]},
		properties:{initial_visibility:hide,alt_text:"Slide"}
	}`, map[uint32]resource{
		2: {format: 287, location: "child"},
		3: {format: 284, location: "slide", mime: "image/png"},
	}, map[string][]byte{
		"child": []byte(`zoomable::{facets:{media:{uri:"kfx://slide"}},properties:{}}`),
		"slide": png,
	})
	node := testStruct(testField(159, testSymbol(274)), testField(175, testSymbol(1)))
	got, _, err := builder.renderValues(node, 1, map[uint32]bool{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, wanted := range []string{`style="visibility:hidden"`, `left:10%;top:20px;width:50%;position:absolute`, `alt="Slide"`} {
		if !strings.Contains(got, wanted) {
			t.Fatalf("slideshow lacks %q: %s", wanted, got)
		}
	}
}

func TestPluginRenderingFailsClosedForWebviewAndCycles(t *testing.T) {
	t.Run("webview", func(t *testing.T) {
		builder := pluginTestBuilder(`webview::{facets:{uri:"kfx://article"},properties:{}}`, nil, nil)
		node := testStruct(testField(159, testSymbol(274)), testField(175, testSymbol(1)))
		_, _, err := builder.renderValues(node, 1, map[uint32]bool{}, 0)
		if err == nil || !strings.Contains(err.Error(), "webview") {
			t.Fatalf("webview error = %v", err)
		}
	})
	t.Run("cycle", func(t *testing.T) {
		builder := pluginTestBuilder(`slideshow::{facets:{children:[{uri:"kfx://plugin"}]},properties:{}}`, nil, nil)
		node := testStruct(testField(159, testSymbol(274)), testField(175, testSymbol(1)))
		_, _, err := builder.renderValues(node, 1, map[uint32]bool{}, 0)
		if err == nil || !strings.Contains(err.Error(), "cycle") {
			t.Fatalf("cycle error = %v", err)
		}
	})
}

func TestEPUBReferenceInspectionIncludesEmbeddedMediaAttributes(t *testing.T) {
	document, err := inspectEPUBXML(strings.NewReader(`<html><body><video src="../media/missing.mp4" poster="../images/missing.png"></video></body></html>`))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]bool{"OEBPS/text/section.xhtml": true}
	err = validateEPUBReferences(files, map[string]epubXMLDocument{"OEBPS/text/section.xhtml": document})
	if err == nil || !strings.Contains(err.Error(), "missing file") {
		t.Fatalf("reference validation error = %v", err)
	}
}

func TestPluginMediaTypeRejectsMismatchedKinds(t *testing.T) {
	if _, _, ok := pluginMediaType([]byte("ID3audio"), "audio/mpeg", "video"); ok {
		t.Fatal("accepted audio as video")
	}
	data := make([]byte, 16)
	copy(data[4:8], "ftyp")
	extension, mediaType, ok := pluginMediaType(data, "", "audio")
	if !ok || extension != "m4a" || mediaType != "audio/mp4" {
		t.Fatalf("MP4 audio type = %q, %q, %t", extension, mediaType, ok)
	}
}

func TestConvertPluginBookPackagesAndValidatesMedia(t *testing.T) {
	builder := pluginTestBuilder(`audio::{facets:{media:{uri:"kfx://sound"},player:{}},properties:{}}`, map[uint32]resource{
		2: {format: 999, location: "sound", mime: "audio/mpeg"},
	}, map[string][]byte{"sound": []byte("ID3audio")})
	node := testStruct(testField(159, testSymbol(274)), testField(175, testSymbol(1)), testField(584, testString("Audio")))
	builder.book.document = testStruct(testField(169, testList(testStruct(testField(170, testList(testSymbol(10)))))))
	builder.book.sections = map[uint32]*ionValue{10: testStruct(testField(141, testStruct(testField(176, testSymbol(20)))))}
	builder.book.storylines = map[uint32]*ionValue{20: testStruct(testField(146, testList(node)))}

	destination := filepath.Join(t.TempDir(), "plugin.epub")
	result, err := convertBookToEPUB(builder.book, destination, Metadata{Identifier: "plugin", Title: "Plugin", Language: "en"})
	if err != nil {
		t.Fatal(err)
	}
	// A publication without a declared cover gets a cover generated from its
	// first non-empty spine page, without a duplicate HTML cover document.
	if result.Sections != 1 || result.Images != 1 || result.Media != 1 || result.Fonts != 0 {
		t.Fatalf("EPUB result = %+v", result)
	}
	archive, err := zip.OpenReader(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	foundMedia, manifested := false, false
	for _, file := range archive.File {
		if file.Name == "OEBPS/media/plugin-0001.mp3" {
			foundMedia = true
		}
		if file.Name == "OEBPS/content.opf" {
			data, err := readZipFile(file)
			if err != nil {
				t.Fatal(err)
			}
			manifested = bytes.Contains(data, []byte(`href="media/plugin-0001.mp3" media-type="audio/mpeg"`))
		}
	}
	if !foundMedia || !manifested {
		t.Fatalf("media file found=%t, manifested=%t", foundMedia, manifested)
	}
}

func pluginTestBuilder(manifest string, resources map[uint32]resource, rawMedia map[string][]byte) *epubBuilder {
	allResources := map[uint32]resource{1: {format: 287, location: "plugin"}}
	for id, metadata := range resources {
		allResources[id] = metadata
	}
	allMedia := map[string][]byte{"plugin": []byte(manifest)}
	for name, data := range rawMedia {
		allMedia[name] = bytes.Clone(data)
	}
	return &epubBuilder{
		book: &decodedBook{
			resources: allResources,
			rawMedia:  allMedia,
			anchors:   map[uint32]anchor{},
			templates: map[uint32]*ionValue{},
		},
		assets: map[uint32]*epubAsset{},
	}
}
