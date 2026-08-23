package kfxconvert

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

type pdfLinkReference struct {
	uri          string
	targetNode   uint32
	targetSymbol bool
}

type kfxPDFLink struct {
	sectionID uint32
	x         float64
	y         float64
	width     float64
	height    float64
	target    pdfLinkReference
}

type kfxPDFPosition struct {
	sectionID uint32
	x         float64
	y         float64
}

type kfxPDFGeometry struct {
	canvasWidth  float64
	canvasHeight float64
	links        []kfxPDFLink
	positions    map[uint32]kfxPDFPosition
}

func addKFXPDFLinks(book *decodedBook, pages []Page, destination string) (int, error) {
	if book == nil || len(pages) == 0 {
		return 0, nil
	}
	geometry := collectKFXPDFGeometry(book, pages)
	if len(geometry.links) == 0 {
		return 0, nil
	}
	file, err := os.Open(destination)
	if err != nil {
		return 0, err
	}
	dimensions, dimsErr := api.PageDims(file, pdfConfiguration())
	closeErr := file.Close()
	if dimsErr != nil || closeErr != nil {
		return 0, fmt.Errorf("read PDF page dimensions for links: %w", errors.Join(dimsErr, closeErr))
	}
	if len(dimensions) != len(pages) {
		return 0, fmt.Errorf("map KFX links: PDF has %d page dimensions for %d KFX pages", len(dimensions), len(pages))
	}

	firstPage := make(map[uint32]int)
	for index, page := range pages {
		if firstPage[page.SectionID] == 0 {
			firstPage[page.SectionID] = index + 1
		}
	}
	sectionIDs := readingOrderSections(book.document)
	builder := epubBuilder{book: book}
	builder.indexSections(sectionIDs)
	annotations := make(map[int][]model.AnnotationRenderer)
	created := 0
	for _, link := range geometry.links {
		pageNumber := firstPage[link.sectionID]
		canvas := geometry.sections[link.sectionID]
		if pageNumber == 0 || canvas.canvasWidth <= 0 || canvas.canvasHeight <= 0 {
			continue
		}
		dimension := dimensions[pageNumber-1]
		rectangle := scaleKFXRectangle(link, canvas, dimension)
		if rectangle == nil {
			continue
		}
		var target *model.Destination
		if link.target.targetNode != 0 {
			position, positionOK := kfxPDFPosition{}, false
			if link.target.targetSymbol {
				section := builder.sectionSections[link.target.targetNode]
				if section > 0 && section <= len(sectionIDs) {
					position = kfxPDFPosition{sectionID: sectionIDs[section-1]}
					positionOK = true
				}
			} else {
				position, positionOK = geometry.positions[link.target.targetNode]
				if !positionOK {
					section := builder.nodeSections[link.target.targetNode]
					if section > 0 && section <= len(sectionIDs) {
						position = kfxPDFPosition{sectionID: sectionIDs[section-1]}
						positionOK = true
					}
				}
			}
			if !positionOK {
				continue
			}
			targetPage := firstPage[position.sectionID]
			targetCanvas := geometry.sections[position.sectionID]
			if targetPage == 0 || targetCanvas.canvasWidth <= 0 || targetCanvas.canvasHeight <= 0 {
				continue
			}
			targetDimension := dimensions[targetPage-1]
			left := int(math.Round(position.x * targetDimension.Width / targetCanvas.canvasWidth))
			top := int(math.Round(targetDimension.Height - position.y*targetDimension.Height/targetCanvas.canvasHeight))
			target = &model.Destination{Typ: model.DestXYZ, PageNr: targetPage, Left: left, Top: top, Zoom: 1}
		}
		created++
		annotation := model.NewLinkAnnotation(
			*rectangle, 0, "", fmt.Sprintf("leafport-link-%d", created), "", 0, nil,
			target, link.target.uri, nil, false, 0, model.BSSolid,
		)
		annotations[pageNumber] = append(annotations[pageNumber], annotation)
	}
	if created == 0 {
		return 0, nil
	}
	if created != len(geometry.links) {
		return 0, fmt.Errorf("map KFX PDF links: resolved %d of %d link regions", created, len(geometry.links))
	}
	if err := removeExistingPDFLinks(destination); err != nil {
		return 0, err
	}
	if err := api.AddAnnotationsMapFile(destination, "", annotations, pdfConfiguration(), false); err != nil {
		return 0, fmt.Errorf("add KFX PDF links: %w", err)
	}
	if err := os.Chmod(destination, 0o600); err != nil {
		return 0, fmt.Errorf("protect linked PDF: %w", err)
	}
	if err := validateKFXPDFLinks(destination, created); err != nil {
		return 0, err
	}
	return created, nil
}

