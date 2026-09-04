package kfxconvert

import (
	"bytes"
	"fmt"
)

// ConversionResult describes Leafport's final publication file.
type ConversionResult struct {
	Path               string
	Format             string
	Pages              int
	Sections           int
	Images             int
	Media              int
	Fonts              int
	Resources          int
	ExactCopy          bool
	BrokenLinksRemoved int
	Privacy            PrivacyResult
}

// ConversionOptions controls optional transformations applied while
// reconstructing the publication.
type ConversionOptions struct {
	Privacy PrivacyOptions
	// Format selects the final publication format. Empty and "auto" retain
	// Leafport's semantic format selection. Supported explicit values are
	// "pdf", "epub", "cbz", and "comic-epub".
	Format string
}

// FormatDecision explains the non-KFX output selected for a publication.
type FormatDecision struct {
	Format string
	Reason string
}

// PreferredFormat determines the closest supported final format without
// writing output.
func PreferredFormat(archivePath string) (FormatDecision, error) {
	book, err := loadBook(archivePath)
	if err != nil {
		return FormatDecision{}, err
	}
	pages, err := book.fixedLayoutPages()
	if err != nil {
		return FormatDecision{}, err
	}
	return chooseFormat(book, pages), nil
}

// Convert reconstructs the closest broadly readable publication format. A
// PDF-backed fixed-layout book remains PDF; other KFX publications become
// EPUB so that text and image reading order are retained.
func Convert(archivePath, destinationBase string, metadata Metadata) (ConversionResult, error) {
	return ConvertWithOptions(archivePath, destinationBase, metadata, ConversionOptions{})
}

// ConvertWithOptions reconstructs a publication with explicit conversion
// options.
func ConvertWithOptions(archivePath, destinationBase string, metadata Metadata, options ConversionOptions) (ConversionResult, error) {
	book, err := loadBook(archivePath)
	if err != nil {
		return ConversionResult{}, err
	}
	return convertBook(book, destinationBase, metadata, options)
}

// ConvertBytes performs the same conversion while keeping the decrypted KFX
// archive in memory.
func ConvertBytes(archive []byte, destinationBase string, metadata Metadata) (ConversionResult, error) {
	return ConvertBytesWithOptions(archive, destinationBase, metadata, ConversionOptions{})
}

// ConvertBytesWithOptions performs an option-controlled conversion while
// keeping the decrypted KFX archive in memory.
func ConvertBytesWithOptions(archive []byte, destinationBase string, metadata Metadata, options ConversionOptions) (ConversionResult, error) {
	book, err := loadBookBytes(archive)
	if err != nil {
		return ConversionResult{}, err
	}
	return convertBook(book, destinationBase, metadata, options)
}

