package kfxconvert

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

var disablePDFCPUConfig sync.Once

// PDFResult summarizes a fixed-layout PDF reconstruction.
type PDFResult struct {
	Pages              int
	Resources          int
	ExactCopy          bool
	BrokenLinksRemoved int
}

// ConvertToPDF reconstructs a PDF-backed fixed-layout KFX publication. The
// destination is created exclusively and removed if validation fails.
func ConvertToPDF(archivePath, destination string) (result PDFResult, err error) {
	book, err := loadBook(archivePath)
	if err != nil {
		return result, err
	}
	pages, err := book.fixedLayoutPages()
	if err != nil {
		return result, err
	}
	return convertFixedLayoutPagesToPDF(book, pages, destination, book.publicationMetadata(Metadata{}))
}

func convertFixedLayoutPagesToPDF(book *decodedBook, pages []Page, destination string, metadata Metadata) (result PDFResult, err error) {
	// pdfcpu otherwise initializes a process-global user configuration and font
	// directory. Leafport only needs core PDF assembly and must leave the user's
	// home directory untouched.
	disablePDFCPUConfig.Do(api.DisableConfigDir)
	if len(pages) == 0 {
		return result, errors.New("KFX publication is not image-backed fixed layout")
	}
	pages, _ = fixedLayoutPagesWithCover(book, pages)
	allPDF, allImages := true, true
	for index, page := range pages {
		isPDF := page.Format == kfxPDFFormat && bytes.HasPrefix(page.Data, []byte("%PDF-"))
		allPDF = allPDF && isPDF
		allImages = allImages && !isPDF
		if isPDF && page.PageIndex < 0 {
			return result, fmt.Errorf("fixed-layout page %d has negative PDF page index", index+1)
		}
	}
	if allImages {
		return convertImageLayoutPagesToPDF(book, pages, destination, metadata)
	}
	if !allPDF {
		return convertMixedLayoutPagesToPDF(book, pages, destination, metadata)
	}

	groups := groupPDFPages(pages)
	result.Pages = len(pages)
	result.Resources = len(groups)
	exactSource := len(groups) == 1 && (book == nil || book.privacy == nil)
	segments := make([][]byte, 0, len(groups))
	readers := make([]io.ReadSeeker, 0, len(groups))
	for _, group := range groups {
		pageCount, countErr := api.PageCount(bytes.NewReader(group.data), pdfConfiguration())
		if countErr != nil {
			return result, fmt.Errorf("validate embedded PDF %q: %w", group.location, countErr)
		}
		for _, pageIndex := range group.pageIndices {
			if pageIndex >= pageCount {
				return result, fmt.Errorf("embedded PDF %q has %d pages but KFX references page %d",
					group.location, pageCount, pageIndex+1)
			}
		}
		if isCompletePDF(group.pageIndices, pageCount) {
			readers = append(readers, bytes.NewReader(group.data))
			continue
		}
		exactSource = false
		var selected bytes.Buffer
		if collectErr := api.Collect(bytes.NewReader(group.data), &selected,
			[]string{pdfPageSelection(group.pageIndices)}, pdfConfiguration()); collectErr != nil {
			return result, fmt.Errorf("select pages from embedded PDF %q: %w", group.location, collectErr)
		}
		segments = append(segments, selected.Bytes())
		readers = append(readers, bytes.NewReader(segments[len(segments)-1]))
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
	if exactSource {
		if _, err = file.Write(groups[0].data); err != nil {
			return result, err
		}
		result.ExactCopy = true
	} else if err = api.MergeRaw(readers, file, false, pdfConfiguration()); err != nil {
		return result, fmt.Errorf("merge embedded PDF resources: %w", err)
	}
	if err = file.Sync(); err != nil {
		return result, err
	}
	if err = file.Close(); err != nil {
		file = nil
		return result, err
	}
	file = nil
	removedLinks, cleanupErr := removeBrokenPDFLinks(destination)
	if cleanupErr != nil {
		return result, fmt.Errorf("remove nonfunctional embedded PDF links: %w", cleanupErr)
	}
	if removedLinks != 0 {
		result.BrokenLinksRemoved = removedLinks
		result.ExactCopy = false
	}
	if properties := pdfProperties(metadata); len(properties) != 0 {
		if err = api.AddPropertiesFile(destination, "", properties, pdfConfiguration()); err != nil {
			return result, fmt.Errorf("add PDF metadata: %w", err)
		}
	}
	if book != nil && book.pageProgressionDirection() == "rtl" {
		preferences := model.ViewerPreferences{Direction: model.DirectionFor("R2L")}
		if err = api.SetViewerPreferencesFile(destination, "", preferences, pdfConfiguration()); err != nil {
			return result, fmt.Errorf("set right-to-left PDF reading direction: %w", err)
		}
		result.ExactCopy = false
	}
	if bookmarks := pdfBookmarks(book, pages); len(bookmarks) != 0 {
		if err = writePDFBookmarks(destination, bookmarks); err != nil {
			return result, fmt.Errorf("add PDF navigation: %w", err)
		}
		result.ExactCopy = false
	}
	if links, linkErr := addKFXPDFLinks(book, pages, destination); linkErr != nil {
		return result, linkErr
	} else if links != 0 {
		result.ExactCopy = false
	}
	if _, err = normalizePDFFile(destination); err != nil {
		return result, fmt.Errorf("normalize PDF for Kindle readers: %w", err)
	}
	result.ExactCopy = false
	if bookmarks := pdfBookmarks(book, pages); len(bookmarks) != 0 {
		if err = validatePDFBookmarks(destination, bookmarks); err != nil {
			return result, err
		}
	}
	if err = validateReconstructedPDF(destination, result.Pages); err != nil {
		return result, err
	}
	if err = os.Chmod(destination, 0o600); err != nil {
		return result, fmt.Errorf("protect reconstructed PDF: %w", err)
	}
	complete = true
	return result, nil
}

// removeBrokenPDFLinks removes only link annotations that cannot perform an
// action: missing/invalid local destinations or a missing action entirely.
// Other action types are retained because their validity can depend on an
// external viewer or file. This is especially important for samples whose
// embedded source PDFs contain disabled GoTo actions for unavailable pages.
func removeBrokenPDFLinks(path string) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	context, err := api.ReadAndValidate(file, pdfConfiguration())
	if err != nil {
		_ = file.Close()
		return 0, err
	}
	objectNumbers := make(map[int]bool)
	for pageNumber := 1; pageNumber <= context.PageCount; pageNumber++ {
		page, _, _, pageErr := context.PageDict(pageNumber, false)
		if pageErr != nil {
			_ = file.Close()
			return 0, pageErr
		}
		annotations, annotationErr := context.DereferenceArray(page["Annots"])
		if annotationErr != nil {
			_ = file.Close()
			return 0, annotationErr
		}
		for index, annotationObject := range annotations {
			annotation, dereferenceErr := context.DereferenceDict(annotationObject)
			if dereferenceErr != nil {
				_ = file.Close()
				return 0, dereferenceErr
			}
			if annotation == nil || annotation.NameEntry("Subtype") == nil || *annotation.NameEntry("Subtype") != "Link" ||
				!brokenPDFLink(context, annotation) {
				continue
			}
			indirect, ok := annotationObject.(types.IndirectRef)
			if !ok {
				_ = file.Close()
				return 0, fmt.Errorf("page %d annotation %d is a nonfunctional direct link that cannot be removed selectively", pageNumber, index)
			}
			objectNumbers[indirect.ObjectNumber.Value()] = true
		}
	}
	if err := file.Close(); err != nil {
		return 0, err
	}
	if len(objectNumbers) == 0 {
		return 0, nil
	}
	objects := make([]int, 0, len(objectNumbers))
	for objectNumber := range objectNumbers {
		objects = append(objects, objectNumber)
	}
	slices.Sort(objects)
	if err := api.RemoveAnnotationsFile(path, "", nil, nil, objects, pdfConfiguration(), false); err != nil {
		return 0, err
	}
	return len(objects), nil
}

