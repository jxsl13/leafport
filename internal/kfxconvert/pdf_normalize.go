package kfxconvert

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

// PDFNormalizationResult reports compatibility-only state removed from a PDF.
// Page content, metadata, outlines, and safe GoTo/HTTP(S)/mailto links remain.
type PDFNormalizationResult struct {
	Pages               int
	SignaturesRemoved   bool
	FormsRemoved        bool
	AttachmentsRemoved  bool
	ActionsRemoved      int
	AnnotationsRemoved  int
	ThumbnailsRemoved   int
	FontMetricsRepaired int
}

// NormalizePDF writes a conservative PDF 1.7 representation suitable for
// resource-constrained readers. The destination is created exclusively.
func NormalizePDF(source, destination string) (result PDFNormalizationResult, err error) {
	input, err := os.Open(source)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, input.Close()) }()
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
	result, err = normalizePDF(input, output)
	if err != nil {
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
	if err = validateKindlePDF(destination); err != nil {
		return result, err
	}
	complete = true
	return result, nil
}

func normalizePDFFile(path string) (result PDFNormalizationResult, err error) {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".leafport-pdf-normalize-*.pdf")
	if err != nil {
		return result, err
	}
	temporaryPath := temporary.Name()
	if chmodErr := temporary.Chmod(0o600); chmodErr != nil {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
		return result, chmodErr
	}
	complete := false
	defer func() {
		if temporary != nil {
			err = errors.Join(err, temporary.Close())
		}
		if !complete {
			err = errors.Join(err, os.Remove(temporaryPath))
		}
	}()
	input, err := os.Open(path)
	if err != nil {
		return result, err
	}
	result, normalizeErr := normalizePDF(input, temporary)
	closeInputErr := input.Close()
	if normalizeErr != nil || closeInputErr != nil {
		return result, errors.Join(normalizeErr, closeInputErr)
	}
	if err = temporary.Sync(); err != nil {
		return result, err
	}
	if err = temporary.Close(); err != nil {
		temporary = nil
		return result, err
	}
	temporary = nil
	if err = validateKindlePDF(temporaryPath); err != nil {
		return result, err
	}
	if err = os.Rename(temporaryPath, path); err != nil {
		return result, err
	}
	complete = true
	return result, nil
}

func normalizePDF(input io.ReadSeeker, output io.Writer) (PDFNormalizationResult, error) {
	configuration := pdfCompatibilityConfiguration()
	configuration.ValidationMode = model.ValidationRelaxed
	configuration.Cmd = model.OPTIMIZE
	context, err := api.ReadValidateAndOptimize(input, configuration)
	if err != nil {
		return PDFNormalizationResult{}, fmt.Errorf("read PDF for compatibility normalization: %w", err)
	}
	result := PDFNormalizationResult{
		Pages: context.PageCount, SignaturesRemoved: context.SignatureExist || context.AppendOnly,
		FormsRemoved: context.Form != nil,
	}
	root, err := context.Catalog()
	if err != nil {
		return result, fmt.Errorf("read PDF catalog: %w", err)
	}
	if _, present := root["AcroForm"]; present {
		result.FormsRemoved = true
	}
	if err := context.RemoveAllSignatures(); err != nil {
		return result, fmt.Errorf("remove PDF signatures: %w", err)
	}
	delete(root, "AcroForm")
	context.Form = nil
	context.SignatureExist = false
	context.AppendOnly = false

	names, err := context.DereferenceDict(root["Names"])
	if err != nil {
		return result, fmt.Errorf("read PDF name tree: %w", err)
	}
	if names != nil {
		if _, present := names["EmbeddedFiles"]; present {
			removed, removeErr := context.RemoveAttachments(nil)
			if removeErr != nil {
				return result, fmt.Errorf("remove PDF attachments: %w", removeErr)
			}
			result.AttachmentsRemoved = removed
		}
		for _, key := range []string{"JavaScript", "EmbeddedFiles"} {
			if _, present := names[key]; present {
				delete(names, key)
				result.ActionsRemoved++
			}
		}
		if len(names) == 0 {
			delete(root, "Names")
		}
	}
	for _, key := range []string{
		"OpenAction", "AA", "Perms", "DSS", "Legal", "Collection", "NeedsRendering", "Requirements", "AF",
	} {
		if _, present := root[key]; present {
			delete(root, key)
			result.ActionsRemoved++
		}
	}
	result.FontMetricsRepaired, err = repairPDFFontMetrics(context)
	if err != nil {
		return result, err
	}

	for pageNumber := 1; pageNumber <= context.PageCount; pageNumber++ {
		page, _, _, pageErr := context.PageDict(pageNumber, false)
		if pageErr != nil {
			return result, fmt.Errorf("read PDF page %d: %w", pageNumber, pageErr)
		}
		for _, key := range []string{"AA", "Dur", "Trans", "PresSteps"} {
			if _, present := page[key]; present {
				delete(page, key)
				result.ActionsRemoved++
			}
		}
		if _, present := page["Thumb"]; present {
			delete(page, "Thumb")
			result.ThumbnailsRemoved++
		}
		annotations, annotationErr := context.DereferenceArray(page["Annots"])
		if annotationErr != nil {
			return result, fmt.Errorf("read PDF page %d annotations: %w", pageNumber, annotationErr)
		}
		if len(annotations) == 0 {
			delete(page, "Annots")
			continue
		}
		kept := make(types.Array, 0, len(annotations))
		for _, object := range annotations {
			annotation, dereferenceErr := context.DereferenceDict(object)
			if dereferenceErr != nil {
				return result, fmt.Errorf("read PDF page %d annotation: %w", pageNumber, dereferenceErr)
			}
			if annotation == nil || annotation.NameEntry("Subtype") == nil ||
				*annotation.NameEntry("Subtype") != "Link" {
				result.AnnotationsRemoved++
				continue
			}
			if _, present := annotation["AA"]; present {
				delete(annotation, "AA")
				result.ActionsRemoved++
			}
			if !safePDFLink(context, annotation) {
				result.AnnotationsRemoved++
				continue
			}
			kept = append(kept, object)
		}
		if len(kept) == 0 {
			delete(page, "Annots")
		} else {
			page["Annots"] = kept
		}
	}
	version := model.V17
	context.HeaderVersion = &version
	context.RootVersion = nil
	if err := api.WriteContext(context, output); err != nil {
		return result, fmt.Errorf("write normalized PDF: %w", err)
	}
	return result, nil
}

