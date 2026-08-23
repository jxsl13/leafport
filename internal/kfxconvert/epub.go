package kfxconvert

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"hash/crc32"
	"html"
	"io"
	"net/url"
	"os"
	pathpkg "path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Metadata supplies library metadata when a KFX publication omits it.
type Metadata struct {
	Identifier string
	Title      string
	Authors    []string
	Language   string
	Publisher  string
}

// EPUBResult summarizes a reflowable EPUB reconstruction.
type EPUBResult struct {
	Sections int
	Images   int
	Media    int
	Fonts    int
}

type epubAsset struct {
	id         uint32
	manifestID string
	path       string
	mediaType  string
	data       []byte
	image      bool
	coverImage bool
}

type epubSection struct {
	path  string
	title string
	data  []byte
}

type epubFontAsset struct {
	path      string
	mediaType string
	data      []byte
}

type epubFontFace struct {
	family  string
	style   string
	weight  string
	stretch string
	path    string
}

type epubNavigationItem struct {
	label    string
	href     string
	epubType string
	section  int
	position int
	children []epubNavigationItem
}

type epubNavigationDocument struct {
	toc       []epubNavigationItem
	landmarks []epubNavigationItem
	pages     []epubNavigationItem
}

type epubBuilder struct {
	book             *decodedBook
	assets           map[uint32]*epubAsset
	assetOrder       []*epubAsset
	pluginAssets     map[string]*epubAsset
	pluginStack      map[uint32]bool
	nextPluginAsset  int
	coverID          uint32
	fontAssets       []epubFontAsset
	fontFaces        []epubFontFace
	nodeSections     map[uint32]int
	sectionSections  map[uint32]int
	nodePositions    map[uint32]int
	sectionNodeCount map[int]int
	nodeTextRunes    map[uint32]int
	positionAnchors  map[uint32]map[int][]string
	nextPositionID   int
	templateStack    map[uint32]bool
	language         string
}

// ConvertToEPUB reconstructs a standards-based EPUB from a DRM-free KFX
// archive. The destination is created exclusively and removed on failure.
func ConvertToEPUB(archivePath, destination string, fallback Metadata) (result EPUBResult, err error) {
	book, err := loadBook(archivePath)
	if err != nil {
		return result, err
	}
	return convertBookToEPUB(book, destination, fallback)
}

func convertBookToEPUB(book *decodedBook, destination string, fallback Metadata) (result EPUBResult, err error) {
	if err := validateEPUBFeatureSupport(book); err != nil {
		return result, err
	}
	metadata := book.publicationMetadata(fallback)
	builder := epubBuilder{book: book, assets: make(map[uint32]*epubAsset), language: metadata.Language}
	builder.coverID = book.coverResourceID()
	builder.prepareFonts()
	sectionIDs := readingOrderSections(book.document)
	if len(sectionIDs) == 0 {
		return result, errors.New("KFX publication has no reading order")
	}
	builder.indexSections(sectionIDs)
	navigation := builder.navigationDocument()
	sections := make([]epubSection, 0, len(sectionIDs))
	for index, sectionID := range sectionIDs {
		section, renderErr := builder.renderSection(sectionID, index+1)
		if renderErr != nil {
			return result, fmt.Errorf("render section $%d: %w", sectionID, renderErr)
		}
		sections = append(sections, section)
	}
	// A cover is a publication resource even when it does not occur in the
	// reading-order storylines. Keep a usable metadata cover in the manifest.
	builder.addMetadataCover()
	if len(sections) == 0 {
		return result, errors.New("KFX reading order produced no EPUB sections")
	}

	file, err := os.OpenFile(destination, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return result, err
	}
	complete := false
	defer func() {
		if file != nil {
			err = errors.Join(err, file.Close())
		}
		if !complete {
			err = errors.Join(err, os.Remove(destination))
		}
	}()

	archive := zip.NewWriter(file)
	if err = writeZIPEntry(archive, "mimetype", "application/epub+zip", zip.Store); err != nil {
		return result, err
	}
	container := `<?xml version="1.0" encoding="UTF-8"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`
	if err = writeZIPEntry(archive, "META-INF/container.xml", container, zip.Deflate); err != nil {
		return result, err
	}
	if err = writeZIPEntry(archive, "OEBPS/styles.css", builder.styleSheet(), zip.Deflate); err != nil {
		return result, err
	}
	for _, section := range sections {
		if err = writeZIPBytes(archive, "OEBPS/"+section.path, section.data, zip.Deflate); err != nil {
			return result, err
		}
	}
	for _, asset := range builder.assetOrder {
		if err = writeZIPBytes(archive, "OEBPS/"+asset.path, asset.data, zip.Deflate); err != nil {
			return result, err
		}
	}
	for _, font := range builder.fontAssets {
		if err = writeZIPBytes(archive, "OEBPS/"+font.path, font.data, zip.Deflate); err != nil {
			return result, err
		}
	}
	if err = writeZIPEntry(archive, "OEBPS/nav.xhtml", buildNavigation(metadata.Title, metadata.Language, sections, navigation), zip.Deflate); err != nil {
		return result, err
	}
	if err = writeZIPEntry(archive, "OEBPS/content.opf", buildPackage(
		metadata, sections, builder.assetOrder, builder.fontAssets, book.pageProgressionDirection()), zip.Deflate); err != nil {
		return result, err
	}
	if err = archive.Close(); err != nil {
		return result, err
	}
	if err = file.Sync(); err != nil {
		return result, err
	}
	if err = file.Close(); err != nil {
		file = nil
		return result, err
	}
	file = nil
	imageCount := builder.imageCount()
	mediaCount := builder.mediaCount()
	if err = validateEPUB(destination, len(sections), imageCount, mediaCount, len(builder.fontAssets)); err != nil {
		return result, err
	}
	result.Sections = len(sections)
	result.Images = imageCount
	result.Media = mediaCount
	result.Fonts = len(builder.fontAssets)
	complete = true
	return result, nil
}