func brokenPDFLink(context *model.Context, annotation types.Dict) bool {
	if destination, ok := annotation["Dest"]; ok {
		return !pdfDestinationResolves(context, destination)
	}
	action, err := context.DereferenceDict(annotation["A"])
	if err != nil || action == nil || action.NameEntry("S") == nil {
		return true
	}
	switch *action.NameEntry("S") {
	case "GoTo":
		destination, ok := action["D"]
		return !ok || !pdfDestinationResolves(context, destination)
	case "URI":
		uriObject, ok := action["URI"]
		if !ok {
			return true
		}
		uri, uriErr := context.DereferenceText(uriObject)
		return uriErr != nil || strings.TrimSpace(uri) == ""
	default:
		return false
	}
}

func pdfDestinationResolves(context *model.Context, destination types.Object) bool {
	page, err := pdfcpu.PageNrFromDestination(context, destination)
	return err == nil && page >= 1 && page <= context.PageCount
}

func convertImageLayoutPagesToPDF(book *decodedBook, pages []Page, destination string, metadata Metadata) (result PDFResult, err error) {
	readers := make([]io.Reader, 0, len(pages))
	locations := make(map[string]bool, len(pages))
	for index, page := range pages {
		if page.Format == kfxPDFFormat {
			return result, fmt.Errorf("fixed-layout page %d unexpectedly contains PDF data", index+1)
		}
		configuration, format, decodeErr := image.DecodeConfig(bytes.NewReader(page.Data))
		if decodeErr != nil {
			return result, fmt.Errorf("fixed-layout page %d (resource $%d, format $%d, %q) uses an unsupported image encoding: %w",
				index+1, page.ResourceID, page.Format, page.Location, decodeErr)
		}
		if configuration.Width <= 0 || configuration.Height <= 0 {
			return result, fmt.Errorf("fixed-layout page %d (%q) has invalid %s dimensions %dx%d",
				index+1, page.Location, format, configuration.Width, configuration.Height)
		}
		if dimensionErr := validateRasterDimensions(configuration.Width, configuration.Height, page.Location); dimensionErr != nil {
			return result, dimensionErr
		}
		readers = append(readers, bytes.NewReader(page.Data))
		locations[page.Location] = true
	}
	result.Pages = len(pages)
	result.Resources = len(locations)
	// With the full-page import mode pdfcpu uses each image's pixel dimensions
	// as that page's media box, preserving portrait/landscape aspect ratios.
	err = writeReconstructedPDF(book, pages, destination, metadata, func(output io.Writer) error {
		if importErr := api.ImportImages(nil, output, readers, nil, pdfConfiguration()); importErr != nil {
			return fmt.Errorf("import fixed-layout page images: %w", importErr)
		}
		return nil
	})
	return result, err
}

