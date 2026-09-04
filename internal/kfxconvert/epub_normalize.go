package kfxconvert

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"io"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"strconv"
	"strings"
)

// EPUBNormalizationResult describes a safely repackaged EPUB and whether its
// cover metadata had to be repaired or generated.
type EPUBNormalizationResult struct {
	Entries       int
	SpineItems    int
	CoverRepaired bool
}

type epubContainerDocument struct {
	Rootfiles []struct {
		FullPath  string `xml:"full-path,attr"`
		MediaType string `xml:"media-type,attr"`
	} `xml:"rootfiles>rootfile"`
}

type epubPackageDocument struct {
	Version          string `xml:"version,attr"`
	UniqueIdentifier string `xml:"unique-identifier,attr"`
	Metadata         struct {
		Identifiers []struct {
			ID    string `xml:"id,attr"`
			Value string `xml:",chardata"`
		} `xml:"identifier"`
		Titles    []string `xml:"title"`
		Languages []string `xml:"language"`
		Metas     []struct {
			Name     string `xml:"name,attr"`
			Content  string `xml:"content,attr"`
			Property string `xml:"property,attr"`
			Value    string `xml:",chardata"`
		} `xml:"meta"`
	} `xml:"metadata"`
	Manifest struct {
		Items []struct {
			ID         string `xml:"id,attr"`
			Href       string `xml:"href,attr"`
			MediaType  string `xml:"media-type,attr"`
			Properties string `xml:"properties,attr"`
		} `xml:"item"`
	} `xml:"manifest"`
	Spine struct {
		Direction string `xml:"page-progression-direction,attr"`
		Items     []struct {
			IDRef string `xml:"idref,attr"`
		} `xml:"itemref"`
	} `xml:"spine"`
}

type epubArchiveInspection struct {
	archive      *zip.ReadCloser
	files        map[string]*zip.File
	container    epubContainerDocument
	packagePath  string
	packageData  []byte
	packageDoc   epubPackageDocument
	manifestByID map[string]struct {
		Href       string
		MediaType  string
		Properties string
	}
	coverID       string
	coverHref     string
	coverPath     string
	firstSpineDoc string
}

// NormalizeEPUB creates a new EPUB with canonical OCF packaging and repairs its
// internal cover metadata. Existing cover content is moved instead of copied;
// when cover metadata is unusable, the first non-empty spine page is the
// fallback source. The source archive is preserved and the destination must
// not already exist.
func NormalizeEPUB(source, destination string) (result EPUBNormalizationResult, err error) {
	inspection, err := inspectEPUBArchive(source)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, inspection.archive.Close()) }()
	if len(inspection.packageDoc.Spine.Items) == 0 {
		return result, errors.New("EPUB package has an empty spine")
	}

	modified := make(map[string][]byte)
	fontChanges, removedFontIDs, removedPaths, fontErr := inspection.pruneUnusedCFFFonts()
	if fontErr != nil {
		return result, fontErr
	}
	for name, data := range fontChanges {
		modified[name] = data
	}
	selection, selectErr := inspection.selectEPUBCover()
	if selectErr != nil {
		return result, selectErr
	}
	coverID := selection.manifestID
	newManifestItem := ""
	if coverID == "" {
		coverID = uniqueEPUBID(inspection.packageDoc, "leafport-fallback-cover-image")
		href, relativeErr := relativeEPUBPath(pathpkg.Dir(inspection.packagePath), selection.path)
		if relativeErr != nil {
			return result, relativeErr
		}
		newManifestItem = `<item id="` + escapeXML(coverID) + `" href="` +
			escapeXML(href) + `" media-type="` + escapeXML(selection.mediaType) + `" properties="cover-image"/>`
		modified[selection.path] = selection.data
	}
	packageData, err := updateEPUBCoverMetadata(inspection.packageData, coverID, newManifestItem)
	if err != nil {
		return result, err
	}
	if len(removedFontIDs) != 0 {
		packageData, err = removeEPUBManifestItems(packageData, removedFontIDs)
		if err != nil {
			return result, err
		}
	}
	if selection.documentPath != "" {
		documentID := inspection.manifestIDForPath(selection.documentPath)
		if documentID == "" {
			return result, fmt.Errorf("cover source document is not in the manifest: %s", selection.documentPath)
		}
		packageData, err = moveEPUBSpineItemFirst(packageData, documentID)
		if err != nil {
			return result, err
		}
	}
	modified[inspection.packagePath] = packageData
	result.CoverRepaired = inspection.coverID != coverID || newManifestItem != "" ||
		(selection.documentPath != "" && selection.documentPath != inspection.firstSpineDoc)

	output, err := os.OpenFile(destination, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return result, err
	}
	complete := false
	defer func() {
		if output != nil {
			err = errors.Join(err, output.Close())
		}
		if !complete {
			err = errors.Join(err, os.Remove(destination))
		}
	}()
	writer := zip.NewWriter(output)
	if err = writeZIPEntry(writer, "mimetype", "application/epub+zip", zip.Store); err != nil {
		return result, err
	}
	written := map[string]bool{"mimetype": true}
	for _, entry := range inspection.archive.File {
		if entry.Name == "mimetype" {
			continue
		}
		if removedPaths[entry.Name] {
			continue
		}
		data, ok := modified[entry.Name]
		if !ok {
			data, err = readEPUBEntry(entry)
			if err != nil {
				return result, err
			}
		}
		method := entry.Method
		if method != zip.Store && method != zip.Deflate {
			method = zip.Deflate
		}
		if err = writeZIPBytes(writer, entry.Name, data, method); err != nil {
			return result, err
		}
		written[entry.Name] = true
	}
	for name, data := range modified {
		if written[name] {
			continue
		}
		if err = writeZIPBytes(writer, name, data, zip.Deflate); err != nil {
			return result, err
		}
		written[name] = true
	}
	if err = writer.Close(); err != nil {
		return result, err
	}
	if err = output.Sync(); err != nil {
		return result, err
	}
	if err = output.Close(); err != nil {
		output = nil
		return result, err
	}
	output = nil
	validated, err := ValidateEPUBFile(destination)
	if err != nil {
		return result, err
	}
	result.Entries = validated.Entries
	result.SpineItems = validated.SpineItems
	complete = true
	return result, nil
}

