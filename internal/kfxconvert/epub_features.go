package kfxconvert

import (
	"fmt"
	"sort"
	"strings"
)

// validateEPUBFeatureSupport rejects KFX constructs that Leafport cannot yet
// reproduce faithfully. Producing no EPUB is preferable to silently dropping
// media, vector shapes, mathematical notation, or alternate layout content.
func validateEPUBFeatureSupport(book *decodedBook) error {
	if book == nil {
		return nil
	}
	unsupported := make(map[string]bool)
	inspect := func(value *ionValue) {
		if value == nil || value.kind != ionStruct {
			return
		}
		if kind, ok := ionSymbolID(ionFieldValue(value, 159)); ok {
			switch kind {
			case 0, 151, 269, 270, 271, 276, 277, 278, 279, 439, 454, 455, 596, 780:
			case 272, 274:
			default:
				unsupported[fmt.Sprintf("content type $%d", kind)] = true
			}
		}
		if ionFieldValue(value, 171) != nil {
			unsupported["conditional page-template content"] = true
		}
		if ionFieldValue(value, 426) != nil || ionFieldValue(value, 684) != nil {
			unsupported["region-magnification content"] = true
		}
	}
	visitIonValues(book.storylines, inspect)
	visitIonValues(book.templates, inspect)
	for _, style := range book.styles {
		for _, field := range style.fields {
			if !supportedEPUBStyleField(field.id) {
				unsupported[fmt.Sprintf("style property $%d", field.id)] = true
			}
		}
	}
	if len(unsupported) == 0 {
		return nil
	}
	features := make([]string, 0, len(unsupported))
	for feature := range unsupported {
		features = append(features, feature)
	}
	sort.Strings(features)
	return fmt.Errorf("faithful EPUB reconstruction is not implemented for: %s", strings.Join(features, ", "))
}

func supportedEPUBStyleField(field uint64) bool {
	switch field {
	case 10, 11, 12, 13, 15, 16, 19, 20, 21, 23, 27, 31, 32, 34, 36, 39, 40, 41, 42, 44,
		47, 48, 49, 50, 51, 52, 53, 54, 55, 56, 57, 62, 63, 64, 65, 70,
		83, 84, 85, 86, 87, 88, 89, 90, 91, 92, 93, 94, 95, 96, 97, 100,
		133, 134, 135, 140, 148, 149, 158, 173, 459, 460, 461, 462, 496,
		546, 554, 576, 577, 580, 583, 633, 761:
		return true
	default:
		return false
	}
}

func visitIonValues(values map[uint32]*ionValue, visit func(*ionValue)) {
	seen := make(map[*ionValue]bool)
	var walk func(*ionValue)
	walk = func(value *ionValue) {
		if value == nil || seen[value] {
			return
		}
		seen[value] = true
		visit(value)
		for _, field := range value.fields {
			walk(field.value)
		}
		for _, child := range value.children {
			walk(child)
		}
	}
	for _, value := range values {
		walk(value)
	}
}

func ionListValues(value *ionValue) []*ionValue {
	if value == nil || value.kind != ionList {
		return nil
	}
	return value.children
}
