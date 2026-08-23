package kfxconvert

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
)

const (
	kfxContentType          = 145
	kfxExternalResourceType = 164
	kfxStyleType            = 157
	kfxFontType             = 262
	kfxStorylineType        = 259
	kfxSectionType          = 260
	kfxAnchorType           = 266
	kfxNavigationType       = 389
	kfxTemplateType         = 608
	kfxImageType            = 271
	kfxTextType             = 269
	kfxMetadataType         = 490
	kfxDocumentDataType     = 538
	kfxPDFFormat            = 565
)

// Page describes one fixed-layout page in reading order.
type Page struct {
	ResourceID uint32
	SectionID  uint32
	Format     uint64
	Location   string
	PageIndex  int
	Width      int
	Height     int
	Data       []byte
}

type decodedBook struct {
	symbols      []string
	entities     map[uint32]map[uint32]*ionValue
	rawMedia     map[string][]byte
	rawFonts     map[string][]byte
	resources    map[uint32]resource
	dependencies map[uint32][]string
	contents     map[uint32][]string
	metadata     map[string]map[string][]string
	metadataRaw  map[string]map[string][]*ionValue
	styles       map[uint32]*ionValue
	fonts        []*ionValue
	anchors      map[uint32]anchor
	navigation   *ionValue
	document     *ionValue
	sections     map[uint32]*ionValue
	storylines   map[uint32]*ionValue
	templates    map[uint32]*ionValue
	privacy      *personalRedactor
}

type resource struct {
	format      uint64
	location    string
	mime        string
	pageIndex   int
	width       int
	height      int
	variants    []uint32
	referred    []uint32
	tiles       [][]string
	tileWidth   int
	tileHeight  int
	tilePadding int
}

type anchor struct {
	externalURL  string
	targetNode   uint32
	targetSymbol bool
	offset       int
}

// FixedLayoutPages returns all image/PDF-backed pages in the publication's
// declared reading order. An empty result means the book is reflowable.
func FixedLayoutPages(path string) ([]Page, error) {
	book, err := loadBook(path)
	if err != nil {
		return nil, err
	}
	return book.fixedLayoutPages()
}

func fixedLayoutPagesBytes(data []byte) ([]Page, error) {
	book, err := loadBookBytes(data)
	if err != nil {
		return nil, err
	}
	return book.fixedLayoutPages()
}

func (book *decodedBook) fixedLayoutPages() ([]Page, error) {
	// A reflowable book can contain hundreds of ordinary illustrations. Do not
	// mistake those for pages. KFX marks fixed-layout publications explicitly;
	// accepting a PDF resource as a legacy fallback keeps older print replicas
	// readable when that metadata is absent.
	if !book.isFixedLayout() && !book.hasPDFResource() {
		return nil, nil
	}
	sectionIDs := readingOrderSections(book.document)
	if len(sectionIDs) == 0 {
		return nil, nil
	}
	type pageResource struct {
		sectionID  uint32
		resourceID uint32
		background bool
	}
	var pageResources []pageResource
	containsText := false
	for _, sectionID := range sectionIDs {
		section := book.sections[sectionID]
		if section == nil {
			return nil, fmt.Errorf("reading order references missing section $%d", sectionID)
		}
		resources, hasText, err := book.collectSectionPageResources(section)
		if err != nil {
			return nil, fmt.Errorf("section $%d: %w", sectionID, err)
		}
		containsText = containsText || hasText
		if len(resources) > 2 {
			return nil, nil
		}
		hasForeground, hasBackground := false, false
		for _, item := range resources {
			hasBackground = hasBackground || item.background
			hasForeground = hasForeground || !item.background
			pageResources = append(pageResources, pageResource{
				sectionID: sectionID, resourceID: item.resourceID, background: item.background,
			})
		}
		if hasForeground && hasBackground {
			return nil, nil
		}
	}
	if len(pageResources) == 0 || containsText {
		return nil, nil
	}
	pages := make([]Page, 0, len(pageResources))
	for _, item := range pageResources {
		metadata, data, ok := book.resolveResource(item.resourceID)
		if !ok {
			return nil, fmt.Errorf("storyline references missing external resource $%d", item.resourceID)
		}
		sanitized, sanitizeErr := book.sanitizeAsset(data, metadata.mime)
		if sanitizeErr != nil {
			return nil, fmt.Errorf("privacy-clean resource $%d (%q): %w", item.resourceID, metadata.location, sanitizeErr)
		}
		data = sanitized
		pages = append(pages, Page{
			ResourceID: item.resourceID, SectionID: item.sectionID,
			Format: metadata.format, Location: metadata.location,
			PageIndex: metadata.pageIndex, Width: metadata.width, Height: metadata.height, Data: data,
		})
	}
	if !book.isFixedLayout() {
		for _, page := range pages {
			if page.Format != kfxPDFFormat || !bytes.HasPrefix(page.Data, []byte("%PDF-")) {
				return nil, nil
			}
		}
	}
	return pages, nil
}