type epubFallbackCover struct {
	path         string
	mediaType    string
	data         []byte
	manifestID   string
	documentPath string
}

const kindleCoverMinimumSide = 500

func (inspection *epubArchiveInspection) selectEPUBCover() (epubFallbackCover, error) {
	// A cover landmark identifies the semantic cover page and is stronger than
	// broken converter metadata. Several real Kindle exports otherwise point
	// cover-image at a small advertisement near the end of the book.
	if documentPath, err := inspection.coverLandmarkDocument(); err != nil {
		return epubFallbackCover{}, err
	} else if documentPath != "" {
		if selected, found, selectErr := inspection.coverFromDocument(documentPath); selectErr != nil {
			return epubFallbackCover{}, selectErr
		} else if found {
			return selected, nil
		}
	}

	if inspection.coverPath != "" {
		width, height, dimensionErr := inspection.imageDimensions(inspection.coverPath)
		if dimensionErr != nil {
			return epubFallbackCover{}, dimensionErr
		}
		if kindleCoverDimensions(width, height) {
			references, referenceErr := inspection.spineDocumentsReferencing(inspection.coverPath)
			if referenceErr != nil {
				return epubFallbackCover{}, referenceErr
			}
			documentPath := ""
			if len(references) != 0 {
				documentPath = references[0]
			}
			return epubFallbackCover{
				path: inspection.coverPath, mediaType: inspection.mediaTypeForPath(inspection.coverPath),
				manifestID: inspection.coverID, documentPath: documentPath,
			}, nil
		}
	}
	return inspection.fallbackCoverFromFirstNonemptyPage()
}

func (inspection *epubArchiveInspection) fallbackCoverFromFirstNonemptyPage() (epubFallbackCover, error) {
	selected, pageText, err := inspection.firstNonemptySpineSection()
	if err != nil {
		return epubFallbackCover{}, err
	}
	if selected.path == "" {
		return epubFallbackCover{}, errors.New("EPUB has no non-empty spine page for a fallback cover")
	}
	if cover, found, selectErr := inspection.coverFromDocument(selected.path); selectErr != nil {
		return epubFallbackCover{}, selectErr
	} else if found {
		return cover, nil
	}
	data, err := fallbackPageCoverJPEG(firstNonempty(inspection.packageDoc.Metadata.Titles, "Untitled"), pageText)
	if err != nil {
		return epubFallbackCover{}, err
	}
	name := uniqueEPUBName(inspection.files, pathpkg.Join(pathpkg.Dir(inspection.packagePath), "leafport-fallback-cover.jpg"))
	return epubFallbackCover{
		path: name, mediaType: "image/jpeg", data: data, documentPath: selected.path,
	}, nil
}

func (inspection *epubArchiveInspection) firstNonemptySpineSection() (epubSection, string, error) {
	for _, spineItem := range inspection.packageDoc.Spine.Items {
		manifest := inspection.manifestByID[spineItem.IDRef]
		if manifest.Href == "" {
			return epubSection{}, "", fmt.Errorf("EPUB spine item %q does not resolve through the manifest", spineItem.IDRef)
		}
		path, err := resolveEPUBReference(inspection.packagePath, manifest.Href)
		if err != nil {
			return epubSection{}, "", err
		}
		entry := inspection.files[path]
		if entry == nil {
			return epubSection{}, "", fmt.Errorf("EPUB spine document is missing: %s", path)
		}
		data, err := readEPUBEntry(entry)
		if err != nil {
			return epubSection{}, "", err
		}
		section := epubSection{path: path, data: data}
		index, text, inspectErr := firstNonemptyEPUBSection([]epubSection{section})
		if inspectErr != nil {
			return epubSection{}, "", inspectErr
		}
		if index == 0 {
			return section, text, nil
		}
	}
	return epubSection{}, "", nil
}

func (inspection *epubArchiveInspection) coverFromDocument(documentPath string) (epubFallbackCover, bool, error) {
	entry := inspection.files[documentPath]
	if entry == nil {
		return epubFallbackCover{}, false, fmt.Errorf("EPUB cover document is missing: %s", documentPath)
	}
	data, err := readEPUBEntry(entry)
	if err != nil {
		return epubFallbackCover{}, false, err
	}
	document, err := inspectEPUBXML(bytes.NewReader(data))
	if err != nil {
		return epubFallbackCover{}, false, fmt.Errorf("decode EPUB cover document %s: %w", documentPath, err)
	}
	for _, reference := range document.hrefs {
		path, resolveErr := resolveEPUBReference(documentPath, reference)
		if resolveErr != nil {
			continue
		}
		manifestID := inspection.manifestIDForPath(path)
		mediaType := inspection.mediaTypeForPath(path)
		if manifestID == "" || !strings.HasPrefix(mediaType, "image/") {
			continue
		}
		width, height, dimensionErr := inspection.imageDimensions(path)
		if dimensionErr != nil {
			return epubFallbackCover{}, false, dimensionErr
		}
		if !kindleCoverDimensions(width, height) {
			continue
		}
		return epubFallbackCover{
			path: path, mediaType: mediaType, manifestID: manifestID, documentPath: documentPath,
		}, true, nil
	}
	return epubFallbackCover{}, false, nil
}

func kindleCoverDimensions(width, height int) bool {
	return width >= kindleCoverMinimumSide && height >= kindleCoverMinimumSide
}

type epubCSSRange struct {
	start int
	end   int
}

type epubCSSFontFace struct {
	cssPath string
	block   epubCSSRange
	family  string
}