func (book *decodedBook) publicationMetadata(fallback Metadata) Metadata {
	result := fallback
	titleMetadata := book.metadata["kindle_title_metadata"]
	first := func(key string) string {
		if len(titleMetadata[key]) != 0 {
			return titleMetadata[key][0]
		}
		return ""
	}
	if value := first("ASIN"); value != "" {
		result.Identifier = value
	} else if value := first("content_id"); value != "" {
		result.Identifier = value
	}
	if value := first("title"); value != "" {
		result.Title = value
	}
	if values := titleMetadata["author"]; len(values) != 0 {
		result.Authors = append([]string(nil), values...)
	}
	if value := first("language"); value != "" {
		result.Language = value
	}
	if value := first("publisher"); value != "" {
		result.Publisher = value
	}
	if result.Identifier == "" {
		result.Identifier = "unknown"
	}
	if result.Title == "" {
		result.Title = result.Identifier
	}
	if result.Language == "" {
		result.Language = "und"
	}
	result.Identifier = book.redactText(result.Identifier)
	result.Title = book.redactText(result.Title)
	result.Language = book.redactText(result.Language)
	result.Publisher = book.redactText(result.Publisher)
	for index, author := range result.Authors {
		result.Authors[index] = book.redactText(author)
	}
	return result
}

func (book *decodedBook) coverResourceID() uint32 {
	values := book.metadata["kindle_title_metadata"]["cover_image"]
	if len(values) == 0 {
		return 0
	}
	for id := range book.resources {
		if resolveSymbol(uint64(id), book.symbols) == values[0] {
			return id
		}
	}
	return 0
}