func convertMixedLayoutPagesToPDF(book *decodedBook, pages []Page, destination string, metadata Metadata) (result PDFResult, err error) {
	workDirectory, err := os.MkdirTemp(filepath.Dir(destination), ".leafport-pdf-*")
	if err != nil {
		return result, fmt.Errorf("create private mixed-layout work directory: %w", err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(workDirectory)) }()

	var segmentPaths []string
	for start := 0; start < len(pages); {
		pdfSegment := pages[start].Format == kfxPDFFormat && bytes.HasPrefix(pages[start].Data, []byte("%PDF-"))
		end := start + 1
		for end < len(pages) {
			isPDF := pages[end].Format == kfxPDFFormat && bytes.HasPrefix(pages[end].Data, []byte("%PDF-"))
			if isPDF != pdfSegment {
				break
			}
			end++
		}
		segmentPath := filepath.Join(workDirectory, fmt.Sprintf("segment-%04d.pdf", len(segmentPaths)+1))
		if _, convertErr := convertFixedLayoutPagesToPDF(nil, pages[start:end], segmentPath, Metadata{}); convertErr != nil {
			return result, fmt.Errorf("reconstruct mixed-layout segment %d: %w", len(segmentPaths)+1, convertErr)
		}
		segmentPaths = append(segmentPaths, segmentPath)
		start = end
	}

	readers := make([]io.ReadSeeker, 0, len(segmentPaths))
	files := make([]*os.File, 0, len(segmentPaths))
	defer func() {
		for _, file := range files {
			err = errors.Join(err, file.Close())
		}
	}()
	for _, path := range segmentPaths {
		file, openErr := os.Open(path)
		if openErr != nil {
			return result, openErr
		}
		files = append(files, file)
		readers = append(readers, file)
	}
	locations := make(map[string]bool, len(pages))
	for _, page := range pages {
		locations[page.Location] = true
	}
	result.Pages = len(pages)
	result.Resources = len(locations)
	err = writeReconstructedPDF(book, pages, destination, metadata, func(output io.Writer) error {
		if mergeErr := api.MergeRaw(readers, output, false, pdfConfiguration()); mergeErr != nil {
			return fmt.Errorf("merge mixed-layout segments: %w", mergeErr)
		}
		return nil
	})
	return result, err
}