type pageResourceReference struct {
	resourceID uint32
	background bool
}

// collectSectionPageResources follows inline and named page templates and
// storylines in document order. It deliberately records text as a disqualifier
// instead of treating every image in a reflowable section as a page.
func (book *decodedBook) collectSectionPageResources(section *ionValue) ([]pageResourceReference, bool, error) {
	var resources []pageResourceReference
	containsText := false
	seenStories := make(map[uint32]bool)
	seenTemplates := make(map[uint32]bool)
	var visit func(*ionValue) error
	visit = func(value *ionValue) error {
		if value == nil {
			return nil
		}
		if value.kind == ionSymbol {
			id := value.unsigned
			if id <= uint64(^uint32(0)) {
				templateID := uint32(id)
				if template := book.templates[templateID]; template != nil && !seenTemplates[templateID] {
					seenTemplates[templateID] = true
					return visit(template)
				}
			}
			return nil
		}
		if value.kind == ionStruct {
			if ignored, ok := ionBoolean(ionFieldValue(value, 69)); ok && ignored {
				return nil
			}
			kind, _ := ionSymbolID(ionFieldValue(value, 159))
			if kind == kfxTextType {
				containsText = true
			}
			if kind == kfxImageType {
				resourceID, ok := ionSymbolID(ionFieldValue(value, 175))
				if !ok || resourceID > uint64(^uint32(0)) {
					return errors.New("image node has no valid resource name")
				}
				resources = append(resources, pageResourceReference{resourceID: uint32(resourceID)})
			}
			if backgroundID, ok := ionSymbolID(ionFieldValue(value, 479)); ok {
				if backgroundID > uint64(^uint32(0)) {
					return errors.New("background image has an invalid resource name")
				}
				resources = append(resources, pageResourceReference{resourceID: uint32(backgroundID), background: true})
			}
			for _, field := range value.fields {
				if field.id == 176 {
					if storyID, ok := ionSymbolID(field.value); ok && storyID <= uint64(^uint32(0)) {
						id := uint32(storyID)
						if id != 0 && !seenStories[id] {
							story := book.storylines[id]
							if story == nil {
								return fmt.Errorf("missing storyline $%d", id)
							}
							seenStories[id] = true
							if err := visit(story); err != nil {
								return err
							}
						}
						continue
					}
				}
				if err := visit(field.value); err != nil {
					return err
				}
			}
		}
		for _, child := range value.children {
			if err := visit(child); err != nil {
				return err
			}
		}
		return nil
	}
	err := visit(ionFieldValue(section, 141))
	return resources, containsText, err
}

func (book *decodedBook) isFixedLayout() bool {
	return book.metadataValue("kindle_capability_metadata", "yj_fixed_layout") != nil
}

// isComic deliberately requires an explicit KFX comic capability. Fixed
// layout alone is also used for textbooks and children's books and must not
// silently change their final format to CBZ.
func (book *decodedBook) isComic() bool {
	if book == nil {
		return false
	}
	if ionFieldValue(book.document, 665) != nil {
		return true
	}
	for _, key := range []string{
		"yj_publisher_panels",
		"yj_facing_page",
		"yj_double_page_spread",
	} {
		if book.metadataValue("kindle_capability_metadata", key) != nil {
			return true
		}
	}
	if value := book.metadataValue("kindle_capability_metadata", "continuous_popup_progression"); value != nil {
		if number, ok := ionInteger(value); ok && number == 0 {
			return true
		}
	}
	return false
}

func (book *decodedBook) hasPDFResource() bool {
	for _, item := range book.resources {
		if item.format == kfxPDFFormat {
			return true
		}
	}
	return false
}

func (book *decodedBook) pageProgressionDirection() string {
	direction, _ := ionSymbolID(ionFieldValue(book.document, 192))
	switch direction {
	case 375:
		return "rtl"
	case 376:
		return "ltr"
	default:
		return ""
	}
}