type collectedKFXPDFGeometry struct {
	sections  map[uint32]*kfxPDFGeometry
	positions map[uint32]kfxPDFPosition
	links     []kfxPDFLink
}

func collectKFXPDFGeometry(book *decodedBook, pages []Page) collectedKFXPDFGeometry {
	result := collectedKFXPDFGeometry{
		sections: make(map[uint32]*kfxPDFGeometry), positions: make(map[uint32]kfxPDFPosition),
	}
	seenSections := make(map[uint32]bool)
	for _, page := range pages {
		sectionID := page.SectionID
		if seenSections[sectionID] {
			continue
		}
		seenSections[sectionID] = true
		sectionGeometry := &kfxPDFGeometry{positions: make(map[uint32]kfxPDFPosition)}
		collectKFXSectionGeometry(book, sectionID, sectionGeometry)
		result.sections[sectionID] = sectionGeometry
		result.links = append(result.links, sectionGeometry.links...)
		for id, position := range sectionGeometry.positions {
			result.positions[id] = position
		}
	}
	return result
}

func collectKFXSectionGeometry(book *decodedBook, sectionID uint32, result *kfxPDFGeometry) {
	seenStories := make(map[uint32]bool)
	var visit func(*ionValue)
	visit = func(value *ionValue) {
		if value == nil {
			return
		}
		if value.kind == ionStruct {
			if width, widthOK := positiveIonNumber(ionFieldValue(value, 66)); widthOK {
				if height, heightOK := positiveIonNumber(ionFieldValue(value, 67)); heightOK &&
					width*height > result.canvasWidth*result.canvasHeight {
					result.canvasWidth, result.canvasHeight = width, height
				}
			}
			x, xOK := ionNumber(ionFieldValue(value, 59))
			y, yOK := ionNumber(ionFieldValue(value, 58))
			width, widthOK := positiveIonNumber(ionFieldValue(value, 56))
			height, heightOK := positiveIonNumber(ionFieldValue(value, 57))
			if id, idOK := ionID(ionFieldValue(value, 155)); idOK && id <= uint64(^uint32(0)) {
				position := kfxPDFPosition{sectionID: sectionID}
				if xOK {
					position.x = x
				}
				if yOK {
					position.y = y
				}
				result.positions[uint32(id)] = position
			}
			if xOK && yOK && widthOK && heightOK {
				result.links = append(result.links, kfxNodePDFLinks(book, sectionID, value, x, y, width, height)...)
			}
			for _, field := range value.fields {
				if field.id == 176 {
					if storyID, ok := ionSymbolID(field.value); ok && storyID <= uint64(^uint32(0)) {
						id := uint32(storyID)
						if !seenStories[id] {
							seenStories[id] = true
							visit(book.storylines[id])
						}
						continue
					}
				}
				visit(field.value)
			}
			return
		}
		for _, child := range value.children {
			visit(child)
		}
	}
	visit(book.sections[sectionID])
}

func kfxNodePDFLinks(book *decodedBook, sectionID uint32, node *ionValue, x, y, width, height float64) []kfxPDFLink {
	events := ionFieldValue(node, 142)
	if events == nil || events.kind != ionList {
		if target, ok := book.pdfLinkReference(ionFieldValue(node, 179)); ok {
			return []kfxPDFLink{{sectionID: sectionID, x: x, y: y, width: width, height: height, target: target}}
		}
		return nil
	}
	var result []kfxPDFLink
	offset := 0.0
	for _, event := range events.children {
		eventWidth, widthOK := positiveIonNumber(ionFieldValue(event, 56))
		if !widthOK {
			continue
		}
		target, targetOK := book.pdfLinkReference(ionFieldValue(event, 179))
		if targetOK {
			link := kfxPDFLink{sectionID: sectionID, x: x + offset, y: y, width: eventWidth, height: height, target: target}
			if len(result) != 0 && samePDFLinkReference(result[len(result)-1].target, target) &&
				math.Abs(result[len(result)-1].x+result[len(result)-1].width-link.x) < 0.01 {
				result[len(result)-1].width += eventWidth
			} else {
				result = append(result, link)
			}
		}
		offset += eventWidth
	}
	return result
}