// repairPDFFontMetrics replaces a non-positive ascent with the descriptor's
// own positive FontBBox top. It does not inspect or rewrite embedded glyph
// programs, widths, encodings, or page content.
func repairPDFFontMetrics(context *model.Context) (int, error) {
	repaired := 0
	for objectNumber, entry := range context.Table {
		if entry == nil || entry.Free {
			continue
		}
		dict := pdfObjectDict(entry.Object)
		if dict == nil || dict.NameEntry("Type") == nil || *dict.NameEntry("Type") != "FontDescriptor" {
			continue
		}
		bbox := dict.ArrayEntry("FontBBox")
		if len(bbox) != 4 {
			continue
		}
		topObject, err := context.Dereference(bbox[3])
		if err != nil {
			return repaired, fmt.Errorf("read PDF font descriptor %d FontBBox: %w", objectNumber, err)
		}
		top, ok := pdfNumericValue(topObject)
		if !ok || top <= 0 {
			continue
		}
		ascent, err := dereferencedPDFNumber(context, dict["Ascent"])
		if err != nil {
			return repaired, fmt.Errorf("read PDF font descriptor %d ascent: %w", objectNumber, err)
		}
		changed := false
		if ascent <= 0 {
			dict["Ascent"] = topObject
			changed = true
		}
		if _, present := dict["CapHeight"]; present {
			capHeight, dereferenceErr := dereferencedPDFNumber(context, dict["CapHeight"])
			if dereferenceErr != nil {
				return repaired, fmt.Errorf("read PDF font descriptor %d cap height: %w", objectNumber, dereferenceErr)
			}
			if capHeight <= 0 {
				dict["CapHeight"] = topObject
				changed = true
			}
		}
		if changed {
			repaired++
		}
	}
	return repaired, nil
}

func pdfObjectDict(object types.Object) types.Dict {
	switch value := object.(type) {
	case types.Dict:
		return value
	case types.StreamDict:
		return value.Dict
	default:
		return nil
	}
}

func dereferencedPDFNumber(context *model.Context, object types.Object) (float64, error) {
	if object == nil {
		return 0, nil
	}
	value, err := context.Dereference(object)
	if err != nil {
		return 0, err
	}
	number, _ := pdfNumericValue(value)
	return number, nil
}

func pdfNumericValue(object types.Object) (float64, bool) {
	switch value := object.(type) {
	case types.Integer:
		return float64(value), true
	case types.Float:
		return float64(value), true
	default:
		return 0, false
	}
}

func safePDFLink(context *model.Context, annotation types.Dict) bool {
	if _, active := annotation["AA"]; active {
		return false
	}
	if brokenPDFLink(context, annotation) {
		return false
	}
	if _, direct := annotation["Dest"]; direct {
		return true
	}
	action, err := context.DereferenceDict(annotation["A"])
	if err != nil || action == nil || action.NameEntry("S") == nil {
		return false
	}
	switch *action.NameEntry("S") {
	case "GoTo":
		return true
	case "URI":
		raw, err := context.DereferenceText(action["URI"])
		if err != nil {
			return false
		}
		parsed, err := url.Parse(strings.TrimSpace(raw))
		if err != nil {
			return false
		}
		switch strings.ToLower(parsed.Scheme) {
		case "http", "https", "mailto":
			return true
		}
	}
	return false
}

func pdfCompatibilityConfiguration() *model.Configuration {
	configuration := pdfConfiguration()
	configuration.WriteObjectStream = false
	configuration.WriteXRefStream = false
	configuration.PostProcessValidate = true
	return configuration
}