func (book *decodedBook) metadataValue(category, key string) *ionValue {
	values := book.metadataRaw[category][key]
	if len(values) == 0 {
		return nil
	}
	return values[0]
}

func (book *decodedBook) resolveResource(resourceID uint32) (resource, []byte, bool) {
	return book.resolveResourceSeen(resourceID, make(map[uint32]bool))
}

func (book *decodedBook) resolveResourceSeen(resourceID uint32, seen map[uint32]bool) (resource, []byte, bool) {
	if seen[resourceID] {
		return resource{}, nil, false
	}
	seen[resourceID] = true
	metadata, ok := book.resources[resourceID]
	if !ok {
		return resource{}, nil, false
	}
	bestMetadata, bestData, found := metadata, []byte(nil), false
	if len(metadata.tiles) != 0 {
		if data, combined := book.combineResourceTiles(metadata); combined {
			bestMetadata.format = 284 // tile composition is encoded losslessly as PNG.
			if bestMetadata.location == "" {
				bestMetadata.location = fmt.Sprintf("resource-%d-tiles.png", resourceID)
			}
			bestData, found = data, true
		}
	}
	// The container entity map is authoritative for attachable resources. A
	// metadata location can also name a different delivery slice.
	if !found {
		for _, candidate := range book.dependencies[resourceID] {
			if data, exists := book.rawMedia[candidate]; exists && rawMatchesFormat(data, metadata.format) {
				bestMetadata.location = candidate
				bestData, found = data, true
				break
			}
		}
	}
	if !found {
		if data, exists := book.rawMedia[metadata.location]; exists && rawMatchesFormat(data, metadata.format) {
			bestData, found = data, true
		}
	}
	for _, variantID := range metadata.variants {
		variantMetadata, variantData, variantFound := book.resolveResourceSeen(variantID, seen)
		if !variantFound {
			continue
		}
		if !found || (variantMetadata.width > bestMetadata.width && variantMetadata.height > bestMetadata.height) {
			bestMetadata, bestData, found = variantMetadata, variantData, true
		}
	}
	return bestMetadata, bestData, found
}

func (book *decodedBook) combineResourceTiles(metadata resource) ([]byte, bool) {
	if metadata.width <= 0 || metadata.height <= 0 || metadata.tileWidth <= 0 || metadata.tileHeight <= 0 {
		return nil, false
	}
	canvas := image.NewNRGBA(image.Rect(0, 0, metadata.width, metadata.height))
	for y, row := range metadata.tiles {
		for x, location := range row {
			data := book.rawMedia[location]
			if len(data) == 0 {
				return nil, false
			}
			tile, _, err := image.Decode(bytes.NewReader(data))
			if err != nil {
				return nil, false
			}
			leftPadding, topPadding := 0, 0
			if x != 0 {
				leftPadding = metadata.tilePadding
			}
			if y != 0 {
				topPadding = metadata.tilePadding
			}
			targetX, targetY := x*metadata.tileWidth, y*metadata.tileHeight
			copyWidth := min(metadata.tileWidth, metadata.width-targetX)
			copyHeight := min(metadata.tileHeight, metadata.height-targetY)
			if copyWidth <= 0 || copyHeight <= 0 {
				continue
			}
			source := image.Pt(tile.Bounds().Min.X+leftPadding, tile.Bounds().Min.Y+topPadding)
			if source.X+copyWidth > tile.Bounds().Max.X || source.Y+copyHeight > tile.Bounds().Max.Y {
				return nil, false
			}
			draw.Draw(canvas, image.Rect(targetX, targetY, targetX+copyWidth, targetY+copyHeight), tile, source, draw.Src)
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, canvas); err != nil {
		return nil, false
	}
	return output.Bytes(), true
}

func rawMatchesFormat(data []byte, format uint64) bool {
	switch format {
	case kfxPDFFormat:
		return bytes.HasPrefix(data, []byte("%PDF-"))
	case 285:
		return len(data) >= 3 && bytes.Equal(data[:3], []byte{0xff, 0xd8, 0xff})
	case 284:
		return bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n"))
	case 286:
		return bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a"))
	case 548:
		return len(data) >= 4 && (bytes.Equal(data[:4], []byte{'I', 'I', 0xbc, 0x01}) ||
			bytes.Equal(data[:4], []byte{'M', 'M', 0x01, 0xbc}))
	}
	return len(data) != 0
}

func loadBook(path string) (*decodedBook, error) {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("open decrypted KFX archive: %w", err)
	}
	defer archive.Close()
	return decodeBookArchive(&archive.Reader)
}