// pruneUnusedCFFFonts removes only PostScript-flavoured OpenType fonts whose
// family is never selected outside its own @font-face block. Amazon's compiler
// warns that these may render poorly; retaining any used family is safer than
// guessing at a font conversion.
func (inspection *epubArchiveInspection) pruneUnusedCFFFonts() (map[string][]byte, map[string]bool, map[string]bool, error) {
	candidates := make(map[string]string)
	for _, item := range inspection.packageDoc.Manifest.Items {
		if !strings.HasPrefix(item.MediaType, "font/") && item.MediaType != "application/vnd.ms-opentype" {
			continue
		}
		fontPath, err := resolveEPUBReference(inspection.packagePath, item.Href)
		if err != nil {
			return nil, nil, nil, err
		}
		entry := inspection.files[fontPath]
		if entry == nil {
			continue
		}
		data, err := readEPUBEntry(entry)
		if err != nil {
			return nil, nil, nil, err
		}
		if len(data) >= 4 && bytes.Equal(data[:4], []byte("OTTO")) {
			candidates[fontPath] = item.ID
		}
	}
	changes := make(map[string][]byte)
	removedIDs := make(map[string]bool)
	removedPaths := make(map[string]bool)
	if len(candidates) == 0 {
		return changes, removedIDs, removedPaths, nil
	}

	cssData := make(map[string][]byte)
	cssBlocks := make(map[string][]epubCSSRange)
	facesByFont := make(map[string][]epubCSSFontFace)
	for _, item := range inspection.packageDoc.Manifest.Items {
		if item.MediaType != "text/css" {
			continue
		}
		cssPath, err := resolveEPUBReference(inspection.packagePath, item.Href)
		if err != nil {
			return nil, nil, nil, err
		}
		entry := inspection.files[cssPath]
		if entry == nil {
			continue
		}
		data, err := readEPUBEntry(entry)
		if err != nil {
			return nil, nil, nil, err
		}
		cssData[cssPath] = data
		blocks := cssFontFaceBlocks(data)
		cssBlocks[cssPath] = blocks
		for _, block := range blocks {
			content := data[block.start:block.end]
			family := cssDeclarationValue(content, "font-family")
			for _, raw := range cssURLValues(content) {
				fontPath, resolveErr := resolveEPUBReference(cssPath, raw)
				if resolveErr == nil {
					facesByFont[fontPath] = append(facesByFont[fontPath], epubCSSFontFace{
						cssPath: cssPath, block: block, family: family,
					})
				}
			}
		}
	}

	removeBlocks := make(map[string]map[int]bool)
	for fontPath, id := range candidates {
		faces := facesByFont[fontPath]
		if len(faces) == 0 {
			continue
		}
		unused := true
		for _, face := range faces {
			if face.family == "" || inspection.epubFontFamilyUsed(face.family, cssData, cssBlocks) {
				unused = false
				break
			}
		}
		if !unused {
			continue
		}
		removedIDs[id] = true
		removedPaths[fontPath] = true
		for _, face := range faces {
			if removeBlocks[face.cssPath] == nil {
				removeBlocks[face.cssPath] = make(map[int]bool)
			}
			removeBlocks[face.cssPath][face.block.start] = true
		}
	}
	for cssPath, starts := range removeBlocks {
		data := cssData[cssPath]
		cursor := 0
		var output bytes.Buffer
		for _, block := range cssBlocks[cssPath] {
			if !starts[block.start] {
				continue
			}
			output.Write(data[cursor:block.start])
			cursor = block.end
		}
		output.Write(data[cursor:])
		changes[cssPath] = output.Bytes()
	}
	return changes, removedIDs, removedPaths, nil
}

func (inspection *epubArchiveInspection) epubFontFamilyUsed(family string, cssData map[string][]byte, cssBlocks map[string][]epubCSSRange) bool {
	needle := []byte(strings.ToLower(strings.Trim(strings.TrimSpace(family), `"'`)))
	if len(needle) == 0 {
		return true
	}
	for cssPath, data := range cssData {
		cursor := 0
		for _, block := range cssBlocks[cssPath] {
			if bytes.Contains(bytes.ToLower(data[cursor:block.start]), needle) {
				return true
			}
			cursor = block.end
		}
		if bytes.Contains(bytes.ToLower(data[cursor:]), needle) {
			return true
		}
	}
	for name, entry := range inspection.files {
		lower := strings.ToLower(name)
		if !strings.HasSuffix(lower, ".xhtml") && !strings.HasSuffix(lower, ".html") && !strings.HasSuffix(lower, ".svg") {
			continue
		}
		data, err := readEPUBEntry(entry)
		if err != nil {
			return true
		}
		if bytes.Contains(bytes.ToLower(data), needle) {
			return true
		}
	}
	return false
}

func cssFontFaceBlocks(data []byte) []epubCSSRange {
	var blocks []epubCSSRange
	for index := 0; index < len(data); {
		if index+1 < len(data) && data[index] == '/' && data[index+1] == '*' {
			index = skipCSSComment(data, index)
			continue
		}
		if data[index] == '\'' || data[index] == '"' {
			index = skipCSSString(data, index)
			continue
		}
		const marker = "@font-face"
		if index+len(marker) > len(data) || !bytes.EqualFold(data[index:index+len(marker)], []byte(marker)) {
			index++
			continue
		}
		start := index
		index += len(marker)
		for index < len(data) && data[index] != '{' {
			if index+1 < len(data) && data[index] == '/' && data[index+1] == '*' {
				index = skipCSSComment(data, index)
				continue
			}
			index++
		}
		if index == len(data) {
			break
		}
		depth := 1
		index++
		for index < len(data) && depth != 0 {
			switch {
			case index+1 < len(data) && data[index] == '/' && data[index+1] == '*':
				index = skipCSSComment(data, index)
			case data[index] == '\'' || data[index] == '"':
				index = skipCSSString(data, index)
			case data[index] == '{':
				depth++
				index++
			case data[index] == '}':
				depth--
				index++
			default:
				index++
			}
		}
		if depth == 0 {
			blocks = append(blocks, epubCSSRange{start: start, end: index})
		}
	}
	return blocks
}

func skipCSSComment(data []byte, index int) int {
	index += 2
	for index+1 < len(data) {
		if data[index] == '*' && data[index+1] == '/' {
			return index + 2
		}
		index++
	}
	return len(data)
}

func skipCSSString(data []byte, index int) int {
	quote := data[index]
	index++
	for index < len(data) {
		if data[index] == '\\' && index+1 < len(data) {
			index += 2
			continue
		}
		if data[index] == quote {
			return index + 1
		}
		index++
	}
	return len(data)
}

