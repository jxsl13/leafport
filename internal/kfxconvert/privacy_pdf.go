package kfxconvert

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	pdfform "github.com/pdfcpu/pdfcpu/pkg/pdfcpu/form"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

func sanitizePDFBytes(data []byte) ([]byte, error) {
	configuration := pdfConfiguration()
	configuration.Cmd = model.REMOVEPROPERTIES
	context, err := api.ReadValidateAndOptimize(bytes.NewReader(data), configuration)
	if err != nil {
		return nil, fmt.Errorf("read embedded PDF: %w", err)
	}

	root, err := context.Catalog()
	if err != nil {
		return nil, fmt.Errorf("read PDF catalog: %w", err)
	}
	if _, present := root["AcroForm"]; present {
		if _, err := pdfform.RemoveFormFields(context, nil); err != nil {
			return nil, fmt.Errorf("remove PDF form data: %w", err)
		}
	}
	if _, err := pdfcpu.RemoveAnnotations(context, nil, nil, nil, false); err != nil {
		return nil, fmt.Errorf("remove PDF annotations: %w", err)
	}
	names, err := context.DereferenceDict(root["Names"])
	if err != nil {
		return nil, fmt.Errorf("read PDF name tree: %w", err)
	}
	if names != nil {
		if _, present := names["EmbeddedFiles"]; present {
			if _, err := context.RemoveAttachments(nil); err != nil {
				return nil, fmt.Errorf("remove PDF attachments: %w", err)
			}
		}
	}
	if err := removePDFMetadataObjects(context); err != nil {
		return nil, err
	}
	if _, err := pdfcpu.PropertiesRemove(context, nil); err != nil {
		return nil, fmt.Errorf("remove PDF document properties: %w", err)
	}
	if err := removePDFDocumentInfo(context); err != nil {
		return nil, err
	}

	// Active document actions are not publication content and can retain user
	// data or execute code. KFX navigation is reconstructed separately.
	for _, key := range []string{"OpenAction", "AA", "Perms", "PieceInfo"} {
		delete(root, key)
	}
	if names != nil {
		delete(names, "JavaScript")
		delete(names, "EmbeddedFiles")
	}

	var output bytes.Buffer
	if err := api.WriteContext(context, &output); err != nil {
		return nil, fmt.Errorf("write metadata-free PDF: %w", err)
	}
	if err := verifySanitizedPDF(output.Bytes()); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func removePDFDocumentInfo(context *model.Context) error {
	if context.Info != nil {
		info, err := context.DereferenceDict(*context.Info)
		if err != nil {
			return fmt.Errorf("read PDF document information: %w", err)
		}
		if info == nil {
			return errors.New("read PDF document information: missing object")
		}
		// An information value may itself be indirect. Clearing the referenced
		// object prevents a removed owner string from surviving as unreachable
		// bytes in writers that retain the complete cross-reference table.
		for _, object := range info {
			if reference, ok := object.(types.IndirectRef); ok {
				if entry, found := context.FindTableEntryForIndRef(&reference); found && entry != nil {
					entry.Object = types.StringLiteral("")
				}
			}
		}
		clear(info)
	}
	context.Title = ""
	context.Subject = ""
	context.Author = ""
	context.Creator = ""
	context.Keywords = ""
	context.KeywordList = nil
	context.Properties = make(map[string]string)
	// The first trailer ID is otherwise retained across rewrites and can act
	// as a per-delivery fingerprint. pdfcpu will create a fresh output ID.
	context.ID = nil
	return nil
}

func removePDFMetadataObjects(context *model.Context) error {
	references := make(map[int]struct{})
	remove := func(dict types.Dict) {
		if dict == nil {
			return
		}
		object, present := dict["Metadata"]
		if !present {
			return
		}
		delete(dict, "Metadata")
		if reference, ok := object.(types.IndirectRef); ok {
			references[reference.ObjectNumber.Value()] = struct{}{}
		}
	}
	for _, entry := range context.Table {
		if entry == nil || entry.Free {
			continue
		}
		switch object := entry.Object.(type) {
		case types.Dict:
			remove(object)
		case types.StreamDict:
			remove(object.Dict)
		}
	}
	for objectNumber := range references {
		if err := context.FreeObject(objectNumber); err != nil {
			return fmt.Errorf("remove PDF metadata object %d: %w", objectNumber, err)
		}
	}
	context.CatalogXMPMeta = nil
	return nil
}

func verifySanitizedPDF(data []byte) error {
	properties, err := api.Properties(bytes.NewReader(data), pdfConfiguration())
	if err != nil {
		return fmt.Errorf("verify PDF properties: %w", err)
	}
	if len(properties) != 0 {
		return fmt.Errorf("metadata-free PDF still contains %d document properties", len(properties))
	}
	info, err := api.PDFInfo(bytes.NewReader(data), "", nil, false, pdfConfiguration())
	if err != nil {
		return fmt.Errorf("verify PDF document information: %w", err)
	}
	if info.Title != "" || info.Author != "" || info.Subject != "" || info.Creator != "" || len(info.Keywords) != 0 {
		return errors.New("metadata-free PDF still contains standard document information")
	}
	context, err := api.ReadValidateAndOptimize(bytes.NewReader(data), pdfConfiguration())
	if err != nil {
		return fmt.Errorf("verify PDF metadata streams: %w", err)
	}
	metadata, err := pdfcpu.ExtractMetadata(context)
	if err != nil {
		return fmt.Errorf("verify PDF metadata streams: %w", err)
	}
	if len(metadata) != 0 {
		return fmt.Errorf("metadata-free PDF still contains %d metadata streams", len(metadata))
	}
	return nil
}
