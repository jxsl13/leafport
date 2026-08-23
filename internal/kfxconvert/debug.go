package kfxconvert

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

const kfxBaseSymbolCount = 852

// DumpEntities writes a bounded semantic view of selected entity types. It is
// intended for Leafport's maintenance command, not the conversion path.
func DumpEntities(path string, types []uint32, limit int, output io.Writer) error {
	wanted := make(map[uint32]bool, len(types))
	for _, entityType := range types {
		wanted[entityType] = true
	}
	archive, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer archive.Close()
	type namedContainer struct {
		name      string
		container Container
	}
	var containers []namedContainer
	var sharedSymbols []string
	for _, file := range archive.File {
		if !isKFXContainerName(file.Name) {
			continue
		}
		data, readErr := readZipFile(file)
		if readErr != nil || !bytes.HasPrefix(data, []byte("CONT")) {
			continue
		}
		container, parseErr := ParseContainer(data)
		if parseErr != nil {
			return fmt.Errorf("%s: %w", file.Name, parseErr)
		}
		if len(container.DocumentSymbols) > len(sharedSymbols) {
			sharedSymbols = container.DocumentSymbols
		}
		containers = append(containers, namedContainer{name: file.Name, container: container})
	}
	written := 0
	for _, item := range containers {
		container := item.container
		for _, entity := range container.Entities {
			if len(wanted) != 0 && !wanted[entity.Type] {
				continue
			}
			if limit > 0 && written >= limit {
				break
			}
			name := resolveSymbol(uint64(entity.ID), sharedSymbols)
			fmt.Fprintf(output, "%s type=$%d id=$%d(%s) bytes=%d\n", item.name, entity.Type, entity.ID, name, len(entity.Payload))
			if entity.Type != kfxRawMediaType && entity.Type != kfxRawFontType {
				values, valueErr := parseIonValues(entity.Payload)
				if valueErr != nil {
					fmt.Fprintf(output, "  <Ion error: %v>\n", valueErr)
				} else {
					for _, value := range values {
						fmt.Fprintf(output, "  %s\n", formatIon(value, sharedSymbols, 0))
					}
				}
			}
			written++
		}
	}
	return nil
}

// DumpDependency writes the container-entity-map entry for one fragment ID.
func DumpDependency(path string, target uint64, output io.Writer) error {
	book, err := loadBook(path)
	if err != nil {
		return err
	}
	for _, entityMap := range book.entities[419] {
		entries := ionFieldValue(entityMap, 253)
		if entries == nil || entries.kind != ionList {
			continue
		}
		for _, entry := range entries.children {
			id, ok := ionSymbolID(ionFieldValue(entry, 155))
			if ok && id == target {
				fmt.Fprintln(output, formatIon(entry, book.symbols, 0))
				return nil
			}
		}
	}
	return fmt.Errorf("no dependency entry for symbol $%d", target)
}

// DumpNode writes every storyline or template struct carrying the requested
// KFX location ID. It is useful when an anchor resolves during indexing but
// its target is absent from reconstructed output.
func DumpNode(path string, target uint32, output io.Writer) error {
	book, err := loadBook(path)
	if err != nil {
		return err
	}
	found := 0
	dumpValues := func(kind string, values map[uint32]*ionValue) {
		for owner, root := range values {
			visitIonTree(root, func(value *ionValue) {
				id, ok := ionID(ionFieldValue(value, 155))
				if !ok || id != uint64(target) {
					return
				}
				fmt.Fprintf(output, "%s $%d node $%d: %s\n", kind, owner, target, formatIon(value, book.symbols, 0))
				found++
			})
		}
	}
	dumpValues("storyline", book.storylines)
	dumpValues("template", book.templates)
	if found == 0 {
		return fmt.Errorf("no content node with location ID $%d", target)
	}
	sectionIDs := readingOrderSections(book.document)
	builder := epubBuilder{book: book, assets: make(map[uint32]*epubAsset), language: "und"}
	builder.indexSections(sectionIDs)
	fmt.Fprintf(output, "indexed section: %d\n", builder.nodeSections[target])
	needle := fmt.Sprintf(`id="kfx-node-%d"`, target)
	var indexed, rendered []int
	for index, sectionID := range sectionIDs {
		probe := epubBuilder{book: book}
		probe.indexSections([]uint32{sectionID})
		if probe.nodeSections[target] != 0 {
			indexed = append(indexed, index+1)
			fmt.Fprintf(output, "section %d ($%d) indexes target through root stories %v\n",
				index+1, sectionID, uniqueSymbols(ionFieldValue(book.sections[sectionID], 141), 176))
		}
		section, renderErr := builder.renderSection(sectionID, index+1)
		if renderErr != nil {
			fmt.Fprintf(output, "section %d render error: %v\n", index+1, renderErr)
			continue
		}
		if bytes.Contains(section.data, []byte(needle)) {
			rendered = append(rendered, index+1)
		}
	}
	fmt.Fprintf(output, "individually indexed sections: %v\n", indexed)
	fmt.Fprintf(output, "rendered sections: %v\n", rendered)
	return nil
}