func (book *decodedBook) pdfLinkReference(value *ionValue) (pdfLinkReference, bool) {
	if raw, ok := ionText(value); ok {
		if book.privacy != nil && book.privacy.matches(raw) {
			return pdfLinkReference{}, false
		}
		uri := safeExternalURL(raw)
		return pdfLinkReference{uri: uri}, uri != ""
	}
	id, ok := ionSymbolID(value)
	if !ok || id > uint64(^uint32(0)) {
		return pdfLinkReference{}, false
	}
	anchor, ok := book.anchors[uint32(id)]
	if !ok {
		return pdfLinkReference{}, false
	}
	if anchor.externalURL != "" {
		uri := safeExternalURL(anchor.externalURL)
		return pdfLinkReference{uri: uri}, uri != ""
	}
	if anchor.targetNode == 0 {
		return pdfLinkReference{}, false
	}
	return pdfLinkReference{targetNode: anchor.targetNode, targetSymbol: anchor.targetSymbol}, true
}

func positiveIonNumber(value *ionValue) (float64, bool) {
	number, ok := ionNumber(value)
	return number, ok && number > 0 && !math.IsInf(number, 0) && !math.IsNaN(number)
}

func samePDFLinkReference(first, second pdfLinkReference) bool {
	return first.uri == second.uri && first.targetNode == second.targetNode && first.targetSymbol == second.targetSymbol
}

func scaleKFXRectangle(link kfxPDFLink, canvas *kfxPDFGeometry, dimension types.Dim) *types.Rectangle {
	left := link.x * dimension.Width / canvas.canvasWidth
	right := (link.x + link.width) * dimension.Width / canvas.canvasWidth
	top := dimension.Height - link.y*dimension.Height/canvas.canvasHeight
	bottom := dimension.Height - (link.y+link.height)*dimension.Height/canvas.canvasHeight
	left, right = max(0, left), min(dimension.Width, right)
	bottom, top = max(0, bottom), min(dimension.Height, top)
	if right <= left || top <= bottom {
		return nil
	}
	return types.NewRectangle(left, bottom, right, top)
}

func validateKFXPDFLinks(path string, expected int) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	context, readErr := api.ReadAndValidate(file, pdfConfiguration())
	if readErr != nil {
		_ = file.Close()
		return fmt.Errorf("validate PDF links: %w", readErr)
	}
	count := 0
	ids := make(map[string]bool)
	for pageNumber := 1; pageNumber <= context.PageCount; pageNumber++ {
		page, _, _, pageErr := context.PageDict(pageNumber, false)
		if pageErr != nil {
			_ = file.Close()
			return fmt.Errorf("validate PDF links on page %d: %w", pageNumber, pageErr)
		}
		annotations, annotationErr := context.DereferenceArray(page["Annots"])
		if annotationErr != nil {
			_ = file.Close()
			return fmt.Errorf("validate PDF links on page %d: %w", pageNumber, annotationErr)
		}
		for _, annotationObject := range annotations {
			annotation, dereferenceErr := context.DereferenceDict(annotationObject)
			if dereferenceErr != nil {
				_ = file.Close()
				return fmt.Errorf("validate PDF link on page %d: %w", pageNumber, dereferenceErr)
			}
			if annotation == nil || annotation.NameEntry("Subtype") == nil || *annotation.NameEntry("Subtype") != "Link" {
				continue
			}
			id := annotation.StringEntry("NM")
			if id == nil || !strings.HasPrefix(*id, "leafport-link-") {
				continue
			}
			if ids[*id] {
				_ = file.Close()
				return fmt.Errorf("validate PDF links: duplicate annotation ID %q", *id)
			}
			ids[*id] = true
			if brokenPDFLink(context, annotation) {
				_ = file.Close()
				return fmt.Errorf("validate PDF links: annotation %q on page %d has an unresolved destination", *id, pageNumber)
			}
			count++
		}
	}
	if closeErr := file.Close(); closeErr != nil {
		return fmt.Errorf("validate PDF links: %w", closeErr)
	}
	if count != expected {
		return fmt.Errorf("validate PDF links: found %d Leafport links; expected %d", count, expected)
	}
	return nil
}

func removeExistingPDFLinks(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	annotations, readErr := api.Annotations(file, nil, pdfConfiguration())
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return fmt.Errorf("inspect inherited PDF links: %w", errors.Join(readErr, closeErr))
	}
	linkCount := 0
	for _, page := range annotations {
		linkCount += len(page[model.AnnLink].Map)
	}
	if linkCount == 0 {
		return nil
	}
	if err := api.RemoveAnnotationsFile(path, "", nil, []string{"Link"}, nil, pdfConfiguration(), false); err != nil {
		return fmt.Errorf("remove inherited PDF links: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("protect PDF after removing inherited links: %w", err)
	}
	return nil
}