func loadBookBytes(data []byte) (*decodedBook, error) {
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open decrypted KFX archive: %w", err)
	}
	return decodeBookArchive(archive)
}

func decodeBookArchive(archive *zip.Reader) (*decodedBook, error) {
	type parsedContainer struct {
		name string
		data Container
	}
	var containers []parsedContainer
	var symbols []string
	for _, file := range archive.File {
		if !isKFXContainerName(file.Name) {
			continue
		}
		data, readErr := readZipFile(file)
		if readErr != nil {
			return nil, readErr
		}
		if !bytes.HasPrefix(data, []byte("CONT")) {
			continue
		}
		container, parseErr := ParseContainer(data)
		if parseErr != nil {
			return nil, fmt.Errorf("%s: %w", file.Name, parseErr)
		}
		if len(container.DocumentSymbols) > len(symbols) {
			symbols = container.DocumentSymbols
		}
		containers = append(containers, parsedContainer{name: file.Name, data: container})
	}
	if len(containers) == 0 {
		return nil, errors.New("archive contains no DRM-free KFX CONT container")
	}
	book := &decodedBook{
		symbols: symbols, entities: make(map[uint32]map[uint32]*ionValue),
		rawMedia: make(map[string][]byte), rawFonts: make(map[string][]byte),
		resources: make(map[uint32]resource),
		sections:  make(map[uint32]*ionValue), storylines: make(map[uint32]*ionValue),
		dependencies: make(map[uint32][]string), contents: make(map[uint32][]string),
		metadata: make(map[string]map[string][]string), metadataRaw: make(map[string]map[string][]*ionValue),
		styles:    make(map[uint32]*ionValue),
		anchors:   make(map[uint32]anchor),
		templates: make(map[uint32]*ionValue),
	}
	for _, item := range containers {
		for _, entity := range item.data.Entities {
			if entity.Type == kfxRawMediaType {
				name := resolveSymbol(uint64(entity.ID), symbols)
				book.rawMedia[name] = entity.Payload
				continue
			}
			if entity.Type == kfxRawFontType {
				book.rawFonts[resolveSymbol(uint64(entity.ID), symbols)] = entity.Payload
				continue
			}
			values, parseErr := parseIonValues(entity.Payload)
			if parseErr != nil || len(values) == 0 {
				if parseErr == nil {
					parseErr = errors.New("missing Ion value")
				}
				return nil, fmt.Errorf("%s entity $%d/$%d: %w", item.name, entity.Type, entity.ID, parseErr)
			}
			value := values[len(values)-1]
			byID := book.entities[entity.Type]
			if byID == nil {
				byID = make(map[uint32]*ionValue)
				book.entities[entity.Type] = byID
			}
			byID[entity.ID] = value
			switch entity.Type {
			case kfxContentType:
				book.collectContent(entity.ID, value)
			case kfxStyleType:
				book.styles[entity.ID] = value
			case kfxFontType:
				book.fonts = append(book.fonts, value)
			case kfxExternalResourceType:
				format, formatOK := ionSymbolID(ionFieldValue(value, 161))
				location, locationOK := book.resourceLocation(ionFieldValue(value, 165))
				mime, _ := book.resourceLocation(ionFieldValue(value, 162))
				pageIndex, indexOK := ionInteger(ionFieldValue(value, 564))
				width := positiveIonInteger(ionFieldValue(value, 422), ionFieldValue(value, 66))
				height := positiveIonInteger(ionFieldValue(value, 423), ionFieldValue(value, 67))
				tiles := book.resourceTileLocations(ionFieldValue(value, 636))
				if formatOK && (locationOK || len(tiles) != 0) {
					if !indexOK {
						pageIndex = 0
					}
					book.resources[entity.ID] = resource{
						format: format, location: location, mime: mime, pageIndex: int(pageIndex), width: width, height: height,
						variants: resourceSymbolIDs(ionFieldValue(value, 635)), referred: resourceSymbolIDs(ionFieldValue(value, 167)), tiles: tiles,
						tileWidth:   positiveIonInteger(ionFieldValue(value, 637)),
						tileHeight:  positiveIonInteger(ionFieldValue(value, 638)),
						tilePadding: nonnegativeIonInteger(ionFieldValue(value, 797)),
					}
				}
			case kfxStorylineType:
				book.storylines[entity.ID] = value
			case kfxSectionType:
				book.sections[entity.ID] = value
			case kfxTemplateType:
				book.templates[entity.ID] = value
			case kfxAnchorType:
				book.collectAnchor(entity.ID, value)
			case kfxNavigationType:
				book.navigation = value
			case kfxDocumentDataType:
				book.document = value
			case kfxMetadataType:
				book.collectMetadata(value)
			case 419:
				book.collectDependencies(value)
			}
		}
	}
	return book, nil
}

