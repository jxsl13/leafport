package kfxconvert

import (
	"strings"
	"testing"
)

func TestTextTransformMatchesKFXSemantics(t *testing.T) {
	for symbol, want := range map[uint64]string{
		349: "none", 372: "uppercase", 373: "lowercase", 374: "capitalize",
	} {
		book := decodedBook{styles: map[uint32]*ionValue{
			1: testStruct(testField(41, testSymbol(symbol))),
		}}
		if got := book.styleProperties(1, make(map[uint32]bool))["text-transform"]; got != want {
			t.Errorf("text-transform $%d = %q, want %q", symbol, got, want)
		}
	}
	if !supportedEPUBStyleField(41) {
		t.Fatal("KFX text-transform field $41 is not accepted")
	}
}

func TestRenderStyledTextPreservesExternalLinkAndStyle(t *testing.T) {
	event := &ionValue{kind: ionStruct, fields: []ionField{
		{id: 143, value: &ionValue{kind: ionInt, integer: 1, unsigned: 1}},
		{id: 144, value: &ionValue{kind: ionInt, integer: 3, unsigned: 3}},
		{id: 157, value: &ionValue{kind: ionSymbol, unsigned: 900}},
		{id: 179, value: &ionValue{kind: ionSymbol, unsigned: 901}},
	}}
	node := &ionValue{kind: ionStruct, fields: []ionField{{
		id: 142, value: &ionValue{kind: ionList, children: []*ionValue{event}},
	}}}
	builder := epubBuilder{book: &decodedBook{anchors: map[uint32]anchor{
		901: {externalURL: "https://example.com/read"},
	}}}
	got, err := builder.renderStyledText(node, "hello")
	if err != nil {
		t.Fatal(err)
	}
	want := `h<a href="https://example.com/read"><span class="kfx-s900">ell</span></a>o`
	if got != want {
		t.Fatalf("styled text = %q, want %q", got, want)
	}
}