func cssDeclarationValue(block []byte, name string) string {
	lower := bytes.ToLower(block)
	marker := []byte(strings.ToLower(name))
	index := bytes.Index(lower, marker)
	if index < 0 {
		return ""
	}
	index += len(marker)
	for index < len(block) && isXMLSpace(block[index]) {
		index++
	}
	if index == len(block) || block[index] != ':' {
		return ""
	}
	index++
	start := index
	for index < len(block) && block[index] != ';' && block[index] != '}' {
		index++
	}
	return strings.Trim(strings.TrimSpace(string(block[start:index])), `"'`)
}

func cssURLValues(block []byte) []string {
	lower := bytes.ToLower(block)
	var values []string
	for cursor := 0; cursor < len(block); {
		index := bytes.Index(lower[cursor:], []byte("url("))
		if index < 0 {
			break
		}
		index += cursor + len("url(")
		end := index
		quote := byte(0)
		for end < len(block) {
			if quote == 0 && (block[end] == '\'' || block[end] == '"') {
				quote = block[end]
			} else if quote != 0 && block[end] == quote {
				quote = 0
			} else if quote == 0 && block[end] == ')' {
				break
			}
			end++
		}
		if end == len(block) {
			break
		}
		value := strings.Trim(strings.TrimSpace(string(block[index:end])), `"'`)
		if value != "" {
			values = append(values, value)
		}
		cursor = end + 1
	}
	return values
}

func (inspection *epubArchiveInspection) manifestIDForPath(target string) string {
	for _, item := range inspection.packageDoc.Manifest.Items {
		resolved, err := resolveEPUBReference(inspection.packagePath, item.Href)
		if err == nil && resolved == target {
			return item.ID
		}
	}
	return ""
}

func (inspection *epubArchiveInspection) mediaTypeForPath(target string) string {
	for _, item := range inspection.packageDoc.Manifest.Items {
		resolved, err := resolveEPUBReference(inspection.packagePath, item.Href)
		if err == nil && resolved == target {
			return item.MediaType
		}
	}
	return ""
}

func (inspection *epubArchiveInspection) imageDimensions(target string) (int, int, error) {
	entry := inspection.files[target]
	if entry == nil {
		return 0, 0, fmt.Errorf("EPUB image is missing: %s", target)
	}
	data, err := readEPUBEntry(entry)
	if err != nil {
		return 0, 0, err
	}
	mediaType := inspection.mediaTypeForPath(target)
	if mediaType == "image/svg+xml" {
		width, height, decodeErr := svgImageDimensions(data)
		if decodeErr != nil {
			return 0, 0, fmt.Errorf("decode EPUB SVG image %s: %w", target, decodeErr)
		}
		return width, height, nil
	}
	configuration, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, fmt.Errorf("inspect EPUB image %s: %w", target, err)
	}
	if err := validateRasterDimensions(configuration.Width, configuration.Height, target); err != nil {
		return 0, 0, err
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return 0, 0, fmt.Errorf("decode EPUB image %s: %w", target, err)
	}
	return decoded.Bounds().Dx(), decoded.Bounds().Dy(), nil
}

func (inspection *epubArchiveInspection) spineDocumentsReferencing(target string) ([]string, error) {
	var references []string
	for _, spineItem := range inspection.packageDoc.Spine.Items {
		manifest := inspection.manifestByID[spineItem.IDRef]
		if manifest.Href == "" {
			return nil, fmt.Errorf("EPUB spine item %q does not resolve through the manifest", spineItem.IDRef)
		}
		documentPath, err := resolveEPUBReference(inspection.packagePath, manifest.Href)
		if err != nil {
			return nil, err
		}
		found, err := epubDocumentReferences(inspection.files, documentPath, target)
		if err != nil {
			return nil, err
		}
		if found {
			references = append(references, documentPath)
		}
	}
	return references, nil
}

func (inspection *epubArchiveInspection) coverLandmarkDocument() (string, error) {
	for _, item := range inspection.packageDoc.Manifest.Items {
		if !hasToken(item.Properties, "nav") {
			continue
		}
		navPath, err := resolveEPUBReference(inspection.packagePath, item.Href)
		if err != nil {
			return "", err
		}
		entry := inspection.files[navPath]
		if entry == nil {
			return "", fmt.Errorf("EPUB navigation document is missing: %s", navPath)
		}
		data, err := readEPUBEntry(entry)
		if err != nil {
			return "", err
		}
		decoder := xml.NewDecoder(bytes.NewReader(data))
		landmarksDepth := 0
		for {
			token, tokenErr := decoder.Token()
			if errors.Is(tokenErr, io.EOF) {
				break
			}
			if tokenErr != nil {
				return "", fmt.Errorf("decode EPUB navigation document %s: %w", navPath, tokenErr)
			}
			switch value := token.(type) {
			case xml.StartElement:
				if landmarksDepth != 0 {
					landmarksDepth++
				}
				typeValue, href := "", ""
				for _, attribute := range value.Attr {
					switch attribute.Name.Local {
					case "type":
						typeValue = attribute.Value
					case "href":
						href = attribute.Value
					}
				}
				if value.Name.Local == "nav" && hasToken(typeValue, "landmarks") {
					landmarksDepth = 1
					continue
				}
				if landmarksDepth != 0 && value.Name.Local == "a" && hasToken(typeValue, "cover") && href != "" {
					return resolveEPUBReference(navPath, href)
				}
			case xml.EndElement:
				if landmarksDepth != 0 {
					landmarksDepth--
				}
			}
		}
	}
	return "", nil
}

// EPUBValidationResult contains the compatibility invariants checked in
// addition to XML and ZIP integrity. EPUBCheck remains the authoritative
// external conformance checker used by the project test gate.
type EPUBValidationResult struct {
	Entries     int
	SpineItems  int
	Images      int
	CoverPath   string
	CoverFirst  bool
	CoverWidth  int
	CoverHeight int
}