func writeReconstructedPDF(book *decodedBook, pages []Page, destination string, metadata Metadata, write func(io.Writer) error) (err error) {
	file, err := os.OpenFile(destination, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
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
	if err = write(file); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		file = nil
		return err
	}
	file = nil
	if properties := pdfProperties(metadata); len(properties) != 0 {
		if err = api.AddPropertiesFile(destination, "", properties, pdfConfiguration()); err != nil {
			return fmt.Errorf("add PDF metadata: %w", err)
		}
	}
	if book != nil && book.pageProgressionDirection() == "rtl" {
		preferences := model.ViewerPreferences{Direction: model.DirectionFor("R2L")}
		if err = api.SetViewerPreferencesFile(destination, "", preferences, pdfConfiguration()); err != nil {
			return fmt.Errorf("set right-to-left PDF reading direction: %w", err)
		}
	}
	if bookmarks := pdfBookmarks(book, pages); len(bookmarks) != 0 {
		if err = writePDFBookmarks(destination, bookmarks); err != nil {
			return fmt.Errorf("add PDF navigation: %w", err)
		}
	}
	if _, err = addKFXPDFLinks(book, pages, destination); err != nil {
		return err
	}
	if _, err = normalizePDFFile(destination); err != nil {
		return fmt.Errorf("normalize PDF for Kindle readers: %w", err)
	}
	if bookmarks := pdfBookmarks(book, pages); len(bookmarks) != 0 {
		if err = validatePDFBookmarks(destination, bookmarks); err != nil {
			return err
		}
	}
	if err = validateReconstructedPDF(destination, len(pages)); err != nil {
		return err
	}
	if err = os.Chmod(destination, 0o600); err != nil {
		return fmt.Errorf("protect reconstructed PDF: %w", err)
	}
	complete = true
	return nil
}

func validatePDFBookmarks(path string, expected []pdfcpu.Bookmark) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	actual, readErr := api.Bookmarks(file, pdfConfiguration())
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return fmt.Errorf("validate PDF navigation: %w", errors.Join(readErr, closeErr))
	}
	var compare func([]pdfcpu.Bookmark, []pdfcpu.Bookmark, string) error
	compare = func(want, got []pdfcpu.Bookmark, parent string) error {
		if len(got) != len(want) {
			return fmt.Errorf("validate PDF navigation below %q: got %d entries; expected %d", parent, len(got), len(want))
		}
		for index := range want {
			if got[index].Title != want[index].Title || got[index].PageFrom != want[index].PageFrom {
				return fmt.Errorf("validate PDF navigation entry %d below %q: got %q page %d; expected %q page %d",
					index+1, parent, got[index].Title, got[index].PageFrom, want[index].Title, want[index].PageFrom)
			}
			if err := compare(want[index].Kids, got[index].Kids, want[index].Title); err != nil {
				return err
			}
		}
		return nil
	}
	return compare(expected, actual, "document root")
}