func convertBook(book *decodedBook, destinationBase string, metadata Metadata, options ConversionOptions) (ConversionResult, error) {
	privacy, err := applyPrivacy(book, options.Privacy)
	if err != nil {
		return ConversionResult{}, fmt.Errorf("configure privacy cleanup: %w", err)
	}
	pages, err := book.fixedLayoutPages()
	if err != nil {
		return ConversionResult{}, err
	}
	decision, err := requestedFormat(book, pages, options.Format)
	if err != nil {
		return ConversionResult{}, err
	}
	if decision.Format == "PDF" {
		path := destinationBase + ".pdf"
		result, convertErr := convertFixedLayoutPagesToPDF(book, pages, path, book.publicationMetadata(metadata))
		if convertErr != nil {
			return ConversionResult{}, fmt.Errorf("reconstruct PDF: %w", convertErr)
		}
		return ConversionResult{
			Path: path, Format: "PDF", Pages: result.Pages, Resources: result.Resources,
			ExactCopy: result.ExactCopy, BrokenLinksRemoved: result.BrokenLinksRemoved, Privacy: privacy,
		}, nil
	}
	if decision.Format == "CBZ" {
		path := destinationBase + ".cbz"
		result, convertErr := convertImageLayoutPagesToCBZ(book, pages, path, book.publicationMetadata(metadata))
		if convertErr != nil {
			return ConversionResult{}, fmt.Errorf("reconstruct CBZ: %w", convertErr)
		}
		return ConversionResult{
			Path: path, Format: "CBZ", Pages: result.Pages, Images: result.Images, Resources: result.Resources,
			Privacy: privacy,
		}, nil
	}
	if decision.Format == "EPUB-COMIC" || decision.Format == "EPUB-FXL" {
		path := destinationBase + ".epub"
		result, convertErr := convertImageLayoutPagesToEPUB(
			book, pages, path, book.publicationMetadata(metadata), decision.Format == "EPUB-COMIC")
		if convertErr != nil {
			return ConversionResult{}, fmt.Errorf("reconstruct fixed-layout EPUB: %w", convertErr)
		}
		return ConversionResult{
			Path: path, Format: decision.Format, Pages: result.Pages, Images: result.Images,
			Sections: result.Pages, Privacy: privacy,
		}, nil
	}
	path := destinationBase + ".epub"
	result, err := convertBookToEPUB(book, path, metadata)
	if err != nil {
		return ConversionResult{}, fmt.Errorf("reconstruct EPUB: %w", err)
	}
	return ConversionResult{
		Path: path, Format: "EPUB", Sections: result.Sections, Images: result.Images, Media: result.Media, Fonts: result.Fonts,
		Privacy: privacy,
	}, nil
}

func requestedFormat(book *decodedBook, pages []Page, requested string) (FormatDecision, error) {
	switch requested {
	case "", "auto":
		return chooseFormat(book, pages), nil
	case "pdf":
		if len(pages) == 0 {
			return FormatDecision{}, fmt.Errorf("PDF output requires an image-backed fixed-layout publication")
		}
		return FormatDecision{Format: "PDF", Reason: "explicitly requested"}, nil
	case "cbz":
		if len(pages) == 0 {
			return FormatDecision{}, fmt.Errorf("CBZ output requires an image-backed fixed-layout publication")
		}
		return FormatDecision{Format: "CBZ", Reason: "explicitly requested"}, nil
	case "comic-epub":
		if len(pages) == 0 {
			return FormatDecision{}, fmt.Errorf("comic EPUB output requires an image-backed fixed-layout publication")
		}
		return FormatDecision{Format: "EPUB-COMIC", Reason: "explicitly requested"}, nil
	case "epub":
		if len(pages) != 0 {
			return FormatDecision{Format: "EPUB-FXL", Reason: "explicitly requested for a fixed-layout publication"}, nil
		}
		return FormatDecision{Format: "EPUB", Reason: "explicitly requested"}, nil
	default:
		return FormatDecision{}, fmt.Errorf("unsupported output format %q", requested)
	}
}

func chooseFormat(book *decodedBook, pages []Page) FormatDecision {
	if len(pages) == 0 {
		return FormatDecision{Format: "EPUB", Reason: "the publication is reflowable or contains rendered text"}
	}
	allPDF := len(pages) != 0
	for _, page := range pages {
		if page.Format != kfxPDFFormat || !bytes.HasPrefix(page.Data, []byte("%PDF-")) {
			allPDF = false
			break
		}
	}
	if allPDF {
		return FormatDecision{Format: "PDF", Reason: "the reading order is entirely backed by embedded PDF pages"}
	}
	allImages := true
	for _, page := range pages {
		_, _, supported := cbzImageType(page.Data)
		if !supported {
			allImages = false
			break
		}
	}
	if allImages && book != nil && book.isComic() {
		return FormatDecision{Format: "CBZ", Reason: "the publication is explicitly marked as an image-based comic"}
	}
	return FormatDecision{Format: "PDF", Reason: "the publication is image-based fixed layout"}
}