func (builder *epubBuilder) renderSection(sectionID uint32, number int) (epubSection, error) {
	section := builder.book.sections[sectionID]
	if section == nil {
		return epubSection{}, fmt.Errorf("missing section")
	}
	pageTemplates := ionFieldValue(section, 141)
	storyIDs := uniqueSymbols(pageTemplates, 176)
	if len(storyIDs) == 0 {
		return epubSection{}, errors.New("section has no page-template storyline")
	}
	var body strings.Builder
	title := "Section " + strconv.Itoa(number)
	fragment, heading, err := builder.renderValues(pageTemplates, 0, make(map[uint32]bool), 0)
	if err != nil {
		return epubSection{}, err
	}
	if heading != "" {
		title = heading
	}
	body.WriteString(fragment)
	if strings.HasPrefix(title, "Section ") {
		for _, storyID := range storyIDs {
			if candidate := builder.firstStoryText(storyID, make(map[uint32]bool)); candidate != "" {
				title = candidate
				break
			}
		}
	}
	title = shortNavigationTitle(title, 80)
	path := fmt.Sprintf("text/section-%04d.xhtml", number)
	document := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE html>
<html xmlns="http://www.w3.org/1999/xhtml" xmlns:epub="http://www.idpf.org/2007/ops" lang="` + escapeXML(builder.language) + `">
<head><meta charset="UTF-8"/><title>` + escapeXML(title) + `</title><link rel="stylesheet" type="text/css" href="../styles.css"/></head>
<body>` + body.String() + `</body></html>`
	return epubSection{path: path, title: title, data: []byte(document)}, nil
}

func (builder *epubBuilder) firstStoryText(storyID uint32, seen map[uint32]bool) string {
	if seen[storyID] {
		return ""
	}
	seen[storyID] = true
	story := builder.book.storylines[storyID]
	var visit func(*ionValue) string
	visit = func(value *ionValue) string {
		if value == nil {
			return ""
		}
		if value.kind == ionStruct {
			if ionFieldValue(value, 145) != nil {
				if text, err := builder.book.nodeText(value); err == nil && strings.TrimSpace(text) != "" {
					return strings.TrimSpace(text)
				}
			}
			if children := ionFieldValue(value, 146); children != nil {
				return visit(children)
			}
			if nested, ok := ionSymbolID(ionFieldValue(value, 176)); ok && nested <= uint64(^uint32(0)) && uint32(nested) != storyID {
				if text := builder.firstStoryText(uint32(nested), seen); text != "" {
					return text
				}
			}
			return ""
		}
		for _, child := range value.children {
			if text := visit(child); text != "" {
				return text
			}
		}
		return ""
	}
	return visit(ionFieldValue(story, 146))
}

func shortNavigationTitle(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return strings.TrimSpace(string(runes[:limit-1])) + "…"
}

func (builder *epubBuilder) renderStory(storyID uint32, stack map[uint32]bool) (string, string, error) {
	if stack[storyID] {
		return "", "", fmt.Errorf("storyline cycle at $%d", storyID)
	}
	story := builder.book.storylines[storyID]
	if story == nil {
		return "", "", fmt.Errorf("missing storyline $%d", storyID)
	}
	stack[storyID] = true
	defer delete(stack, storyID)
	return builder.renderValues(ionFieldValue(story, 146), storyID, stack, 0)
}

func (builder *epubBuilder) renderValues(value *ionValue, storyID uint32, stack map[uint32]bool, parentKind uint64) (string, string, error) {
	if value == nil {
		return "", "", nil
	}
	if value.kind == ionList || value.kind == ionSExpr {
		var output strings.Builder
		var heading string
		for _, child := range value.children {
			fragment, childHeading, err := builder.renderValues(child, storyID, stack, parentKind)
			if err != nil {
				return "", "", err
			}
			output.WriteString(fragment)
			if heading == "" {
				heading = childHeading
			}
		}
		return output.String(), heading, nil
	}
	if value.kind == ionSymbol {
		templateID := value.unsigned
		if templateID > uint64(^uint32(0)) || builder.book.templates[uint32(templateID)] == nil {
			return "", "", fmt.Errorf("content references missing template $%d", templateID)
		}
		id := uint32(templateID)
		if builder.templateStack == nil {
			builder.templateStack = make(map[uint32]bool)
		}
		if builder.templateStack[id] {
			return "", "", fmt.Errorf("template cycle at $%d", id)
		}
		builder.templateStack[id] = true
		defer delete(builder.templateStack, id)
		return builder.renderValues(builder.book.templates[id], storyID, stack, parentKind)
	}
	if value.kind != ionStruct {
		if text, ok := ionText(value); ok {
			return escapeText(builder.book.redactText(text)), "", nil
		}
		return "", "", nil
	}
	kind, _ := ionSymbolID(ionFieldValue(value, 159))
	switch kind {
	case 269:
		if ionFieldValue(value, 145) != nil {
			text, err := builder.book.nodeText(value)
			if err != nil {
				return "", "", err
			}
			content, err := builder.renderStyledText(value, text)
			if err != nil {
				return "", "", err
			}
			attributes := builder.nodeAttributes(value)
			level, heading := ionInteger(ionFieldValue(value, 790))
			if !heading {
				if styleID, ok := ionSymbolID(ionFieldValue(value, 157)); ok && styleID <= uint64(^uint32(0)) {
					heading = builder.book.styleHasLayoutHint(uint32(styleID), 760, make(map[uint32]bool))
					level = 2
				}
			}
			if heading {
				if level < 1 || level > 6 {
					level = 2
				}
				tag := "h" + strconv.FormatInt(level, 10)
				return "<" + tag + attributes + ">" + content + "</" + tag + ">", strings.TrimSpace(text), nil
			}
			if classification, ok := ionSymbolID(ionFieldValue(value, 615)); ok {
				switch classification {
				case 618, 281:
					return `<aside epub:type="footnote"` + attributes + `>` + content + `</aside>`, "", nil
				case 619:
					return `<aside epub:type="endnote"` + attributes + `>` + content + `</aside>`, "", nil
				case 688:
					return `<span role="math"` + attributes + `>` + content + `</span>`, "", nil
				case 453:
					if parentKind == 278 {
						return "<caption" + attributes + ">" + content + "</caption>", "", nil
					}
				}
			}
			if parentKind == 279 {
				return "<td" + attributes + ">" + content + "</td>", "", nil
			}
			if render, ok := ionSymbolID(ionFieldValue(value, 601)); ok && render == 283 {
				return "<span" + attributes + ">" + content + "</span>", "", nil
			}
			return "<p" + attributes + ">" + content + "</p>", "", nil
		}
	case 271:
		resourceID, ok := ionSymbolID(ionFieldValue(value, 175))
		if !ok || resourceID > uint64(^uint32(0)) {
			return "", "", errors.New("image node has no valid resource")
		}
		asset, err := builder.addAsset(uint32(resourceID))
		if err != nil {
			return "", "", err
		}
		alt, _ := ionText(ionFieldValue(value, 584))
		alt = builder.book.redactText(alt)
		return `<figure` + builder.nodeAttributes(value) + `><img src="../` + escapeXML(asset.path) + `" alt="` + escapeXML(alt) + `"/></figure>`, "", nil
	case 272:
		svg, err := builder.renderKVG(value)
		return svg, "", err
	case 274:
		plugin, err := builder.renderPlugin(value)
		return plugin, "", err
	case 596:
		return "<hr" + builder.nodeAttributes(value) + "/>", "", nil
	case 780:
		return "<br/>", "", nil
	}

	var children, heading string
	if mathML, found, mathErr := builder.mathMLAnnotation(value); mathErr != nil {
		return "", "", mathErr
	} else if found {
		children = mathML
	} else if ionFieldValue(value, 145) != nil {
		text, textErr := builder.book.nodeText(value)
		if textErr != nil {
			return "", "", textErr
		}
		children = escapeText(text)
	} else if childValues := ionFieldValue(value, 146); childValues != nil {
		var renderErr error
		children, heading, renderErr = builder.renderValues(childValues, storyID, stack, kind)
		if renderErr != nil {
			return "", "", renderErr
		}
	} else if nestedID, ok := ionSymbolID(ionFieldValue(value, 176)); ok && nestedID <= uint64(^uint32(0)) && uint32(nestedID) != storyID {
		var nestedErr error
		children, heading, nestedErr = builder.renderStory(uint32(nestedID), stack)
		if nestedErr != nil {
			return "", "", nestedErr
		}
	}
	if href := builder.linkTarget(ionFieldValue(value, 179)); href != "" {
		children = `<a href="` + escapeXML(href) + `">` + children + `</a>`
	}
	tag := "div"
	switch kind {
	case 151:
		tag = "thead"
	case 270:
		tag = "div"
	case 276:
		tag = "ul"
		if style, ok := ionSymbolID(ionFieldValue(value, 100)); ok && isOrderedListStyle(style) {
			tag = "ol"
		}
		if start, ok := ionInteger(ionFieldValue(value, 104)); ok {
			return "<" + tag + builder.nodeAttributes(value) + ` start="` + strconv.FormatInt(start, 10) + `">` + children + "</" + tag + ">", heading, nil
		}
	case 277:
		tag = "li"
		if itemValue, ok := ionInteger(ionFieldValue(value, 104)); ok {
			return `<li` + builder.nodeAttributes(value) + ` value="` + strconv.FormatInt(itemValue, 10) + `">` + children + "</li>", heading, nil
		}
	case 278:
		tag = "table"
		children = tableColumnGroup(value) + children
	case 279:
		tag = "tr"
	case 439:
		return `<div style="display:none"` + builder.nodeAttributes(value) + `>` + children + `</div>`, heading, nil
	case 454:
		tag = "tbody"
	case 455:
		tag = "tfoot"
	}
	// KFX represents table cells as ordinary containers below a row. Some
	// producers use the explicitly known container kind $270, so this must be
	// applied after (and independently of) the kind switch.
	if parentKind == 279 {
		tag = "td"
	}
	if render, ok := ionSymbolID(ionFieldValue(value, 601)); ok && render == 283 && tag == "div" {
		tag = "span"
	}
	if classification, ok := ionSymbolID(ionFieldValue(value, 615)); ok {
		switch classification {
		case 618, 281:
			return `<aside epub:type="footnote"` + builder.nodeAttributes(value) + `>` + children + `</aside>`, heading, nil
		case 619:
			return `<aside epub:type="endnote"` + builder.nodeAttributes(value) + `>` + children + `</aside>`, heading, nil
		case 453:
			if parentKind == 278 {
				tag = "caption"
			}
		}
	}
	return "<" + tag + builder.nodeAttributes(value) + ">" + children + "</" + tag + ">", heading, nil
}

func isOrderedListStyle(style uint64) bool {
	switch style {
	case 343, 344, 345, 346, 347:
		return true
	default:
		return false
	}
}

func (book *decodedBook) nodeText(node *ionValue) (string, error) {
	text, err := book.nodeTextRaw(node)
	return book.redactText(text), err
}

func (book *decodedBook) nodeTextRaw(node *ionValue) (string, error) {
	content := ionFieldValue(node, 145)
	if text, ok := ionText(content); ok {
		return text, nil
	}
	if content == nil || content.kind != ionStruct {
		return "", nil
	}
	contentID, ok := ionSymbolID(ionFieldValue(content, 4))
	if !ok || contentID > uint64(^uint32(0)) {
		return "", errors.New("text node has an invalid content reference")
	}
	index, ok := ionInteger(ionFieldValue(content, 403))
	values := book.contents[uint32(contentID)]
	if !ok || index < 0 || index >= int64(len(values)) {
		return "", fmt.Errorf("text node references missing content $%d[%d]", contentID, index)
	}
	return values[index], nil
}

func (builder *epubBuilder) addAsset(resourceID uint32) (*epubAsset, error) {
	if asset := builder.assets[resourceID]; asset != nil {
		return asset, nil
	}
	_, data, ok := builder.book.resolveResource(resourceID)
	if !ok {
		return nil, fmt.Errorf("missing image resource $%d", resourceID)
	}
	data, err := builder.book.sanitizeAsset(data, "")
	if err != nil {
		return nil, fmt.Errorf("privacy-clean image resource $%d: %w", resourceID, err)
	}
	extension, mediaType, ok := imageMediaType(data)
	if !ok {
		return nil, fmt.Errorf("resource $%d uses an image format EPUB readers cannot display", resourceID)
	}
	asset := &epubAsset{
		id: resourceID, path: fmt.Sprintf("images/resource-%d.%s", resourceID, extension),
		manifestID: "asset-" + strconv.FormatUint(uint64(resourceID), 10),
		mediaType:  mediaType, data: data, image: true, coverImage: resourceID == builder.coverID,
	}
	builder.assets[resourceID] = asset
	builder.assetOrder = append(builder.assetOrder, asset)
	return asset, nil
}

func (builder *epubBuilder) imageCount() int {
	count := 0
	for _, asset := range builder.assetOrder {
		if asset.image {
			count++
		}
	}
	return count
}

func (builder *epubBuilder) mediaCount() int {
	count := 0
	for _, asset := range builder.assetOrder {
		if !asset.image {
			count++
		}
	}
	return count
}

func (builder *epubBuilder) addMetadataCover() {
	if builder.coverID != 0 && builder.assets[builder.coverID] == nil {
		_, _ = builder.addAsset(builder.coverID)
	}
}

func imageMediaType(data []byte) (extension, mediaType string, ok bool) {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "png", "image/png", true
	case len(data) >= 3 && bytes.Equal(data[:3], []byte{0xff, 0xd8, 0xff}):
		return "jpg", "image/jpeg", true
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return "gif", "image/gif", true
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return "webp", "image/webp", true
	case isSVG(data):
		return "svg", "image/svg+xml", true
	default:
		return "", "", false
	}
}

func isSVG(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '<' {
		return false
	}
	decoder := xml.NewDecoder(bytes.NewReader(trimmed))
	for {
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		if start, ok := token.(xml.StartElement); ok {
			return strings.EqualFold(start.Name.Local, "svg")
		}
	}
}

func (builder *epubBuilder) prepareFonts() {
	if len(builder.book.rawFonts) == 0 {
		return
	}
	locations := make([]string, 0, len(builder.book.rawFonts))
	for location := range builder.book.rawFonts {
		locations = append(locations, location)
	}
	sort.Strings(locations)
	assets := make(map[string]*epubFontAsset)
	for _, location := range locations {
		extension, mediaType, ok := fontMediaType(builder.book.rawFonts[location])
		if !ok {
			continue
		}
		builder.fontAssets = append(builder.fontAssets, epubFontAsset{
			path:      fmt.Sprintf("fonts/font-%03d.%s", len(builder.fontAssets)+1, extension),
			mediaType: mediaType, data: builder.book.rawFonts[location],
		})
		assets[location] = &builder.fontAssets[len(builder.fontAssets)-1]
	}
	for _, font := range builder.book.fonts {
		location, locationOK := ionText(ionFieldValue(font, 165))
		family, familyOK := ionText(ionFieldValue(font, 11))
		asset := assets[location]
		family = strings.TrimSpace(family)
		if !locationOK || !familyOK || asset == nil || family == "" || family == "default" {
			continue
		}
		builder.fontFaces = append(builder.fontFaces, epubFontFace{
			family: family, path: asset.path,
			style:   fontStyle(ionFieldValue(font, 12)),
			weight:  fontWeight(ionFieldValue(font, 13)),
			stretch: fontStretch(ionFieldValue(font, 15)),
		})
	}
}

func fontMediaType(data []byte) (extension, mediaType string, ok bool) {
	if len(data) < 4 {
		return "", "", false
	}
	switch string(data[:4]) {
	case "\x00\x01\x00\x00", "true", "typ1":
		return "ttf", "font/ttf", true
	case "OTTO":
		return "otf", "font/otf", true
	case "wOFF":
		return "woff", "font/woff", true
	case "wOF2":
		return "woff2", "font/woff2", true
	default:
		return "", "", false
	}
}

func fontStyle(value *ionValue) string {
	id, _ := ionSymbolID(value)
	return map[uint64]string{350: "normal", 381: "oblique", 382: "italic"}[id]
}

func fontWeight(value *ionValue) string {
	id, _ := ionSymbolID(value)
	return map[uint64]string{
		350: "normal", 355: "100", 356: "200", 357: "300", 358: "400",
		359: "500", 360: "600", 361: "700", 362: "800", 363: "900",
	}[id]
}

func fontStretch(value *ionValue) string {
	id, _ := ionSymbolID(value)
	return map[uint64]string{
		350: "normal", 365: "condensed", 366: "semi-condensed",
		367: "semi-expanded", 368: "expanded",
	}[id]
}

func buildNavigation(title, language string, sections []epubSection, navigation epubNavigationDocument) string {
	var items strings.Builder
	if len(navigation.toc) != 0 {
		writeNavigationItems(&items, navigation.toc)
	} else {
		for _, section := range sections {
			items.WriteString(`<li><a href="` + escapeXML(section.path) + `">` + escapeXML(section.title) + `</a></li>`)
		}
	}
	var supplemental strings.Builder
	if len(navigation.landmarks) != 0 {
		supplemental.WriteString(`<nav epub:type="landmarks"><h2>Landmarks</h2><ol>`)
		writeNavigationItems(&supplemental, navigation.landmarks)
		supplemental.WriteString(`</ol></nav>`)
	}
	if len(navigation.pages) != 0 {
		supplemental.WriteString(`<nav epub:type="page-list"><h2>Pages</h2><ol>`)
		writeNavigationItems(&supplemental, navigation.pages)
		supplemental.WriteString(`</ol></nav>`)
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE html>
<html xmlns="http://www.w3.org/1999/xhtml" xmlns:epub="http://www.idpf.org/2007/ops" lang="` + escapeXML(language) + `">
<head><meta charset="UTF-8"/><title>` + escapeXML(title) + `</title></head>
<body><nav epub:type="toc" id="toc"><h1>` + escapeXML(title) + `</h1><ol>` + items.String() + `</ol></nav>` + supplemental.String() + `</body></html>`
}