func visitIonTree(root *ionValue, visit func(*ionValue)) {
	seen := make(map[*ionValue]bool)
	var walk func(*ionValue)
	walk = func(value *ionValue) {
		if value == nil || seen[value] {
			return
		}
		seen[value] = true
		if value.kind == ionStruct {
			visit(value)
		}
		for _, field := range value.fields {
			walk(field.value)
		}
		for _, child := range value.children {
			walk(child)
		}
	}
	walk(root)
}

func resolveSymbol(id uint64, local []string) string {
	if id >= kfxBaseSymbolCount {
		index := id - kfxBaseSymbolCount
		if index < uint64(len(local)) {
			return local[index]
		}
	}
	return "$" + strconv.FormatUint(id, 10)
}

func formatIon(value *ionValue, local []string, depth int) string {
	if depth >= 12 {
		return "…"
	}
	annotation := ""
	if len(value.annotations) != 0 {
		parts := make([]string, len(value.annotations))
		for index, id := range value.annotations {
			parts[index] = resolveSymbol(id, local)
		}
		annotation = strings.Join(parts, "::") + "::"
	}
	switch value.kind {
	case ionNull:
		return annotation + "null"
	case ionBool:
		return annotation + strconv.FormatBool(value.boolean)
	case ionInt:
		return annotation + strconv.FormatInt(value.integer, 10)
	case ionFloat:
		return annotation + strconv.FormatFloat(value.floating, 'g', -1, 64)
	case ionSymbol:
		return annotation + resolveSymbol(value.unsigned, local)
	case ionString:
		return annotation + strconv.Quote(value.text)
	case ionBLOB, ionCLOB:
		return annotation + fmt.Sprintf("{{%d bytes}}", len(value.data))
	case ionList, ionSExpr:
		parts := make([]string, 0, len(value.children))
		for index, child := range value.children {
			if index == 40 {
				parts = append(parts, fmt.Sprintf("…%d more", len(value.children)-index))
				break
			}
			parts = append(parts, formatIon(child, local, depth+1))
		}
		open, close := "[", "]"
		if value.kind == ionSExpr {
			open, close = "(", ")"
		}
		return annotation + open + strings.Join(parts, ", ") + close
	case ionStruct:
		fields := append([]ionField(nil), value.fields...)
		sort.SliceStable(fields, func(i, j int) bool { return fields[i].id < fields[j].id })
		parts := make([]string, 0, len(fields))
		for index, field := range fields {
			if index == 40 {
				parts = append(parts, fmt.Sprintf("…%d more", len(fields)-index))
				break
			}
			parts = append(parts, resolveSymbol(field.id, local)+": "+formatIon(field.value, local, depth+1))
		}
		return annotation + "{" + strings.Join(parts, ", ") + "}"
	default:
		return annotation + fmt.Sprintf("<Ion %d: %d bytes>", value.kind, len(value.data))
	}
}