// ValidateEPUBFile validates OCF packaging, package references, all XML, all
// raster image streams, and the Kindle internal-cover compatibility profile.
func ValidateEPUBFile(path string) (result EPUBValidationResult, err error) {
	inspection, err := inspectEPUBArchive(path)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, inspection.archive.Close()) }()
	result.Entries = len(inspection.archive.File)
	result.SpineItems = len(inspection.packageDoc.Spine.Items)
	result.CoverPath = inspection.coverPath
	if result.SpineItems == 0 {
		return result, errors.New("validate EPUB: package spine is empty")
	}
	if inspection.coverPath == "" {
		return result, errors.New("validate EPUB: no cover-image manifest item")
	}
	firstNonempty, _, err := inspection.firstNonemptySpineSection()
	if err != nil {
		return result, err
	}
	references, err := inspection.spineDocumentsReferencing(inspection.coverPath)
	if err != nil {
		return result, err
	}
	// Amazon uses the cover-image metadata itself and explicitly discourages an
	// additional generated HTML cover page. Metadata-only covers are therefore
	// valid. If the image is also existing content, it must occur once on the
	// first non-empty spine page.
	result.CoverFirst = len(references) == 0 ||
		(len(references) == 1 && firstNonempty.path != "" && references[0] == firstNonempty.path)
	if !result.CoverFirst {
		return result, errors.New("validate EPUB: cover content is duplicated or is not the first non-empty spine page")
	}
	files := make(map[string]bool, len(inspection.files))
	documents := make(map[string]epubXMLDocument)
	for name, entry := range inspection.files {
		files[name] = true
		lower := strings.ToLower(name)
		switch {
		case strings.HasSuffix(lower, ".xhtml"), strings.HasSuffix(lower, ".html"),
			strings.HasSuffix(lower, ".xml"), strings.HasSuffix(lower, ".opf"), strings.HasSuffix(lower, ".svg"):
			data, readErr := readEPUBEntry(entry)
			if readErr != nil {
				return result, readErr
			}
			document, decodeErr := inspectEPUBXML(bytes.NewReader(data))
			if decodeErr != nil {
				return result, fmt.Errorf("validate EPUB XML %s: %w", name, decodeErr)
			}
			documents[name] = document
		}
	}
	if err := validateEPUBReferences(files, documents); err != nil {
		return result, err
	}
	for _, item := range inspection.packageDoc.Manifest.Items {
		resolved, resolveErr := resolveEPUBReference(inspection.packagePath, item.Href)
		if resolveErr != nil {
			return result, resolveErr
		}
		entry := inspection.files[resolved]
		if entry == nil {
			return result, fmt.Errorf("validate EPUB: manifest item %q targets missing file %s", item.ID, resolved)
		}
		if item.MediaType == "image/svg+xml" {
			data, readErr := readEPUBEntry(entry)
			if readErr != nil {
				return result, readErr
			}
			width, height, decodeErr := svgImageDimensions(data)
			if decodeErr != nil {
				return result, fmt.Errorf("validate EPUB image %s: %w", resolved, decodeErr)
			}
			result.Images++
			if resolved == inspection.coverPath {
				result.CoverWidth, result.CoverHeight = width, height
			}
		} else if strings.HasPrefix(item.MediaType, "image/") {
			data, readErr := readEPUBEntry(entry)
			if readErr != nil {
				return result, readErr
			}
			configuration, _, decodeErr := image.DecodeConfig(bytes.NewReader(data))
			if decodeErr != nil {
				return result, fmt.Errorf("inspect EPUB image %s: %w", resolved, decodeErr)
			}
			if decodeErr = validateRasterDimensions(configuration.Width, configuration.Height, resolved); decodeErr != nil {
				return result, decodeErr
			}
			decoded, _, decodeErr := image.Decode(bytes.NewReader(data))
			if decodeErr != nil {
				return result, fmt.Errorf("validate EPUB image %s: %w", resolved, decodeErr)
			}
			result.Images++
			if resolved == inspection.coverPath {
				result.CoverWidth = decoded.Bounds().Dx()
				result.CoverHeight = decoded.Bounds().Dy()
			}
		}
	}
	if result.CoverWidth <= 0 || result.CoverHeight <= 0 {
		return result, errors.New("validate EPUB: cover image dimensions are unavailable")
	}
	if !kindleCoverDimensions(result.CoverWidth, result.CoverHeight) {
		return result, fmt.Errorf("validate EPUB: cover is too small for the Kindle profile: %dx%d (minimum side %d px)",
			result.CoverWidth, result.CoverHeight, kindleCoverMinimumSide)
	}
	return result, nil
}

func svgImageDimensions(data []byte) (int, int, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return 0, 0, errors.New("SVG has no root element")
		}
		if err != nil {
			return 0, 0, err
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		if start.Name.Local != "svg" {
			return 0, 0, errors.New("image/svg+xml resource has a non-SVG root element")
		}
		width, height := 0, 0
		viewBox := ""
		for _, attribute := range start.Attr {
			switch attribute.Name.Local {
			case "width":
				width = svgNumericDimension(attribute.Value)
			case "height":
				height = svgNumericDimension(attribute.Value)
			case "viewBox":
				viewBox = attribute.Value
			}
		}
		if width <= 0 || height <= 0 {
			fields := strings.Fields(strings.ReplaceAll(viewBox, ",", " "))
			if len(fields) == 4 {
				width = svgNumericDimension(fields[2])
				height = svgNumericDimension(fields[3])
			}
		}
		if width <= 0 || height <= 0 {
			return 0, 0, errors.New("SVG has no positive intrinsic dimensions or viewBox")
		}
		return width, height, nil
	}
}

func svgNumericDimension(value string) int {
	value = strings.TrimSpace(value)
	end := 0
	for end < len(value) {
		character := value[end]
		if (character >= '0' && character <= '9') || character == '.' || character == '+' || character == '-' || character == 'e' || character == 'E' {
			end++
			continue
		}
		break
	}
	if end == 0 {
		return 0
	}
	number, err := strconv.ParseFloat(value[:end], 64)
	if err != nil || number <= 0 {
		return 0
	}
	return int(number + 0.5)
}

