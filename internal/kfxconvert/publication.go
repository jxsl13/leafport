package kfxconvert

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/pdfcpu/pdfcpu/pkg/api"
)

// PublicationValidationResult is a format-neutral validation summary.
type PublicationValidationResult struct {
	Format      string
	Entries     int
	Pages       int
	SpineItems  int
	Images      int
	CoverFirst  bool
	CoverWidth  int
	CoverHeight int
	HasMetadata bool
}

// ValidatePublication applies Leafport's offline compatibility checks to an
// existing PDF, EPUB, or CBZ without modifying it.
func ValidatePublication(path string) (PublicationValidationResult, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".pdf":
		if err := validateKindlePDF(path); err != nil {
			return PublicationValidationResult{}, err
		}
		pages, err := api.PageCountFile(path)
		if err != nil {
			return PublicationValidationResult{}, err
		}
		return PublicationValidationResult{Format: "PDF", Pages: pages}, nil
	case ".epub":
		result, err := ValidateEPUBFile(path)
		if err != nil {
			return PublicationValidationResult{}, err
		}
		return PublicationValidationResult{
			Format: "EPUB", Entries: result.Entries, SpineItems: result.SpineItems,
			Images: result.Images, CoverFirst: result.CoverFirst,
			CoverWidth: result.CoverWidth, CoverHeight: result.CoverHeight,
		}, nil
	case ".cbz":
		result, err := ValidateCBZFile(path)
		if err != nil {
			return PublicationValidationResult{}, err
		}
		return PublicationValidationResult{
			Format: "CBZ", Entries: result.Entries, Pages: result.Images,
			Images: result.Images, HasMetadata: result.ComicInfo,
		}, nil
	default:
		return PublicationValidationResult{}, fmt.Errorf("unsupported publication extension %q (supported: .pdf, .epub, .cbz)", filepath.Ext(path))
	}
}

// FixPublication creates a normalized copy and never overwrites either path.
func FixPublication(source, destination string) (PublicationValidationResult, error) {
	if !strings.EqualFold(filepath.Ext(source), filepath.Ext(destination)) {
		return PublicationValidationResult{}, errorsNewMatchingExtension(source, destination)
	}
	sourceAbsolute, err := filepath.Abs(source)
	if err != nil {
		return PublicationValidationResult{}, err
	}
	destinationAbsolute, err := filepath.Abs(destination)
	if err != nil {
		return PublicationValidationResult{}, err
	}
	if sourceAbsolute == destinationAbsolute {
		return PublicationValidationResult{}, fmt.Errorf("source and destination must be different; originals are never modified")
	}
	switch strings.ToLower(filepath.Ext(source)) {
	case ".pdf":
		if _, err := NormalizePDF(source, destination); err != nil {
			return PublicationValidationResult{}, err
		}
	case ".epub":
		if _, err := NormalizeEPUB(source, destination); err != nil {
			return PublicationValidationResult{}, err
		}
	case ".cbz":
		if _, err := NormalizeCBZ(source, destination); err != nil {
			return PublicationValidationResult{}, err
		}
	default:
		return PublicationValidationResult{}, fmt.Errorf("unsupported publication extension %q (supported: .pdf, .epub, .cbz)", filepath.Ext(source))
	}
	return ValidatePublication(destination)
}

func errorsNewMatchingExtension(source, destination string) error {
	return fmt.Errorf("source %s and destination %s must have matching PDF, EPUB, or CBZ extensions",
		filepath.Base(source), filepath.Base(destination))
}