func positiveIonInteger(values ...*ionValue) int {
	for _, value := range values {
		if number, ok := ionInteger(value); ok && number > 0 && number <= int64(^uint(0)>>1) {
			return int(number)
		}
	}
	return 0
}

func nonnegativeIonInteger(value *ionValue) int {
	number, ok := ionInteger(value)
	if !ok || number < 0 || number > int64(^uint(0)>>1) {
		return 0
	}
	return int(number)
}

func resourceSymbolIDs(value *ionValue) []uint32 {
	if value == nil || value.kind != ionList {
		return nil
	}
	result := make([]uint32, 0, len(value.children))
	for _, item := range value.children {
		if id, ok := ionSymbolID(item); ok && id <= uint64(^uint32(0)) {
			result = append(result, uint32(id))
		}
	}
	return result
}

func (book *decodedBook) resourceLocation(value *ionValue) (string, bool) {
	if location, ok := ionText(value); ok {
		return location, true
	}
	if id, ok := ionSymbolID(value); ok {
		return resolveSymbol(id, book.symbols), true
	}
	return "", false
}

func (book *decodedBook) resourceTileLocations(value *ionValue) [][]string {
	if value == nil || value.kind != ionList {
		return nil
	}
	rows := make([][]string, 0, len(value.children))
	for _, rowValue := range value.children {
		if rowValue == nil || rowValue.kind != ionList {
			return nil
		}
		row := make([]string, 0, len(rowValue.children))
		for _, item := range rowValue.children {
			location, ok := book.resourceLocation(item)
			if !ok {
				return nil
			}
			row = append(row, location)
		}
		if len(row) == 0 {
			return nil
		}
		rows = append(rows, row)
	}
	return rows
}

func (book *decodedBook) collectAnchor(id uint32, value *ionValue) {
	item := anchor{}
	item.externalURL, _ = ionText(ionFieldValue(value, 186))
	position := ionFieldValue(value, 183)
	targetValue := ionFieldValue(position, 155)
	if target, ok := ionID(targetValue); ok && target <= uint64(^uint32(0)) {
		item.targetNode = uint32(target)
		_, item.targetSymbol = ionSymbolID(targetValue)
	}
	if offset, ok := ionInteger(ionFieldValue(position, 143)); ok && offset >= 0 {
		item.offset = int(offset)
	}
	book.anchors[id] = item
}

func (book *decodedBook) collectContent(id uint32, value *ionValue) {
	items := ionFieldValue(value, 146)
	if items == nil || items.kind != ionList {
		return
	}
	for _, item := range items.children {
		if text, ok := ionText(item); ok {
			book.contents[id] = append(book.contents[id], text)
		}
	}
}

func (book *decodedBook) collectMetadata(value *ionValue) {
	categories := ionFieldValue(value, 491)
	if categories == nil || categories.kind != ionList {
		return
	}
	for _, category := range categories.children {
		name, ok := ionText(ionFieldValue(category, 495))
		if !ok {
			continue
		}
		if book.metadata[name] == nil {
			book.metadata[name] = make(map[string][]string)
		}
		if book.metadataRaw[name] == nil {
			book.metadataRaw[name] = make(map[string][]*ionValue)
		}
		entries := ionFieldValue(category, 258)
		if entries == nil || entries.kind != ionList {
			continue
		}
		for _, entry := range entries.children {
			key, keyOK := ionText(ionFieldValue(entry, 492))
			dataValue := ionFieldValue(entry, 307)
			if keyOK && dataValue != nil {
				book.metadataRaw[name][key] = append(book.metadataRaw[name][key], dataValue)
				if data, dataOK := ionText(dataValue); dataOK {
					book.metadata[name][key] = append(book.metadata[name][key], data)
				}
			}
		}
	}
}