func inspectEPUBArchive(path string) (*epubArchiveInspection, error) {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("open EPUB ZIP: %w", err)
	}
	fail := func(cause error) (*epubArchiveInspection, error) {
		return nil, errors.Join(cause, archive.Close())
	}
	if len(archive.File) == 0 || archive.File[0].Name != "mimetype" || archive.File[0].Method != zip.Store ||
		archive.File[0].Flags&0x9 != 0 || len(archive.File[0].Extra) != 0 {
		return fail(errors.New("EPUB mimetype must be the first uncompressed, unencrypted, descriptor-free entry without extra fields"))
	}
	if len(archive.File) > maximumArchiveEntries {
		return fail(fmt.Errorf("EPUB has %d entries; safety limit is %d", len(archive.File), maximumArchiveEntries))
	}
	mimetype, err := readEPUBEntry(archive.File[0])
	if err != nil || !bytes.Equal(mimetype, []byte("application/epub+zip")) {
		return fail(errors.Join(errors.New("EPUB has invalid mimetype contents"), err))
	}
	inspection := &epubArchiveInspection{archive: archive, files: make(map[string]*zip.File)}
	var expandedBytes uint64
	for _, entry := range archive.File {
		if entry.UncompressedSize64 > maximumExpandedArchiveBytes-expandedBytes {
			return fail(errors.New("EPUB expanded size exceeds the 16 GiB safety limit"))
		}
		expandedBytes += entry.UncompressedSize64
		if err := validateEPUBEntryName(entry.Name); err != nil {
			return fail(err)
		}
		if entry.Flags&1 != 0 {
			return fail(fmt.Errorf("EPUB entry %s is encrypted", entry.Name))
		}
		if inspection.files[entry.Name] != nil {
			return fail(fmt.Errorf("EPUB contains duplicate entry %s", entry.Name))
		}
		inspection.files[entry.Name] = entry
	}
	containerEntry := inspection.files["META-INF/container.xml"]
	if containerEntry == nil {
		return fail(errors.New("EPUB is missing META-INF/container.xml"))
	}
	containerData, err := readEPUBEntry(containerEntry)
	if err != nil {
		return fail(err)
	}
	if err := xml.Unmarshal(containerData, &inspection.container); err != nil {
		return fail(fmt.Errorf("decode EPUB container: %w", err))
	}
	if len(inspection.container.Rootfiles) != 1 {
		return fail(fmt.Errorf("EPUB container has %d rootfiles; exactly one is required by Leafport's compatibility profile", len(inspection.container.Rootfiles)))
	}
	if inspection.container.Rootfiles[0].MediaType != "application/oebps-package+xml" {
		return fail(fmt.Errorf("EPUB rootfile has unsupported media type %q", inspection.container.Rootfiles[0].MediaType))
	}
	inspection.packagePath = inspection.container.Rootfiles[0].FullPath
	if err := validateEPUBEntryName(inspection.packagePath); err != nil {
		return fail(err)
	}
	packageEntry := inspection.files[inspection.packagePath]
	if packageEntry == nil {
		return fail(fmt.Errorf("EPUB package document is missing: %s", inspection.packagePath))
	}
	inspection.packageData, err = readEPUBEntry(packageEntry)
	if err != nil {
		return fail(err)
	}
	if err := xml.Unmarshal(inspection.packageData, &inspection.packageDoc); err != nil {
		return fail(fmt.Errorf("decode EPUB package: %w", err))
	}
	if inspection.packageDoc.Version != "3.0" {
		return fail(fmt.Errorf("EPUB package version %q is unsupported; EPUB 3 is required", inspection.packageDoc.Version))
	}
	if len(inspection.packageDoc.Metadata.Titles) == 0 || len(inspection.packageDoc.Metadata.Languages) == 0 ||
		len(inspection.packageDoc.Metadata.Identifiers) == 0 {
		return fail(errors.New("EPUB package is missing required identifier, title, or language metadata"))
	}
	inspection.manifestByID = make(map[string]struct {
		Href       string
		MediaType  string
		Properties string
	})
	legacyCover := ""
	navigationItems := 0
	for _, meta := range inspection.packageDoc.Metadata.Metas {
		if strings.EqualFold(meta.Name, "cover") {
			legacyCover = meta.Content
		}
	}
	for _, item := range inspection.packageDoc.Manifest.Items {
		if item.ID == "" || item.Href == "" || item.MediaType == "" {
			return fail(errors.New("EPUB manifest item is missing id, href, or media-type"))
		}
		if _, exists := inspection.manifestByID[item.ID]; exists {
			return fail(fmt.Errorf("EPUB manifest contains duplicate id %s", item.ID))
		}
		inspection.manifestByID[item.ID] = struct {
			Href       string
			MediaType  string
			Properties string
		}{item.Href, item.MediaType, item.Properties}
		if hasToken(item.Properties, "nav") {
			navigationItems++
			if item.MediaType != "application/xhtml+xml" {
				return fail(fmt.Errorf("EPUB navigation item %s is not XHTML", item.ID))
			}
		}
		if hasToken(item.Properties, "cover-image") || item.ID == legacyCover {
			if inspection.coverID != "" {
				return fail(errors.New("EPUB package declares more than one cover image"))
			}
			inspection.coverID, inspection.coverHref = item.ID, item.Href
		}
	}
	if navigationItems != 1 {
		return fail(fmt.Errorf("EPUB package declares %d navigation documents; exactly one is required", navigationItems))
	}
	if inspection.coverHref != "" {
		inspection.coverPath, err = resolveEPUBReference(inspection.packagePath, inspection.coverHref)
		if err != nil {
			return fail(err)
		}
		if inspection.files[inspection.coverPath] == nil {
			return fail(fmt.Errorf("EPUB cover targets missing file %s", inspection.coverPath))
		}
	}
	if len(inspection.packageDoc.Spine.Items) != 0 {
		first := inspection.manifestByID[inspection.packageDoc.Spine.Items[0].IDRef]
		if first.Href == "" {
			return fail(errors.New("EPUB first spine item does not resolve through the manifest"))
		}
		inspection.firstSpineDoc, err = resolveEPUBReference(inspection.packagePath, first.Href)
		if err != nil {
			return fail(err)
		}
	}
	return inspection, nil
}