func TestRenderStyledTextPreservesNoteSemantics(t *testing.T) {
	event := testStruct(
		testField(143, testInteger(1)), testField(144, testInteger(3)),
		testField(179, testSymbol(901)), testField(616, testSymbol(617)),
	)
	node := testStruct(testField(142, testList(event)))
	builder := epubBuilder{book: &decodedBook{anchors: map[uint32]anchor{
		901: {externalURL: "https://example.com/note"},
	}}}
	got, err := builder.renderStyledText(node, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if got != `h<a epub:type="noteref" href="https://example.com/note">ell</a>o` {
		t.Fatalf("note reference = %q", got)
	}

	direct := testStruct(testField(179, testSymbol(901)), testField(616, testSymbol(617)))
	got, err = builder.renderStyledText(direct, "note")
	if err != nil || got != `<a epub:type="noteref" href="https://example.com/note">note</a>` {
		t.Fatalf("direct note reference = %q, %v", got, err)
	}
}

func TestRenderTextPreservesEndnoteClassification(t *testing.T) {
	node := testStruct(
		testField(159, testSymbol(269)), testField(145, testString("Endnote text")),
		testField(615, testSymbol(619)),
	)
	builder := epubBuilder{book: &decodedBook{anchors: map[uint32]anchor{}}}
	got, _, err := builder.renderValues(node, 1, map[uint32]bool{}, 0)
	if err != nil || got != `<aside epub:type="endnote">Endnote text</aside>` {
		t.Fatalf("endnote = %q, %v", got, err)
	}
}

func TestLinkTargetResolvesInternalNode(t *testing.T) {
	builder := epubBuilder{
		book:         &decodedBook{anchors: map[uint32]anchor{5: {targetNode: 42}}},
		nodeSections: map[uint32]int{42: 3},
	}
	got := builder.linkTarget(&ionValue{kind: ionSymbol, unsigned: 5})
	if got != "section-0003.xhtml#kfx-node-42" {
		t.Fatalf("internal link = %q", got)
	}
	if got := safeExternalURL("javascript:alert(1)"); got != "" {
		t.Fatalf("unsafe URL accepted: %q", got)
	}
}

func TestLinkTargetSeparatesSectionSymbolsFromNumericNodeIDs(t *testing.T) {
	builder := epubBuilder{
		book: &decodedBook{anchors: map[uint32]anchor{
			1: {targetNode: 42},
			2: {targetNode: 42, targetSymbol: true},
		}},
		nodeSections:    map[uint32]int{42: 1},
		sectionSections: map[uint32]int{42: 2},
	}
	if got := builder.linkTarget(testSymbol(1)); got != "section-0001.xhtml#kfx-node-42" {
		t.Fatalf("numeric node target = %q", got)
	}
	if got := builder.linkTarget(testSymbol(2)); got != "section-0002.xhtml" {
		t.Fatalf("section-symbol target = %q", got)
	}
}

func TestInternalTextOffsetProducesAndTargetsExactAnchor(t *testing.T) {
	node := testStruct(
		testField(155, testSymbol(42)),
		testField(145, testString("hello")),
	)
	builder := epubBuilder{
		book:         &decodedBook{anchors: map[uint32]anchor{5: {targetNode: 42, offset: 2}}},
		nodeSections: map[uint32]int{42: 3}, nodeTextRunes: map[uint32]int{42: 5},
	}
	if got := builder.linkTarget(testSymbol(5)); got != "section-0003.xhtml#kfx-anchor-5" {
		t.Fatalf("internal offset link = %q", got)
	}
	got, err := builder.renderStyledText(node, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if got != `he<span id="kfx-anchor-5"></span>llo` {
		t.Fatalf("anchored text = %q", got)
	}
}

func TestRenderStyledTextReconstructsRubyAnnotation(t *testing.T) {
	event := testStruct(
		testField(143, testInteger(0)),
		testField(144, testInteger(2)),
		testField(757, testSymbol(500)),
		testField(758, testInteger(7)),
	)
	node := testStruct(testField(142, testList(event)))
	book := &decodedBook{
		entities: map[uint32]map[uint32]*ionValue{
			756: {500: testStruct(testField(146, testList(testStruct(
				testField(758, testInteger(7)),
				testField(159, testSymbol(269)),
				testField(145, testString("かんじ")),
			))))},
		},
		anchors: map[uint32]anchor{}, templates: map[uint32]*ionValue{}, storylines: map[uint32]*ionValue{},
	}
	got, err := (&epubBuilder{book: book}).renderStyledText(node, "漢字")
	if err != nil {
		t.Fatal(err)
	}
	if got != `<ruby><rb>漢字</rb><rt>かんじ</rt></ruby>` {
		t.Fatalf("ruby output = %q", got)
	}
}

func TestMathMLAnnotationIsSanitizedAndManifested(t *testing.T) {
	annotation := testStruct(
		testField(687, testSymbol(690)),
		testField(145, testString(`<math xmlns="http://www.w3.org/1998/Math/MathML" class="source"><mi amzn-src-id="1">x</mi></math>`)),
	)
	node := testStruct(
		testField(155, testInteger(12)),
		testField(159, testSymbol(270)),
		testField(683, testList(annotation)),
	)
	builder := epubBuilder{book: &decodedBook{anchors: map[uint32]anchor{}, templates: map[uint32]*ionValue{}}}
	got, _, err := builder.renderValues(node, 1, map[uint32]bool{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"source", "amzn-src-id"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("MathML contains %q: %s", unwanted, got)
		}
	}
	if !strings.Contains(got, mathMLNamespace) || !strings.Contains(got, `alttext=""`) {
		t.Fatalf("MathML output = %s", got)
	}
	if got := sectionManifestProperties([]byte(got)); got != ` properties="mathml"` {
		t.Fatalf("manifest properties = %q", got)
	}
}

func TestRenderKVGProducesEPUBSVG(t *testing.T) {
	dimension := func(number int64) *ionValue {
		return testStruct(testField(306, testSymbol(319)), testField(307, testInteger(number)))
	}
	path := testStruct(
		testField(159, testSymbol(273)),
		testField(249, testList(
			testInteger(0), testInteger(0), testInteger(0),
			testInteger(1), testInteger(100), testInteger(200),
			testInteger(4),
		)),
		testField(70, testInteger(int64(0xffff0000))),
	)
	node := testStruct(
		testField(155, testInteger(9)),
		testField(159, testSymbol(272)),
		testField(66, dimension(100)),
		testField(67, dimension(200)),
		testField(146, testList()),
		testField(250, testList(path)),
	)
	builder := epubBuilder{book: &decodedBook{anchors: map[uint32]anchor{}, entities: map[uint32]map[uint32]*ionValue{}}}
	got, _, err := builder.renderValues(node, 1, map[uint32]bool{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, wanted := range []string{
		`xmlns="http://www.w3.org/2000/svg"`, `viewBox="0 0 100 200"`,
		`<path d="M 0 0 L 100 200 Z" fill="#ff0000"/>`, `id="kfx-node-9"`,
	} {
		if !strings.Contains(got, wanted) {
			t.Fatalf("KVG output missing %q: %s", wanted, got)
		}
	}
	if got := sectionManifestProperties([]byte(got)); got != ` properties="svg"` {
		t.Fatalf("manifest properties = %q", got)
	}
}

func TestValidateEPUBReferencesRejectsMissingFragment(t *testing.T) {
	files := map[string]bool{"OEBPS/nav.xhtml": true, "OEBPS/text/section.xhtml": true}
	documents := map[string]epubXMLDocument{
		"OEBPS/nav.xhtml":          {ids: map[string]bool{}, hrefs: []string{"text/section.xhtml#missing"}},
		"OEBPS/text/section.xhtml": {ids: map[string]bool{"present": true}},
	}
	if err := validateEPUBReferences(files, documents); err == nil || !strings.Contains(err.Error(), "missing fragment") {
		t.Fatalf("validation error = %v", err)
	}
	documents["OEBPS/nav.xhtml"] = epubXMLDocument{
		ids: map[string]bool{}, hrefs: []string{"text/section.xhtml#present"},
	}
	if err := validateEPUBReferences(files, documents); err != nil {
		t.Fatal(err)
	}
}

func TestEPUBFeatureAuditAllowsPluginsButRejectsKnownLossyContent(t *testing.T) {
	book := &decodedBook{storylines: map[uint32]*ionValue{1: testStruct(
		testField(146, &ionValue{kind: ionList, children: []*ionValue{
			testStruct(testField(159, testSymbol(274))),
			testStruct(testField(159, testSymbol(270)), testField(171, testSymbol(99))),
		}}),
	)}}
	err := validateEPUBFeatureSupport(book)
	if err == nil || !strings.Contains(err.Error(), "conditional page-template") || strings.Contains(err.Error(), "audio/video") {
		t.Fatalf("feature audit error = %v", err)
	}
}

func TestRenderListsHiddenContentAndInlineContainers(t *testing.T) {
	builder := epubBuilder{book: &decodedBook{anchors: map[uint32]anchor{}}}
	ordered := testStruct(
		testField(159, testSymbol(276)),
		testField(100, testSymbol(346)),
		testField(104, &ionValue{kind: ionInt, integer: 3, unsigned: 3}),
	)
	got, _, err := builder.renderValues(ordered, 1, map[uint32]bool{}, 0)
	if err != nil || got != `<ol start="3"></ol>` {
		t.Fatalf("ordered list = %q, %v", got, err)
	}
	hidden := testStruct(testField(159, testSymbol(439)))
	got, _, err = builder.renderValues(hidden, 1, map[uint32]bool{}, 0)
	if err != nil || got != `<div style="display:none"></div>` {
		t.Fatalf("hidden content = %q, %v", got, err)
	}
	inline := testStruct(testField(159, testSymbol(270)), testField(601, testSymbol(283)))
	got, _, err = builder.renderValues(inline, 1, map[uint32]bool{}, 0)
	if err != nil || got != `<span></span>` {
		t.Fatalf("inline container = %q, %v", got, err)
	}
}

func TestRenderTablePreservesColumnsSpacingAndAccessibility(t *testing.T) {
	dimension := func(number float64) *ionValue {
		return testStruct(testField(306, testSymbol(314)), ionField{id: 307, value: &ionValue{kind: ionFloat, floating: number}})
	}
	annotation := testStruct(testField(687, testSymbol(584)), testField(145, testString("Nährwerte")))
	table := testStruct(
		testField(159, testSymbol(278)),
		testField(150, &ionValue{kind: ionBool, boolean: true}),
		testField(152, testList(
			testStruct(testField(56, dimension(70))),
			testStruct(testField(56, dimension(30)), testField(118, testInteger(2))),
		)),
		testField(456, testStruct(testField(306, testSymbol(318)), testField(307, testInteger(1)))),
		testField(457, testStruct(testField(306, testSymbol(318)), testField(307, testInteger(2)))),
		testField(683, testList(annotation)),
	)
	builder := epubBuilder{book: &decodedBook{anchors: map[uint32]anchor{}, templates: map[uint32]*ionValue{}}}
	got, _, err := builder.renderValues(table, 1, map[uint32]bool{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, wanted := range []string{
		`aria-label="Nährwerte"`, `border-collapse:collapse`, `border-spacing:2pt 1pt`,
		`<col style="width:70%"/>`, `<col span="2" style="width:30%"/>`,
	} {
		if !strings.Contains(got, wanted) {
			t.Fatalf("table output missing %q: %s", wanted, got)
		}
	}
}

func TestRenderTableTreatsKnownContainersAsCells(t *testing.T) {
	cell := testStruct(
		testField(159, testSymbol(270)),
		testField(146, testList(testStruct(
			testField(159, testSymbol(269)),
			testField(145, testString("value")),
		))),
	)
	row := testStruct(
		testField(159, testSymbol(279)),
		testField(146, testList(cell)),
	)
	builder := epubBuilder{book: &decodedBook{anchors: map[uint32]anchor{}, templates: map[uint32]*ionValue{}}}
	got, _, err := builder.renderValues(row, 1, map[uint32]bool{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != `<tr><td><p>value</p></td></tr>` {
		t.Fatalf("table row = %q", got)
	}
}

func TestRenderTableResolvesInheritedCellSpan(t *testing.T) {
	cell := testStruct(
		testField(157, testSymbol(2)), testField(159, testSymbol(270)),
		testField(146, testList(testStruct(testField(159, testSymbol(269)), testField(145, testString("value"))))),
	)
	row := testStruct(testField(159, testSymbol(279)), testField(146, testList(cell)))
	book := &decodedBook{
		styles: map[uint32]*ionValue{
			1: testStruct(testField(149, testInteger(2))),
			2: testStruct(testField(158, testSymbol(1))),
		},
		anchors: map[uint32]anchor{}, templates: map[uint32]*ionValue{},
	}
	got, _, err := (&epubBuilder{book: book}).renderValues(row, 1, map[uint32]bool{}, 0)
	if err != nil || !strings.Contains(got, `<td class="kfx-s2" rowspan="2">`) {
		t.Fatalf("spanning table cell = %q, %v", got, err)
	}
}

func TestNavigationUsesRenderedReadingOrder(t *testing.T) {
	entry := func(label string, target int64) *ionValue {
		return testStruct(
			testField(241, testStruct(testField(244, testString(label)))),
			testField(246, testStruct(testField(155, testInteger(target)))),
		)
	}
	builder := epubBuilder{
		book:          &decodedBook{},
		nodeSections:  map[uint32]int{10: 3, 20: 2, 30: 2},
		nodePositions: map[uint32]int{10: 1, 20: 9, 30: 4},
	}
	items := builder.parseNavigationEntries(testList(
		entry("third section", 10),
		entry("later in second", 20),
		entry("earlier in second", 30),
	), 212)
	if len(items) != 3 || items[0].label != "earlier in second" || items[1].label != "later in second" || items[2].label != "third section" {
		t.Fatalf("navigation order = %+v", items)
	}
}

func TestNavigationPreservesLandmarksPagesAndOffsets(t *testing.T) {
	entry := func(label string, target, offset int64, landmark uint64) *ionValue {
		fields := []ionField{
			testField(241, testStruct(testField(244, testString(label)))),
			testField(246, testStruct(testField(143, testInteger(offset)), testField(155, testInteger(target)))),
		}
		if landmark != 0 {
			fields = append(fields, testField(238, testSymbol(landmark)))
		}
		return testStruct(fields...)
	}
	container := func(kind uint64, entries ...*ionValue) *ionValue {
		return testStruct(testField(235, testSymbol(kind)), testField(247, testList(entries...)))
	}
	book := &decodedBook{
		navigation: testList(testStruct(testField(392, testList(
			container(212, entry("Chapter", 10, 0, 0)),
			container(236, entry("Cover", 10, 0, 233), entry("", 11, 0, 396)),
			container(237, entry("7", 10, 2, 0)),
		)))),
		anchors: map[uint32]anchor{},
	}
	builder := epubBuilder{
		book: book, nodeSections: map[uint32]int{10: 1, 11: 2},
		nodePositions: map[uint32]int{10: 1, 11: 1}, nodeTextRunes: map[uint32]int{10: 5},
	}
	navigation := builder.navigationDocument()
	if len(navigation.toc) != 1 || len(navigation.landmarks) != 2 || len(navigation.pages) != 1 {
		t.Fatalf("navigation = %+v", navigation)
	}
	if navigation.landmarks[0].epubType != "cover" || navigation.landmarks[1].epubType != "bodymatter" ||
		navigation.landmarks[1].label != "Beginning" || !strings.Contains(navigation.pages[0].href, "#kfx-nav-") {
		t.Fatalf("supplemental navigation = %+v / %+v", navigation.landmarks, navigation.pages)
	}
	node := testStruct(testField(155, testInteger(10)))
	text, err := builder.renderStyledText(node, "hello")
	if err != nil || !strings.Contains(text, `he<span id="kfx-nav-`) {
		t.Fatalf("position anchor = %q, %v", text, err)
	}
	nav := buildNavigation("Book", "en", nil, navigation)
	for _, wanted := range []string{`epub:type="landmarks"`, `epub:type="cover"`, `epub:type="bodymatter"`, `epub:type="page-list"`} {
		if !strings.Contains(nav, wanted) {
			t.Fatalf("nav document missing %q: %s", wanted, nav)
		}
	}
}

func TestRenderExpandsNamedTemplate(t *testing.T) {
	builder := epubBuilder{book: &decodedBook{
		templates: map[uint32]*ionValue{7: testStruct(
			testField(159, testSymbol(269)),
			testField(145, testString("from template")),
		)},
		anchors: map[uint32]anchor{},
	}}
	got, _, err := builder.renderValues(testSymbol(7), 1, map[uint32]bool{}, 0)
	if err != nil || got != `<p>from template</p>` {
		t.Fatalf("template output = %q, %v", got, err)
	}
}

func TestIndexAndRenderUseKFXContentBranchPrecedence(t *testing.T) {
	nestedNode := testStruct(testField(155, testSymbol(99)), testField(159, testSymbol(269)), testField(145, testString("unused")))
	visibleNode := testStruct(testField(155, testSymbol(42)), testField(159, testSymbol(269)), testField(145, testString("visible")))
	book := &decodedBook{
		storylines: map[uint32]*ionValue{
			1: testStruct(testField(146, testList(testStruct(
				testField(146, testList(visibleNode)),
				testField(176, testSymbol(2)),
			)))),
			2: testStruct(testField(146, testList(nestedNode))),
		},
		sections: map[uint32]*ionValue{10: testStruct(testField(141, testStruct(testField(176, testSymbol(1)))))},
		document: testStruct(testField(169, testList(testStruct(testField(170, testList(testSymbol(10))))))),
		anchors:  map[uint32]anchor{}, templates: map[uint32]*ionValue{},
	}
	builder := epubBuilder{book: book, assets: map[uint32]*epubAsset{}}
	builder.indexSections([]uint32{10})
	if builder.nodeSections[42] != 1 {
		t.Fatalf("visible node section = %d", builder.nodeSections[42])
	}
	if builder.nodeSections[99] != 0 {
		t.Fatalf("unused nested branch was indexed in section %d", builder.nodeSections[99])
	}
	section, err := builder.renderSection(10, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(section.data), "visible") || strings.Contains(string(section.data), "unused") {
		t.Fatalf("section content = %s", section.data)
	}
}

func TestEPUBDoesNotDiscardKFXIgnoreContainer(t *testing.T) {
	book := &decodedBook{anchors: map[uint32]anchor{}, templates: map[uint32]*ionValue{}}
	builder := epubBuilder{book: book, assets: map[uint32]*epubAsset{}}
	node := testStruct(
		testField(69, &ionValue{kind: ionBool, boolean: true}),
		testField(155, testSymbol(77)),
		testField(159, testSymbol(270)),
		testField(146, testList(testStruct(testField(159, testSymbol(269)), testField(145, testString("kept"))))),
	)
	got, _, err := builder.renderValues(node, 1, map[uint32]bool{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `id="kfx-node-77"`) || !strings.Contains(got, "kept") {
		t.Fatalf("ignored container content was discarded: %s", got)
	}
}

func TestStyleSheetIncludesInheritedProperties(t *testing.T) {
	dimension := func(unit uint64, number float64) *ionValue {
		return &ionValue{kind: ionStruct, fields: []ionField{
			{id: 306, value: &ionValue{kind: ionSymbol, unsigned: unit}},
			{id: 307, value: &ionValue{kind: ionFloat, floating: number}},
		}}
	}
	book := &decodedBook{styles: map[uint32]*ionValue{
		1: {kind: ionStruct, fields: []ionField{{id: 13, value: &ionValue{kind: ionSymbol, unsigned: 361}}}},
		2: {kind: ionStruct, fields: []ionField{
			{id: 158, value: &ionValue{kind: ionSymbol, unsigned: 1}},
			{id: 16, value: dimension(505, 1.25)},
		}},
	}}
	css := (&epubBuilder{book: book}).styleSheet()
	if !strings.Contains(css, ".kfx-s2 {font-size:1.25rem;font-weight:700;}") {
		t.Fatalf("stylesheet does not contain inherited properties:\n%s", css)
	}
}

func TestStyleSheetPreservesExtendedKFXPresentation(t *testing.T) {
	dimension := func(unit uint64, number float64) *ionValue {
		return testStruct(
			testField(306, testSymbol(unit)),
			ionField{id: 307, value: &ionValue{kind: ionFloat, floating: number}},
		)
	}
	book := &decodedBook{styles: map[uint32]*ionValue{3: testStruct(
		testField(10, testString("de")),
		testField(70, testInteger(int64(0x80010203))),
		testField(546, testSymbol(377)),
		testField(580, testSymbol(320)),
		testField(583, testSymbol(369)),
		testField(761, testList(testSymbol(760))),
		testField(496, testStruct(
			testField(498, testInteger(int64(0xff000000))),
			testField(499, dimension(318, 1)),
			testField(500, dimension(318, 2)),
			testField(501, dimension(318, 3)),
		)),
	)}}
	builder := epubBuilder{book: book}
	css := builder.styleSheet()
	for _, wanted := range []string{
		"background-color:rgba(1,2,3,0.504)", "box-sizing:content-box",
		"font-variant:small-caps", "margin-left:auto", "margin-right:auto",
		"box-shadow:1pt 2pt 3pt #000000",
	} {
		if !strings.Contains(css, wanted) {
			t.Fatalf("stylesheet missing %q:\n%s", wanted, css)
		}
	}
	node := testStruct(testField(157, testSymbol(3)), testField(159, testSymbol(269)), testField(145, testString("Überschrift")))
	if attributes := builder.nodeAttributes(node); !strings.Contains(attributes, `lang="de" xml:lang="de"`) {
		t.Fatalf("node attributes = %q", attributes)
	}
	got, _, err := builder.renderValues(node, 1, map[uint32]bool{}, 0)
	if err != nil || !strings.HasPrefix(got, `<h2`) {
		t.Fatalf("heading output = %q, %v", got, err)
	}
}

func TestImageMediaTypeIncludesEPUBWebP(t *testing.T) {
	data := []byte("RIFF\x00\x00\x00\x00WEBP")
	extension, mediaType, ok := imageMediaType(data)
	if !ok || extension != "webp" || mediaType != "image/webp" {
		t.Fatalf("imageMediaType = %q, %q, %t", extension, mediaType, ok)
	}
}

func TestImageMediaTypeRecognizesSVGWithXMLDeclaration(t *testing.T) {
	extension, mediaType, ok := imageMediaType([]byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"/>`))
	if !ok || extension != "svg" || mediaType != "image/svg+xml" {
		t.Fatalf("imageMediaType = %q, %q, %t", extension, mediaType, ok)
	}
}

func TestEmbeddedFontsProduceManifestAssetsAndFontFaces(t *testing.T) {
	font := &ionValue{kind: ionStruct, fields: []ionField{
		{id: 165, value: &ionValue{kind: ionString, text: "fonts/body"}},
		{id: 11, value: &ionValue{kind: ionString, text: `Book "Sans"`}},
		{id: 12, value: &ionValue{kind: ionSymbol, unsigned: 382}},
		{id: 13, value: &ionValue{kind: ionSymbol, unsigned: 361}},
		{id: 15, value: &ionValue{kind: ionSymbol, unsigned: 365}},
	}}
	builder := epubBuilder{book: &decodedBook{
		rawFonts: map[string][]byte{"fonts/body": append([]byte("OTTO"), make([]byte, 16)...)},
		fonts:    []*ionValue{font}, styles: map[uint32]*ionValue{},
	}}
	builder.prepareFonts()
	if len(builder.fontAssets) != 1 || builder.fontAssets[0].mediaType != "font/otf" {
		t.Fatalf("font assets = %+v", builder.fontAssets)
	}
	css := builder.styleSheet()
	for _, want := range []string{
		`font-family:"Book \"Sans\""`, `src:url("fonts/font-001.otf")`,
		"font-style:italic", "font-weight:700", "font-stretch:condensed",
	} {
		if !strings.Contains(css, want) {
			t.Fatalf("stylesheet missing %q:\n%s", want, css)
		}
	}
	opf := buildPackage(Metadata{Identifier: "id", Title: "title", Language: "en"},
		[]epubSection{{path: "text/section-0001.xhtml"}}, nil, builder.fontAssets, "rtl")
	if !strings.Contains(opf, `href="fonts/font-001.otf" media-type="font/otf"`) {
		t.Fatalf("package does not manifest font:\n%s", opf)
	}
	if !strings.Contains(opf, `<spine page-progression-direction="rtl">`) {
		t.Fatalf("package does not preserve reading direction:\n%s", opf)
	}
}

func TestMetadataCoverIsManifestedOutsideReadingOrder(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...)
	builder := epubBuilder{
		book: &decodedBook{
			resources: map[uint32]resource{7: {format: 284, location: "cover"}},
			rawMedia:  map[string][]byte{"cover": png},
		},
		assets: make(map[uint32]*epubAsset), coverID: 7,
	}
	builder.addMetadataCover()
	if len(builder.assetOrder) != 1 || !builder.assetOrder[0].coverImage {
		t.Fatalf("cover assets = %+v", builder.assetOrder)
	}
	opf := buildPackage(Metadata{Identifier: "id", Title: "title", Language: "en"},
		[]epubSection{{path: "text/section-0001.xhtml"}}, builder.assetOrder, nil, "")
	if !strings.Contains(opf, `properties="cover-image"`) {
		t.Fatalf("package does not mark cover:\n%s", opf)
	}
}
