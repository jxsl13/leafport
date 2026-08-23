package kfxconvert

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const svgNamespace = "http://www.w3.org/2000/svg"

func (builder *epubBuilder) renderKVG(node *ionValue) (string, error) {
	width, widthOK := kvgNumber(ionFieldValue(node, 66))
	height, heightOK := kvgNumber(ionFieldValue(node, 67))
	if !widthOK || !heightOK || width <= 0 || height <= 0 {
		return "", errors.New("KVG vector content has no valid width and height")
	}
	content := append([]*ionValue(nil), ionListValues(ionFieldValue(node, 146))...)
	var shapes strings.Builder
	for _, shape := range ionListValues(ionFieldValue(node, 250)) {
		fragment, used, err := builder.renderKVGShape(shape, content)
		if err != nil {
			return "", err
		}
		if used >= 0 {
			content = append(content[:used], content[used+1:]...)
		}
		shapes.WriteString(fragment)
	}
	if len(content) != 0 {
		return "", fmt.Errorf("KVG vector content left %d unreferenced content node(s)", len(content))
	}
	return `<svg xmlns="` + svgNamespace + `" version="1.1" preserveAspectRatio="xMidYMid meet" viewBox="0 0 ` +
		formatKVGNumber(width) + ` ` + formatKVGNumber(height) + `"` + builder.nodeAttributes(node) + `>` + shapes.String() + `</svg>`, nil
}

func (builder *epubBuilder) renderKVGShape(shape *ionValue, content []*ionValue) (string, int, error) {
	kind, ok := ionSymbolID(ionFieldValue(shape, 159))
	if !ok {
		return "", -1, errors.New("KVG shape has no type")
	}
	attributes, err := kvgShapeAttributes(shape)
	if err != nil {
		return "", -1, err
	}
	switch kind {
	case 273:
		path, err := builder.kvgPath(ionFieldValue(shape, 249), make(map[*ionValue]bool))
		if err != nil {
			return "", -1, err
		}
		return `<path d="` + escapeXML(path) + `"` + attributes + `/>`, -1, nil
	case 270:
		source, ok := ionID(ionFieldValue(shape, 474))
		if !ok {
			return "", -1, errors.New("KVG text shape has no source location")
		}
		for index, candidate := range content {
			resolved := candidate
			if templateID, isTemplate := ionSymbolID(candidate); isTemplate && templateID <= uint64(^uint32(0)) {
				resolved = builder.book.templates[uint32(templateID)]
			}
			location, locationOK := ionID(ionFieldValue(resolved, 155))
			if !locationOK {
				location, locationOK = ionID(ionFieldValue(resolved, 598))
			}
			if !locationOK || location != source {
				continue
			}
			text := builder.collectAnnotationText(resolved, make(map[*ionValue]bool))
			return `<text` + attributes + `>` + escapeText(text) + `</text>`, index, nil
		}
		return "", -1, fmt.Errorf("KVG text shape references missing content location $%d", source)
	default:
		return "", -1, fmt.Errorf("unsupported KVG shape type $%d", kind)
	}
}

func (builder *epubBuilder) kvgPath(value *ionValue, seen map[*ionValue]bool) (string, error) {
	if value == nil || seen[value] {
		return "", errors.New("missing or cyclic KVG path")
	}
	seen[value] = true
	if value.kind == ionStruct {
		bundleID, bundleOK := ionID(ionFieldValue(value, 4))
		index, indexOK := ionInteger(ionFieldValue(value, 403))
		if !bundleOK || !indexOK || bundleID > uint64(^uint32(0)) || index < 0 {
			return "", errors.New("KVG path has an invalid path-bundle reference")
		}
		bundle := builder.book.entities[692][uint32(bundleID)]
		paths := ionListValues(ionFieldValue(bundle, 693))
		if index >= int64(len(paths)) {
			return "", fmt.Errorf("KVG path bundle $%d has no path %d", bundleID, index)
		}
		return builder.kvgPath(paths[index], seen)
	}
	values := ionListValues(value)
	if values == nil {
		return "", errors.New("KVG path is not a list")
	}
	var output []string
	for index := 0; index < len(values); {
		instruction, ok := ionInteger(values[index])
		if !ok {
			return "", errors.New("KVG path instruction is not an integer")
		}
		index++
		command := ""
		arguments := 0
		switch instruction {
		case 0:
			command, arguments = "M", 2
		case 1:
			command, arguments = "L", 2
		case 2:
			command, arguments = "Q", 4
		case 3:
			command, arguments = "C", 6
		case 4:
			command = "Z"
		default:
			return "", fmt.Errorf("unsupported KVG path instruction %d", instruction)
		}
		if len(values)-index < arguments {
			return "", fmt.Errorf("KVG path command %s is missing arguments", command)
		}
		output = append(output, command)
		for count := 0; count < arguments; count++ {
			number, ok := ionNumber(values[index])
			if !ok {
				return "", fmt.Errorf("KVG path command %s has a non-numeric argument", command)
			}
			output = append(output, formatKVGNumber(number))
			index++
		}
	}
	return strings.Join(output, " "), nil
}