func epubDocumentReferences(files map[string]*zip.File, documentPath, targetPath string) (bool, error) {
	entry := files[documentPath]
	if entry == nil {
		return false, fmt.Errorf("EPUB spine document is missing: %s", documentPath)
	}
	data, err := readEPUBEntry(entry)
	if err != nil {
		return false, err
	}
	document, err := inspectEPUBXML(bytes.NewReader(data))
	if err != nil {
		return false, fmt.Errorf("decode EPUB spine document %s: %w", documentPath, err)
	}
	for _, raw := range document.hrefs {
		resolved, err := resolveEPUBReference(documentPath, raw)
		if err == nil && resolved == targetPath {
			return true, nil
		}
	}
	return false, nil
}

type epubBytePatch struct {
	start int
	end   int
	data  []byte
}

func updateEPUBCoverMetadata(data []byte, selectedID, newManifestItem string) ([]byte, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	inManifest := false
	manifestEnd := -1
	selectedFound := false
	var patches []epubBytePatch
	for {
		before := int(decoder.InputOffset())
		token, err := decoder.Token()
		after := int(decoder.InputOffset())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("scan EPUB package cover metadata: %w", err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			if value.Name.Local == "manifest" {
				inManifest = true
				continue
			}
			attributes := make(map[string]string, len(value.Attr))
			for _, attribute := range value.Attr {
				attributes[attribute.Name.Local] = attribute.Value
			}
			if inManifest && value.Name.Local == "item" {
				id := attributes["id"]
				properties := strings.Fields(attributes["properties"])
				filtered := properties[:0]
				for _, property := range properties {
					if property != "cover-image" {
						filtered = append(filtered, property)
					}
				}
				if id == selectedID {
					selectedFound = true
					filtered = append(filtered, "cover-image")
				}
				updated, updateErr := updateXMLStartTagAttribute(data[before:after], "properties", strings.Join(filtered, " "), true)
				if updateErr != nil {
					return nil, updateErr
				}
				if !bytes.Equal(updated, data[before:after]) {
					patches = append(patches, epubBytePatch{start: before, end: after, data: updated})
				}
			}
			if value.Name.Local == "meta" && strings.EqualFold(attributes["name"], "cover") {
				updated, updateErr := updateXMLStartTagAttribute(data[before:after], "content", selectedID, false)
				if updateErr != nil {
					return nil, updateErr
				}
				if !bytes.Equal(updated, data[before:after]) {
					patches = append(patches, epubBytePatch{start: before, end: after, data: updated})
				}
			}
		case xml.EndElement:
			if value.Name.Local == "manifest" {
				manifestEnd = before
				inManifest = false
			}
		}
	}
	if manifestEnd < 0 {
		return nil, errors.New("EPUB package has no manifest end element")
	}
	if newManifestItem == "" && !selectedFound {
		return nil, fmt.Errorf("selected EPUB cover manifest item is missing: %s", selectedID)
	}
	if newManifestItem != "" {
		patches = append(patches, epubBytePatch{start: manifestEnd, end: manifestEnd, data: []byte(newManifestItem)})
	}
	output := append([]byte(nil), data...)
	for index := len(patches) - 1; index >= 0; index-- {
		patch := patches[index]
		updated := make([]byte, 0, len(output)-(patch.end-patch.start)+len(patch.data))
		updated = append(updated, output[:patch.start]...)
		updated = append(updated, patch.data...)
		updated = append(updated, output[patch.end:]...)
		output = updated
	}
	return output, nil
}

func removeEPUBManifestItems(data []byte, removed map[string]bool) ([]byte, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	inManifest := false
	activeStart, activeDepth := -1, 0
	var patches []epubBytePatch
	for {
		before := int(decoder.InputOffset())
		token, err := decoder.Token()
		after := int(decoder.InputOffset())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("scan EPUB manifest removals: %w", err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			if value.Name.Local == "manifest" {
				inManifest = true
				continue
			}
			if activeDepth != 0 {
				activeDepth++
			}
			if inManifest && value.Name.Local == "item" {
				id := ""
				for _, attribute := range value.Attr {
					if attribute.Name.Local == "id" {
						id = attribute.Value
					}
				}
				if removed[id] {
					activeStart = before
					activeDepth = 1
				}
			}
		case xml.EndElement:
			if activeDepth != 0 {
				activeDepth--
				if activeDepth == 0 && activeStart >= 0 {
					patches = append(patches, epubBytePatch{start: activeStart, end: after})
					activeStart = -1
				}
			}
			if value.Name.Local == "manifest" {
				inManifest = false
			}
		}
	}
	output := append([]byte(nil), data...)
	for index := len(patches) - 1; index >= 0; index-- {
		patch := patches[index]
		output = append(append([]byte(nil), output[:patch.start]...), output[patch.end:]...)
	}
	return output, nil
}

func updateXMLStartTagAttribute(tag []byte, name, value string, removeEmpty bool) ([]byte, error) {
	fullStart, fullEnd, valueStart, valueEnd, found, err := xmlAttributeRange(tag, name)
	if err != nil {
		return nil, err
	}
	if found {
		if removeEmpty && value == "" {
			return append(append([]byte(nil), tag[:fullStart]...), tag[fullEnd:]...), nil
		}
		output := make([]byte, 0, len(tag)-(valueEnd-valueStart)+len(value))
		output = append(output, tag[:valueStart]...)
		output = append(output, escapeXML(value)...)
		output = append(output, tag[valueEnd:]...)
		return output, nil
	}
	if removeEmpty && value == "" {
		return append([]byte(nil), tag...), nil
	}
	insert := bytes.LastIndexByte(tag, '>')
	if insert < 0 {
		return nil, errors.New("XML start tag has no closing delimiter")
	}
	if insert > 0 && tag[insert-1] == '/' {
		insert--
	}
	attribute := []byte(` ` + name + `="` + escapeXML(value) + `"`)
	output := make([]byte, 0, len(tag)+len(attribute))
	output = append(output, tag[:insert]...)
	output = append(output, attribute...)
	output = append(output, tag[insert:]...)
	return output, nil
}