func validateKindlePDF(path string) error {
	configuration := pdfCompatibilityConfiguration()
	configuration.ValidationMode = model.ValidationStrict
	// URI reachability is network state, not PDF conformance. Link syntax and
	// allowed schemes are checked locally below.
	configuration.ValidateLinks = false
	if err := api.ValidateFile(path, configuration); err != nil {
		return fmt.Errorf("strictly validate normalized PDF: %w", err)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	context, readErr := api.ReadValidateAndOptimize(file, configuration)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return fmt.Errorf("inspect normalized PDF: %w", errors.Join(readErr, closeErr))
	}
	if context.PageCount == 0 {
		return errors.New("normalized PDF contains no pages")
	}
	if context.HeaderVersion == nil || *context.HeaderVersion != model.V17 || context.RootVersion != nil {
		return errors.New("normalized PDF is not a strict PDF 1.7 file")
	}
	if context.Read.UsingXRefStreams || context.Read.UsingObjectStreams {
		return errors.New("normalized PDF uses unsupported cross-reference or object streams")
	}
	if context.Encrypt != nil {
		return errors.New("normalized PDF remains encrypted")
	}
	root, err := context.Catalog()
	if err != nil {
		return err
	}
	for _, key := range []string{
		"AcroForm", "OpenAction", "AA", "Perms", "DSS", "Legal", "Collection", "NeedsRendering", "Requirements", "AF",
	} {
		if _, present := root[key]; present {
			return fmt.Errorf("normalized PDF retains unsupported catalog entry %s", key)
		}
	}
	names, err := context.DereferenceDict(root["Names"])
	if err != nil {
		return fmt.Errorf("inspect normalized PDF name tree: %w", err)
	}
	for _, key := range []string{"EmbeddedFiles", "JavaScript"} {
		if _, present := names[key]; present {
			return fmt.Errorf("normalized PDF retains unsupported %s name tree", key)
		}
	}
	if context.SignatureExist || context.AppendOnly || context.Form != nil {
		return errors.New("normalized PDF retains interactive form or signature state")
	}
	if err := validatePDFFontMetrics(context); err != nil {
		return err
	}
	for pageNumber := 1; pageNumber <= context.PageCount; pageNumber++ {
		page, _, inherited, pageErr := context.PageDict(pageNumber, false)
		if pageErr != nil {
			return pageErr
		}
		if inherited.MediaBox == nil || inherited.MediaBox.Width() <= 0 || inherited.MediaBox.Height() <= 0 ||
			inherited.MediaBox.Width() > 14400 || inherited.MediaBox.Height() > 14400 {
			return fmt.Errorf("normalized PDF page %d has an unsafe media box", pageNumber)
		}
		if inherited.Rotate%90 != 0 {
			return fmt.Errorf("normalized PDF page %d has unsupported rotation %d", pageNumber, inherited.Rotate)
		}
		for _, key := range []string{"AA", "Dur", "Trans", "PresSteps", "Thumb"} {
			if _, present := page[key]; present {
				return fmt.Errorf("normalized PDF page %d retains unsupported entry %s", pageNumber, key)
			}
		}
		annotations, annotationErr := context.DereferenceArray(page["Annots"])
		if annotationErr != nil {
			return annotationErr
		}
		for _, object := range annotations {
			annotation, dereferenceErr := context.DereferenceDict(object)
			if dereferenceErr != nil || annotation == nil || annotation.NameEntry("Subtype") == nil ||
				*annotation.NameEntry("Subtype") != "Link" || !safePDFLink(context, annotation) {
				return fmt.Errorf("normalized PDF page %d retains an unsupported annotation", pageNumber)
			}
		}
	}
	return nil
}

func validatePDFFontMetrics(context *model.Context) error {
	for objectNumber, entry := range context.Table {
		if entry == nil || entry.Free {
			continue
		}
		dict := pdfObjectDict(entry.Object)
		if dict == nil || dict.NameEntry("Type") == nil || *dict.NameEntry("Type") != "FontDescriptor" {
			continue
		}
		bbox := dict.ArrayEntry("FontBBox")
		if len(bbox) != 4 {
			continue
		}
		topObject, err := context.Dereference(bbox[3])
		if err != nil {
			return fmt.Errorf("validate PDF font descriptor %d FontBBox: %w", objectNumber, err)
		}
		top, ok := pdfNumericValue(topObject)
		if !ok || top <= 0 {
			continue
		}
		ascent, err := dereferencedPDFNumber(context, dict["Ascent"])
		if err != nil {
			return fmt.Errorf("validate PDF font descriptor %d ascent: %w", objectNumber, err)
		}
		if ascent <= 0 {
			return fmt.Errorf("normalized PDF font descriptor %d has non-positive ascent %.2f", objectNumber, ascent)
		}
		if _, present := dict["CapHeight"]; present {
			capHeight, dereferenceErr := dereferencedPDFNumber(context, dict["CapHeight"])
			if dereferenceErr != nil {
				return fmt.Errorf("validate PDF font descriptor %d cap height: %w", objectNumber, dereferenceErr)
			}
			if capHeight <= 0 {
				return fmt.Errorf("normalized PDF font descriptor %d has non-positive cap height %.2f", objectNumber, capHeight)
			}
		}
	}
	return nil
}