func kvgShapeAttributes(shape *ionValue) (string, error) {
	attributes := make(map[string]string)
	for field, name := range map[uint64]string{70: "fill", 75: "stroke"} {
		if value := ionFieldValue(shape, field); value != nil {
			color, ok := cssColor(value)
			if !ok {
				return "", fmt.Errorf("KVG %s is not a color", name)
			}
			attributes[name] = color
		}
	}
	for field, name := range map[uint64]string{72: "fill-opacity", 530: "stroke-miterlimit"} {
		if value := ionFieldValue(shape, field); value != nil {
			number, ok := ionNumber(value)
			if !ok {
				return "", fmt.Errorf("KVG %s is not numeric", name)
			}
			attributes[name] = formatKVGNumber(number)
		}
	}
	for field, name := range map[uint64]string{532: "stroke-dashoffset", 76: "stroke-width"} {
		if value := ionFieldValue(shape, field); value != nil {
			length, ok := svgLength(value)
			if !ok {
				return "", fmt.Errorf("KVG %s is not a length", name)
			}
			attributes[name] = length
		}
	}
	if value, ok := ionSymbolID(ionFieldValue(shape, 77)); ok {
		attributes["stroke-linecap"] = map[uint64]string{534: "butt", 533: "round", 341: "square"}[value]
	}
	if value, ok := ionSymbolID(ionFieldValue(shape, 529)); ok {
		attributes["stroke-linejoin"] = map[uint64]string{536: "bevel", 535: "miter", 533: "round"}[value]
	}
	if values := ionListValues(ionFieldValue(shape, 531)); len(values) != 0 {
		parts := make([]string, 0, len(values))
		for _, value := range values {
			length, ok := svgLength(value)
			if !ok {
				return "", errors.New("KVG stroke-dasharray contains a non-length")
			}
			parts = append(parts, length)
		}
		attributes["stroke-dasharray"] = strings.Join(parts, " ")
	}
	if transform := ionListValues(ionFieldValue(shape, 98)); len(transform) != 0 {
		if len(transform) != 6 {
			return "", errors.New("KVG transform is not a six-value matrix")
		}
		parts := make([]string, len(transform))
		for index, value := range transform {
			number, ok := ionNumber(value)
			if !ok {
				return "", errors.New("KVG transform contains a non-numeric value")
			}
			parts[index] = formatKVGNumber(number)
		}
		attributes["transform"] = "matrix(" + strings.Join(parts, " ") + ")"
	}
	if attributes["stroke"] != "" && attributes["fill"] == "" {
		attributes["fill"] = "none"
	}
	names := []string{"fill", "fill-opacity", "stroke", "stroke-dasharray", "stroke-dashoffset", "stroke-linecap", "stroke-linejoin", "stroke-miterlimit", "stroke-width", "transform"}
	var output strings.Builder
	for _, name := range names {
		if value := attributes[name]; value != "" {
			output.WriteByte(' ')
			output.WriteString(name)
			output.WriteString(`="`)
			output.WriteString(escapeXML(value))
			output.WriteByte('"')
		}
	}
	return output.String(), nil
}

func svgLength(value *ionValue) (string, bool) {
	if length, ok := cssDimension(value); ok {
		return length, true
	}
	if number, ok := ionNumber(value); ok {
		return formatKVGNumber(number), true
	}
	return "", false
}

func kvgNumber(value *ionValue) (float64, bool) {
	if number, ok := ionNumber(value); ok {
		return number, true
	}
	return ionNumber(ionFieldValue(value, 307))
}

func formatKVGNumber(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}