func xmlAttributeRange(tag []byte, target string) (fullStart, fullEnd, valueStart, valueEnd int, found bool, err error) {
	i := 0
	for i < len(tag) && tag[i] != '<' {
		i++
	}
	if i == len(tag) {
		return 0, 0, 0, 0, false, errors.New("invalid XML start tag")
	}
	i++
	for i < len(tag) && !isXMLSpace(tag[i]) && tag[i] != '>' && tag[i] != '/' {
		i++
	}
	for i < len(tag) {
		spaceStart := i
		for i < len(tag) && isXMLSpace(tag[i]) {
			i++
		}
		if i >= len(tag) || tag[i] == '>' || tag[i] == '/' {
			return 0, 0, 0, 0, false, nil
		}
		nameStart := i
		for i < len(tag) && !isXMLSpace(tag[i]) && tag[i] != '=' && tag[i] != '>' && tag[i] != '/' {
			i++
		}
		attributeName := string(tag[nameStart:i])
		if separator := strings.LastIndexByte(attributeName, ':'); separator >= 0 {
			attributeName = attributeName[separator+1:]
		}
		for i < len(tag) && isXMLSpace(tag[i]) {
			i++
		}
		if i >= len(tag) || tag[i] != '=' {
			return 0, 0, 0, 0, false, errors.New("malformed XML attribute")
		}
		i++
		for i < len(tag) && isXMLSpace(tag[i]) {
			i++
		}
		if i >= len(tag) || (tag[i] != '\'' && tag[i] != '"') {
			return 0, 0, 0, 0, false, errors.New("malformed quoted XML attribute")
		}
		quote := tag[i]
		i++
		start := i
		for i < len(tag) && tag[i] != quote {
			i++
		}
		if i >= len(tag) {
			return 0, 0, 0, 0, false, errors.New("unterminated XML attribute")
		}
		end := i
		i++
		if attributeName == target {
			return spaceStart, i, start, end, true, nil
		}
	}
	return 0, 0, 0, 0, false, nil
}

func isXMLSpace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\n'
}

func moveEPUBSpineItemFirst(data []byte, selectedID string) ([]byte, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	inSpine := false
	spineStart := -1
	firstID := ""
	targetStart, targetEnd := -1, -1
	targetDepth := 0
	for {
		before := int(decoder.InputOffset())
		token, err := decoder.Token()
		after := int(decoder.InputOffset())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("scan EPUB spine: %w", err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			if value.Name.Local == "spine" {
				inSpine = true
				spineStart = after
				continue
			}
			if targetDepth != 0 {
				targetDepth++
			}
			if inSpine && value.Name.Local == "itemref" {
				idref := ""
				for _, attribute := range value.Attr {
					if attribute.Name.Local == "idref" {
						idref = attribute.Value
					}
				}
				if firstID == "" {
					firstID = idref
				}
				if idref == selectedID {
					targetStart = before
					targetDepth = 1
				}
			}
		case xml.EndElement:
			if targetDepth != 0 {
				targetDepth--
				if targetDepth == 0 && targetStart >= 0 {
					targetEnd = after
				}
			}
			if value.Name.Local == "spine" {
				inSpine = false
			}
		}
	}
	if firstID == selectedID {
		return append([]byte(nil), data...), nil
	}
	if spineStart < 0 || targetStart < spineStart || targetEnd < targetStart {
		return nil, fmt.Errorf("EPUB cover document %s is not in the spine", selectedID)
	}
	item := append([]byte(nil), data[targetStart:targetEnd]...)
	output := make([]byte, 0, len(data))
	output = append(output, data[:spineStart]...)
	output = append(output, item...)
	output = append(output, data[spineStart:targetStart]...)
	output = append(output, data[targetEnd:]...)
	return output, nil
}

func resolveEPUBReference(source, raw string) (string, error) {
	reference, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("invalid EPUB reference %q: %w", raw, err)
	}
	if reference.Scheme != "" || reference.Host != "" {
		return "", fmt.Errorf("EPUB reference %q is not local", raw)
	}
	decoded, err := url.PathUnescape(reference.Path)
	if err != nil {
		return "", fmt.Errorf("invalid EPUB path escape in %q: %w", raw, err)
	}
	result := source
	if decoded != "" {
		result = pathpkg.Clean(pathpkg.Join(pathpkg.Dir(source), decoded))
	}
	if err := validateEPUBEntryName(result); err != nil {
		return "", err
	}
	return result, nil
}

func validateEPUBEntryName(name string) error {
	if name == "" || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") ||
		pathpkg.Clean(name) != name || name == "." || strings.HasPrefix(name, "../") {
		return fmt.Errorf("EPUB contains unsafe entry path %q", name)
	}
	return nil
}

func readEPUBEntry(entry *zip.File) ([]byte, error) {
	if entry.UncompressedSize64 > 1<<30 {
		return nil, fmt.Errorf("EPUB entry %s exceeds the 1 GiB safety limit", entry.Name)
	}
	reader, err := entry.Open()
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, int64(entry.UncompressedSize64)+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if uint64(len(data)) != entry.UncompressedSize64 {
		return nil, fmt.Errorf("EPUB entry %s has an inconsistent uncompressed size", entry.Name)
	}
	return data, nil
}

func hasToken(value, token string) bool {
	for _, field := range strings.Fields(value) {
		if field == token {
			return true
		}
	}
	return false
}

func uniqueEPUBName(files map[string]*zip.File, preferred string) string {
	if files[preferred] == nil {
		return preferred
	}
	extension := pathpkg.Ext(preferred)
	base := strings.TrimSuffix(preferred, extension)
	for index := 2; ; index++ {
		candidate := base + "-" + strconv.Itoa(index) + extension
		if files[candidate] == nil {
			return candidate
		}
	}
}

func uniqueEPUBID(document epubPackageDocument, preferred string) string {
	used := make(map[string]bool)
	for _, item := range document.Manifest.Items {
		used[item.ID] = true
	}
	if !used[preferred] {
		return preferred
	}
	for index := 2; ; index++ {
		candidate := preferred + "-" + strconv.Itoa(index)
		if !used[candidate] {
			return candidate
		}
	}
}

func relativeEPUBPath(base, target string) (string, error) {
	baseFS := filepath.FromSlash(base)
	targetFS := filepath.FromSlash(target)
	relative, err := filepath.Rel(baseFS, targetFS)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(relative), nil
}

func firstNonempty(values []string, fallback string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return fallback
}