func (book *decodedBook) collectDependencies(entityMap *ionValue) {
	entries := ionFieldValue(entityMap, 253)
	if entries == nil || entries.kind != ionList {
		return
	}
	for _, entry := range entries.children {
		id, ok := ionSymbolID(ionFieldValue(entry, 155))
		if !ok || id > ^uint64(0)>>32 {
			continue
		}
		mandatory := ionFieldValue(entry, 254)
		if mandatory == nil || mandatory.kind != ionList {
			continue
		}
		for _, dependency := range mandatory.children {
			dependencyID, symbolOK := ionSymbolID(dependency)
			if symbolOK {
				book.dependencies[uint32(id)] = append(book.dependencies[uint32(id)],
					resolveSymbol(dependencyID, book.symbols))
			}
		}
	}
}

func readingOrderSections(document *ionValue) []uint32 {
	orders := ionFieldValue(document, 169)
	if orders == nil || orders.kind != ionList {
		return nil
	}
	var result []uint32
	seen := make(map[uint32]bool)
	for _, order := range orders.children {
		sections := ionFieldValue(order, 170)
		if sections == nil || sections.kind != ionList {
			continue
		}
		for _, section := range sections.children {
			id, ok := ionSymbolID(section)
			if ok && id <= ^uint64(0)>>32 && !seen[uint32(id)] {
				seen[uint32(id)] = true
				result = append(result, uint32(id))
			}
		}
	}
	return result
}

func uniqueSymbols(value *ionValue, fieldID uint64) []uint32 {
	var result []uint32
	seen := make(map[uint32]bool)
	var visit func(*ionValue)
	visit = func(item *ionValue) {
		if item == nil {
			return
		}
		if item.kind == ionStruct {
			for _, field := range item.fields {
				if field.id == fieldID {
					if id, ok := ionSymbolID(field.value); ok && id <= ^uint64(0)>>32 && !seen[uint32(id)] {
						seen[uint32(id)] = true
						result = append(result, uint32(id))
					}
				} else {
					visit(field.value)
				}
			}
		}
		for _, child := range item.children {
			visit(child)
		}
	}
	visit(value)
	return result
}

func (book *decodedBook) collectStoryResources(storyID uint32, result *[]uint32, seen map[uint32]bool) error {
	if seen[storyID] {
		return nil
	}
	seen[storyID] = true
	story := book.storylines[storyID]
	if story == nil {
		return fmt.Errorf("missing storyline $%d", storyID)
	}
	var visit func(*ionValue) error
	visit = func(value *ionValue) error {
		if value == nil {
			return nil
		}
		if value.kind == ionStruct {
			if ignored, ok := ionBoolean(ionFieldValue(value, 69)); ok && ignored {
				return nil
			}
			kind, _ := ionSymbolID(ionFieldValue(value, 159))
			if kind == kfxImageType {
				resourceID, ok := ionSymbolID(ionFieldValue(value, 175))
				if !ok || resourceID > ^uint64(0)>>32 {
					return errors.New("image node has no valid resource name")
				}
				*result = append(*result, uint32(resourceID))
			}
			if backgroundID, ok := ionSymbolID(ionFieldValue(value, 479)); ok && backgroundID <= ^uint64(0)>>32 {
				*result = append(*result, uint32(backgroundID))
			}
			for _, field := range value.fields {
				if field.id == 176 {
					if nestedID, ok := ionSymbolID(field.value); ok && nestedID != uint64(storyID) && nestedID <= ^uint64(0)>>32 {
						if err := book.collectStoryResources(uint32(nestedID), result, seen); err != nil {
							return err
						}
					}
					continue
				}
				if err := visit(field.value); err != nil {
					return err
				}
			}
		}
		for _, child := range value.children {
			if err := visit(child); err != nil {
				return err
			}
		}
		return nil
	}
	return visit(ionFieldValue(story, 146))
}

func ionSymbolID(value *ionValue) (uint64, bool) {
	if value == nil || value.kind != ionSymbol {
		return 0, false
	}
	return value.unsigned, true
}

func ionID(value *ionValue) (uint64, bool) {
	if id, ok := ionSymbolID(value); ok {
		return id, true
	}
	return ionUint(value)
}

func ionText(value *ionValue) (string, bool) {
	if value == nil || value.kind != ionString {
		return "", false
	}
	return value.text, true
}

func ionInteger(value *ionValue) (int64, bool) {
	if value == nil || value.kind != ionInt {
		return 0, false
	}
	return value.integer, true
}

func ionBoolean(value *ionValue) (bool, bool) {
	if value == nil || value.kind != ionBool {
		return false, false
	}
	return value.boolean, true
}