func validateReconstructedPDF(path string, expectedPages int) error {
	if err := api.ValidateFile(path, strictPDFConfiguration()); err != nil {
		return fmt.Errorf("validate reconstructed PDF structure: %w", err)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	pageCount, countErr := api.PageCount(file, pdfConfiguration())
	closeErr := file.Close()
	if countErr != nil || closeErr != nil {
		return fmt.Errorf("validate reconstructed PDF page tree: %w", errors.Join(countErr, closeErr))
	}
	if pageCount != expectedPages {
		return fmt.Errorf("reconstructed PDF has %d pages; expected %d", pageCount, expectedPages)
	}
	return nil
}

func strictPDFConfiguration() *model.Configuration {
	configuration := pdfCompatibilityConfiguration()
	configuration.ValidationMode = model.ValidationStrict
	configuration.ValidateLinks = false
	return configuration
}

func pdfProperties(metadata Metadata) map[string]string {
	properties := make(map[string]string)
	if title := strings.TrimSpace(metadata.Title); title != "" {
		properties["Title"] = title
	}
	if len(metadata.Authors) != 0 {
		var authors []string
		for _, author := range metadata.Authors {
			if author = strings.TrimSpace(author); author != "" {
				authors = append(authors, author)
			}
		}
		if len(authors) != 0 {
			properties["Author"] = strings.Join(authors, ", ")
		}
	}
	return properties
}

func pdfBookmarks(book *decodedBook, pages []Page) []pdfcpu.Bookmark {
	if book == nil || len(pages) == 0 {
		return nil
	}
	sectionIDs := readingOrderSections(book.document)
	if len(sectionIDs) == 0 {
		return nil
	}
	builder := epubBuilder{book: book}
	builder.indexSections(sectionIDs)
	navigation := builder.navigationItems()
	if len(navigation) == 0 {
		return nil
	}
	firstPage := make(map[uint32]int)
	for index, page := range pages {
		if _, exists := firstPage[page.SectionID]; !exists {
			firstPage[page.SectionID] = index + 1
		}
	}
	var convert func([]epubNavigationItem) []pdfcpu.Bookmark
	convert = func(items []epubNavigationItem) []pdfcpu.Bookmark {
		var result []pdfcpu.Bookmark
		for _, item := range items {
			children := convert(item.children)
			if item.section < 1 || item.section > len(sectionIDs) {
				result = append(result, children...)
				continue
			}
			page := firstPage[sectionIDs[item.section-1]]
			if page == 0 {
				result = append(result, children...)
				continue
			}
			result = append(result, pdfcpu.Bookmark{
				Title: item.label, PageFrom: page, Kids: children,
			})
		}
		return result
	}
	return convert(navigation)
}

type pdfPageGroup struct {
	location    string
	data        []byte
	pageIndices []int
}

func groupPDFPages(pages []Page) []pdfPageGroup {
	var groups []pdfPageGroup
	for _, page := range pages {
		if len(groups) == 0 || groups[len(groups)-1].location != page.Location {
			groups = append(groups, pdfPageGroup{location: page.Location, data: page.Data})
		}
		last := &groups[len(groups)-1]
		last.pageIndices = append(last.pageIndices, page.PageIndex)
	}
	return groups
}

func isCompletePDF(indices []int, pageCount int) bool {
	if len(indices) != pageCount {
		return false
	}
	for index, pageIndex := range indices {
		if pageIndex != index {
			return false
		}
	}
	return true
}

func pdfPageSelection(indices []int) string {
	values := make([]string, len(indices))
	for index, pageIndex := range indices {
		values[index] = strconv.Itoa(pageIndex + 1)
	}
	return strings.Join(values, ",")
}

func pdfConfiguration() *model.Configuration {
	disablePDFCPUConfig.Do(api.DisableConfigDir)
	configuration := model.NewDefaultConfiguration()
	configuration.ValidationMode = model.ValidationRelaxed
	configuration.CreateBookmarks = false
	return configuration
}