func writeNavigationItems(output *strings.Builder, items []epubNavigationItem) {
	for _, item := range items {
		output.WriteString(`<li><a`)
		if item.epubType != "" {
			output.WriteString(` epub:type="` + escapeXML(item.epubType) + `"`)
		}
		output.WriteString(` href="` + escapeXML(item.href) + `">` + escapeXML(item.label) + `</a>`)
		if len(item.children) != 0 {
			output.WriteString("<ol>")
			writeNavigationItems(output, item.children)
			output.WriteString("</ol>")
		}
		output.WriteString("</li>")
	}
}

func buildPackage(metadata Metadata, sections []epubSection, assets []*epubAsset, fonts []epubFontAsset, direction string) string {
	var manifest, spine, creators strings.Builder
	manifest.WriteString(`<item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/>`)
	manifest.WriteString(`<item id="css" href="styles.css" media-type="text/css"/>`)
	for index, section := range sections {
		id := fmt.Sprintf("section-%04d", index+1)
		properties := sectionManifestProperties(section.data)
		manifest.WriteString(`<item id="` + id + `" href="` + escapeXML(section.path) + `" media-type="application/xhtml+xml"` + properties + `/>`)
		spine.WriteString(`<itemref idref="` + id + `"/>`)
	}
	for _, asset := range assets {
		manifestID := asset.manifestID
		if manifestID == "" {
			manifestID = "asset-" + strconv.FormatUint(uint64(asset.id), 10)
		}
		properties := ""
		if asset.coverImage {
			properties = ` properties="cover-image"`
		}
		manifest.WriteString(`<item id="` + escapeXML(manifestID) + `" href="` +
			escapeXML(asset.path) + `" media-type="` + escapeXML(asset.mediaType) + `"` + properties + `/>`)
	}
	for index, font := range fonts {
		manifest.WriteString(`<item id="font-` + strconv.Itoa(index+1) + `" href="` +
			escapeXML(font.path) + `" media-type="` + font.mediaType + `"/>`)
	}
	for _, author := range metadata.Authors {
		creators.WriteString(`<dc:creator>` + escapeXML(author) + `</dc:creator>`)
	}
	publisher := ""
	if metadata.Publisher != "" {
		publisher = `<dc:publisher>` + escapeXML(metadata.Publisher) + `</dc:publisher>`
	}
	modified := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	spineAttributes := ""
	if direction == "ltr" || direction == "rtl" {
		spineAttributes = ` page-progression-direction="` + direction + `"`
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="pub-id">
<metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
<dc:identifier id="pub-id">` + escapeXML(metadata.Identifier) + `</dc:identifier>
<dc:title>` + escapeXML(metadata.Title) + `</dc:title><dc:language>` + escapeXML(metadata.Language) + `</dc:language>` +
		creators.String() + publisher + `<meta property="dcterms:modified">` + modified + `</meta></metadata>
<manifest>` + manifest.String() + `</manifest><spine` + spineAttributes + `>` + spine.String() + `</spine></package>`
}

func sectionManifestProperties(data []byte) string {
	var properties []string
	if bytes.Contains(data, []byte(mathMLNamespace)) {
		properties = append(properties, "mathml")
	}
	if bytes.Contains(data, []byte("http://www.w3.org/2000/svg")) {
		properties = append(properties, "svg")
	}
	if len(properties) == 0 {
		return ""
	}
	return ` properties="` + strings.Join(properties, " ") + `"`
}

func writeZIPEntry(archive *zip.Writer, name, value string, method uint16) error {
	return writeZIPBytes(archive, name, []byte(value), method)
}

func writeZIPBytes(archive *zip.Writer, name string, value []byte, method uint16) error {
	header := &zip.FileHeader{Name: name, Method: method}
	header.SetMode(0o600)
	var (
		writer io.Writer
		err    error
	)
	if method == zip.Store {
		header.CRC32 = crc32.ChecksumIEEE(value)
		header.CompressedSize64 = uint64(len(value))
		header.UncompressedSize64 = uint64(len(value))
		writer, err = archive.CreateRaw(header)
	} else {
		writer, err = archive.CreateHeader(header)
	}
	if err != nil {
		return err
	}
	_, err = writer.Write(value)
	return err
}

func validateEPUB(path string, sections, images, media, fonts int) error {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return fmt.Errorf("validate EPUB ZIP: %w", err)
	}
	defer archive.Close()
	if len(archive.File) == 0 || archive.File[0].Name != "mimetype" || archive.File[0].Method != zip.Store || archive.File[0].Flags&0x8 != 0 {
		return errors.New("validate EPUB: mimetype is not the first uncompressed entry")
	}
	mimetype, err := readZipFile(archive.File[0])
	if err != nil {
		return fmt.Errorf("validate EPUB mimetype: %w", err)
	}
	if !bytes.Equal(mimetype, []byte("application/epub+zip")) {
		return errors.New("validate EPUB: invalid mimetype contents")
	}
	wanted := map[string]bool{
		"mimetype": false, "META-INF/container.xml": false, "OEBPS/content.opf": false,
		"OEBPS/nav.xhtml": false, "OEBPS/styles.css": false,
	}
	sectionCount, imageCount, mediaCount, fontCount := 0, 0, 0, 0
	files := make(map[string]bool, len(archive.File))
	for _, file := range archive.File {
		files[file.Name] = true
	}
	documents := make(map[string]epubXMLDocument)
	for _, file := range archive.File {
		if _, ok := wanted[file.Name]; ok {
			wanted[file.Name] = true
		}
		if strings.HasPrefix(file.Name, "OEBPS/text/") && strings.HasSuffix(file.Name, ".xhtml") {
			sectionCount++
		}
		if strings.HasPrefix(file.Name, "OEBPS/images/") {
			imageCount++
		}
		if strings.HasPrefix(file.Name, "OEBPS/media/") {
			mediaCount++
		}
		if strings.HasPrefix(file.Name, "OEBPS/fonts/") {
			fontCount++
		}
		if strings.HasSuffix(file.Name, ".xhtml") || strings.HasSuffix(file.Name, ".opf") || strings.HasSuffix(file.Name, ".xml") {
			reader, openErr := file.Open()
			if openErr != nil {
				return openErr
			}
			document, decodeErr := inspectEPUBXML(reader)
			closeErr := reader.Close()
			if decodeErr != nil || closeErr != nil {
				return fmt.Errorf("validate EPUB XML %s: %w", file.Name, errors.Join(decodeErr, closeErr))
			}
			documents[file.Name] = document
		}
	}
	for name, found := range wanted {
		if !found {
			return fmt.Errorf("validate EPUB: missing %s", name)
		}
	}
	if sectionCount != sections || imageCount != images || mediaCount != media || fontCount != fonts {
		return fmt.Errorf("validate EPUB: found %d sections/%d images/%d media/%d fonts; expected %d/%d/%d/%d",
			sectionCount, imageCount, mediaCount, fontCount, sections, images, media, fonts)
	}
	if err := validateEPUBReferences(files, documents); err != nil {
		return err
	}
	return nil
}

type epubXMLDocument struct {
	ids   map[string]bool
	hrefs []string
}

func inspectEPUBXML(reader io.Reader) (epubXMLDocument, error) {
	document := epubXMLDocument{ids: make(map[string]bool)}
	decoder := xml.NewDecoder(reader)
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return document, nil
		}
		if err != nil {
			return document, err
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		for _, attribute := range start.Attr {
			switch attribute.Name.Local {
			case "id":
				if attribute.Value == "" {
					continue
				}
				if document.ids[attribute.Value] {
					return document, fmt.Errorf("duplicate XML id %q", attribute.Value)
				}
				document.ids[attribute.Value] = true
			case "href", "src", "poster", "data":
				document.hrefs = append(document.hrefs, attribute.Value)
			}
		}
	}
}

func validateEPUBReferences(files map[string]bool, documents map[string]epubXMLDocument) error {
	for source, document := range documents {
		for _, raw := range document.hrefs {
			reference, err := url.Parse(raw)
			if err != nil {
				return fmt.Errorf("validate EPUB link in %s: invalid href %q: %w", source, raw, err)
			}
			if reference.Scheme != "" {
				continue
			}
			target := source
			if reference.Path != "" {
				target = pathpkg.Clean(pathpkg.Join(pathpkg.Dir(source), reference.Path))
			}
			if target == "." || strings.HasPrefix(target, "../") || !files[target] {
				return fmt.Errorf("validate EPUB link in %s: href %q targets missing file %s", source, raw, target)
			}
			if reference.Fragment == "" {
				continue
			}
			targetDocument, ok := documents[target]
			if !ok || !targetDocument.ids[reference.Fragment] {
				return fmt.Errorf("validate EPUB link in %s: href %q targets missing fragment %q in %s",
					source, raw, reference.Fragment, target)
			}
		}
	}
	return nil
}

func escapeXML(value string) string {
	return html.EscapeString(value)
}

func escapeText(value string) string {
	return strings.ReplaceAll(escapeXML(value), "\n", "<br/>")
}

const defaultEPUBCSS = `html { font-family: serif; line-height: 1.45; }
body { margin: 5%; }
img { display: block; height: auto; margin: 1em auto; max-width: 100%; }
audio, video { max-width: 100%; }
figure { margin: 1em 0; text-align: center; }
table { border-collapse: collapse; max-width: 100%; }
td, th { padding: .25em; vertical-align: top; }
p { margin: .65em 0; }
aside { font-size: .9em; }
.kfx-dropcap { float: left; font-size: 3em; line-height: .8; padding-right: .08em; }
`
