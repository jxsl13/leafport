package kfxconvert

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/url"
	"slices"
	"strconv"
	"strings"

	amazonion "github.com/amazon-ion/ion-go/ion"
)

type resolvedPluginResource struct {
	id       uint32
	metadata resource
	data     []byte
	key      string
}

func (builder *epubBuilder) renderPlugin(node *ionValue) (string, error) {
	resourceID, ok := ionSymbolID(ionFieldValue(node, 175))
	if !ok || resourceID > uint64(^uint32(0)) {
		return "", errors.New("plugin node has no valid resource")
	}
	alt, _ := ionText(ionFieldValue(node, 584))
	alt = builder.book.redactText(alt)
	if builder.pluginStack == nil {
		builder.pluginStack = make(map[uint32]bool)
	}
	return builder.renderPluginResource(uint32(resourceID), alt, builder.nodeAttributes(node))
}

func (builder *epubBuilder) renderPluginResource(resourceID uint32, alt, attributes string) (string, error) {
	if builder.pluginStack[resourceID] {
		return "", fmt.Errorf("plugin resource cycle at $%d", resourceID)
	}
	metadata, data, ok := builder.book.resolveResource(resourceID)
	if !ok {
		return "", fmt.Errorf("missing plugin resource $%d", resourceID)
	}
	if strings.EqualFold(metadata.mime, "plugin/kfx-html-article") || strings.EqualFold(metadata.mime, "text/html") {
		return "", fmt.Errorf("KFX HTML/webview plugin $%d is not yet supported faithfully", resourceID)
	}
	if _, _, image := imageMediaType(data); image {
		asset, err := builder.addResolvedPluginAsset(resolvedPluginResource{
			id: resourceID, metadata: metadata, data: data,
			key: "resource:" + strconv.FormatUint(uint64(resourceID), 10),
		}, "image")
		if err != nil {
			return "", err
		}
		return `<img` + attributes + ` src="../` + escapeXML(asset.path) + `" alt="` + escapeXML(alt) + `"/>`, nil
	}
	if metadata.format != 287 {
		return "", fmt.Errorf("plugin resource $%d has unsupported KFX format $%d", resourceID, metadata.format)
	}

	pluginType, manifest, err := decodePluginManifest(data)
	if err != nil {
		return "", fmt.Errorf("decode plugin resource $%d: %w", resourceID, err)
	}
	builder.pluginStack[resourceID] = true
	defer delete(builder.pluginStack, resourceID)
	return builder.renderPluginManifest(resourceID, pluginType, manifest, alt, attributes)
}

func decodePluginManifest(data []byte) (string, map[string]any, error) {
	reader := amazonion.NewReaderBytes(data)
	if !reader.Next() {
		if err := reader.Err(); err != nil {
			return "", nil, err
		}
		return "", nil, errors.New("manifest is empty")
	}
	if reader.Type() != amazonion.StructType {
		return "", nil, errors.New("manifest is not an Ion struct")
	}
	annotations, err := reader.Annotations()
	if err != nil {
		return "", nil, err
	}
	if len(annotations) != 1 || annotations[0].Text == nil {
		return "", nil, fmt.Errorf("manifest must have exactly one textual type annotation, found %d", len(annotations))
	}
	pluginType := strings.TrimSpace(*annotations[0].Text)
	if pluginType == "" {
		return "", nil, errors.New("manifest has an empty type annotation")
	}
	if reader.Next() {
		return "", nil, errors.New("manifest contains more than one top-level Ion value")
	}
	if err := reader.Err(); err != nil {
		return "", nil, err
	}
	var manifest map[string]any
	if err := amazonion.Unmarshal(data, &manifest); err != nil {
		return "", nil, err
	}
	return pluginType, manifest, nil
}

func (builder *epubBuilder) renderPluginManifest(resourceID uint32, pluginType string, manifest map[string]any, alt, attributes string) (string, error) {
	alt = builder.book.redactText(alt)
	facets, err := requiredPluginMap(manifest, "facets")
	if err != nil {
		return "", fmt.Errorf("%s plugin $%d: %w", pluginType, resourceID, err)
	}
	properties, _ := pluginMap(manifest["properties"])

	switch pluginType {
	case "audio":
		media, err := requiredPluginMap(facets, "media")
		if err != nil {
			return "", fmt.Errorf("audio plugin $%d: %w", resourceID, err)
		}
		uri, err := requiredPluginString(media, "uri")
		if err != nil {
			return "", fmt.Errorf("audio plugin $%d: %w", resourceID, err)
		}
		asset, err := builder.addPluginURI(uri, "audio")
		if err != nil {
			return "", fmt.Errorf("audio plugin $%d: %w", resourceID, err)
		}
		fallback := alt
		if fallback == "" {
			fallback = "Audio playback is not available."
		}
		return `<audio` + attributes + ` controls="" src="../` + escapeXML(asset.path) + `">` + escapeText(fallback) + `</audio>`, nil

	case "video":
		media, err := requiredPluginMap(facets, "media")
		if err != nil {
			return "", fmt.Errorf("video plugin $%d: %w", resourceID, err)
		}
		uri, err := requiredPluginString(media, "uri")
		if err != nil {
			return "", fmt.Errorf("video plugin $%d: %w", resourceID, err)
		}
		asset, err := builder.addPluginURI(uri, "video")
		if err != nil {
			return "", fmt.Errorf("video plugin $%d: %w", resourceID, err)
		}
		var options strings.Builder
		if interaction, _ := pluginString(properties["user_interaction"]); interaction == "enabled" {
			options.WriteString(` controls=""`)
		}
		if events, ok := pluginMap(manifest["events"]); ok {
			if enterView, ok := pluginMap(events["enter_view"]); ok {
				if name, _ := pluginString(enterView["name"]); name == "start" {
					options.WriteString(` autoplay=""`)
				}
			}
		}
		if playContext, ok := pluginMap(properties["play_context"]); ok {
			if loopCount, ok := pluginNumber(playContext["loop_count"]); ok && loopCount < 0 {
				options.WriteString(` loop=""`)
			}
		}
		if poster, ok := pluginMap(facets["poster"]); ok {
			posterURI, err := requiredPluginString(poster, "uri")
			if err != nil {
				return "", fmt.Errorf("video plugin $%d poster: %w", resourceID, err)
			}
			posterAsset, err := builder.addPluginURI(posterURI, "image")
			if err != nil {
				return "", fmt.Errorf("video plugin $%d poster: %w", resourceID, err)
			}
			options.WriteString(` poster="../` + escapeXML(posterAsset.path) + `"`)
		}
		fallback := alt
		if fallback == "" {
			fallback = "Video playback is not available."
		}
		return `<video` + attributes + options.String() + ` src="../` + escapeXML(asset.path) + `">` + escapeText(fallback) + `</video>`, nil

	case "button", "image_sequence":
		images, err := requiredPluginList(facets, "images")
		if err != nil {
			return "", fmt.Errorf("%s plugin $%d: %w", pluginType, resourceID, err)
		}
		if len(images) == 0 {
			return "", fmt.Errorf("%s plugin $%d has no images", pluginType, resourceID)
		}
		var output strings.Builder
		output.WriteString(`<div` + attributes + `>`)
		for index, value := range images {
			image, ok := pluginMap(value)
			if !ok {
				return "", fmt.Errorf("%s plugin $%d image %d is not a struct", pluginType, resourceID, index)
			}
			uri, err := requiredPluginString(image, "uri")
			if err != nil {
				return "", fmt.Errorf("%s plugin $%d image %d: %w", pluginType, resourceID, index, err)
			}
			asset, err := builder.addPluginURI(uri, "image")
			if err != nil {
				return "", fmt.Errorf("%s plugin $%d image %d: %w", pluginType, resourceID, index, err)
			}
			output.WriteString(`<img src="../` + escapeXML(asset.path) + `" alt="` + escapeXML(alt) + `"/>`)
		}
		output.WriteString(`</div>`)
		return output.String(), nil

	case "zoomable":
		media, err := requiredPluginMap(facets, "media")
		if err != nil {
			return "", fmt.Errorf("zoomable plugin $%d: %w", resourceID, err)
		}
		uri, err := requiredPluginString(media, "uri")
		if err != nil {
			return "", fmt.Errorf("zoomable plugin $%d: %w", resourceID, err)
		}
		asset, err := builder.addPluginURI(uri, "image")
		if err != nil {
			return "", fmt.Errorf("zoomable plugin $%d: %w", resourceID, err)
		}
		return `<img` + attributes + ` src="../` + escapeXML(asset.path) + `" alt="` + escapeXML(alt) + `"/>`, nil

	case "hyperlink":
		uri, err := requiredPluginString(facets, "uri")
		if err != nil {
			return "", fmt.Errorf("hyperlink plugin $%d: %w", resourceID, err)
		}
		href, err := builder.pluginHyperlink(uri)
		if err != nil {
			return "", fmt.Errorf("hyperlink plugin $%d: %w", resourceID, err)
		}
		label := alt
		if label == "" {
			label = "Link"
		}
		return `<a` + attributes + ` href="` + escapeXML(href) + `">` + escapeText(label) + `</a>`, nil

	case "scrollable", "slideshow":
		children, err := requiredPluginList(facets, "children")
		if err != nil {
			return "", fmt.Errorf("%s plugin $%d: %w", pluginType, resourceID, err)
		}
		if propertyAlt, ok := pluginString(properties["alt_text"]); ok && strings.TrimSpace(propertyAlt) != "" {
			alt = builder.book.redactText(propertyAlt)
		}
		if visibility, _ := pluginString(properties["initial_visibility"]); visibility == "hide" {
			attributes = appendPluginStyle(attributes, "visibility:hidden")
		}
		var output strings.Builder
		output.WriteString(`<div` + attributes + `>`)
		for index, value := range children {
			child, ok := pluginMap(value)
			if !ok {
				return "", fmt.Errorf("%s plugin $%d child %d is not a struct", pluginType, resourceID, index)
			}
			uri, err := requiredPluginString(child, "uri")
			if err != nil {
				return "", fmt.Errorf("%s plugin $%d child %d: %w", pluginType, resourceID, index, err)
			}
			childAttributes := ""
			if bounds, ok := pluginMap(child["bounds"]); ok {
				style, err := pluginBoundsStyle(bounds)
				if err != nil {
					return "", fmt.Errorf("%s plugin $%d child %d bounds: %w", pluginType, resourceID, index, err)
				}
				if style != "" {
					childAttributes = ` style="` + escapeXML(style) + `"`
				}
			}
			childResource, err := builder.resolvePluginResource(uri)
			if err != nil {
				return "", fmt.Errorf("%s plugin $%d child %d: %w", pluginType, resourceID, index, err)
			}
			if childResource.id == 0 {
				return "", fmt.Errorf("%s plugin $%d child %d URI %q does not name a plugin entity", pluginType, resourceID, index, uri)
			}
			fragment, err := builder.renderPluginResource(childResource.id, alt, childAttributes)
			if err != nil {
				return "", fmt.Errorf("%s plugin $%d child %d: %w", pluginType, resourceID, index, err)
			}
			output.WriteString(fragment)
		}
		output.WriteString(`</div>`)
		return output.String(), nil

	case "webview":
		return "", fmt.Errorf("KFX webview plugin $%d is not yet supported faithfully", resourceID)
	default:
		return "", fmt.Errorf("KFX plugin $%d has unsupported type %q", resourceID, pluginType)
	}
}

func (builder *epubBuilder) addPluginURI(rawURI, expectedKind string) (*epubAsset, error) {
	resolved, err := builder.resolvePluginResource(rawURI)
	if err != nil {
		return nil, err
	}
	return builder.addResolvedPluginAsset(resolved, expectedKind)
}

func (builder *epubBuilder) addResolvedPluginAsset(resolved resolvedPluginResource, expectedKind string) (*epubAsset, error) {
	if builder.pluginAssets == nil {
		builder.pluginAssets = make(map[string]*epubAsset)
	}
	cacheKey := expectedKind + "\x00" + resolved.key
	if asset := builder.pluginAssets[cacheKey]; asset != nil {
		return asset, nil
	}
	extension, mediaType, ok := pluginMediaType(resolved.data, resolved.metadata.mime, expectedKind)
	if !ok {
		return nil, fmt.Errorf("resource %q is not supported %s media", resolved.metadata.location, expectedKind)
	}
	data, err := builder.book.sanitizeAsset(resolved.data, mediaType)
	if err != nil {
		return nil, fmt.Errorf("privacy-clean %s resource %q: %w", expectedKind, resolved.metadata.location, err)
	}
	builder.nextPluginAsset++
	directory := "media"
	image := strings.HasPrefix(mediaType, "image/")
	if image {
		directory = "images"
	}
	asset := &epubAsset{
		manifestID: fmt.Sprintf("plugin-asset-%04d", builder.nextPluginAsset),
		path:       fmt.Sprintf("%s/plugin-%04d.%s", directory, builder.nextPluginAsset, extension),
		mediaType:  mediaType,
		data:       data,
		image:      image,
	}
	builder.pluginAssets[cacheKey] = asset
	builder.assetOrder = append(builder.assetOrder, asset)
	return asset, nil
}

func (builder *epubBuilder) resolvePluginResource(rawURI string) (resolvedPluginResource, error) {
	candidates, err := kfxReferenceCandidates(rawURI)
	if err != nil {
		return resolvedPluginResource{}, err
	}
	ids := make([]int, 0, len(builder.book.resources))
	for id := range builder.book.resources {
		ids = append(ids, int(id))
	}
	slices.Sort(ids)
	for _, numericID := range ids {
		id := uint32(numericID)
		metadata := builder.book.resources[id]
		name := resolveSymbol(uint64(id), builder.book.symbols)
		if !matchesPluginReference(candidates, name) && !matchesPluginReference(candidates, metadata.location) {
			continue
		}
		resolvedMetadata, data, ok := builder.book.resolveResource(id)
		if !ok {
			return resolvedPluginResource{}, fmt.Errorf("KFX resource %q ($%d) has no raw media", candidates[0], id)
		}
		return resolvedPluginResource{
			id: id, metadata: resolvedMetadata, data: data,
			key: "resource:" + strconv.FormatUint(uint64(id), 10),
		}, nil
	}
	for _, candidate := range candidates {
		if data, ok := builder.book.rawMedia[candidate]; ok {
			return resolvedPluginResource{
				metadata: resource{location: candidate}, data: data, key: "raw:" + candidate,
			}, nil
		}
	}
	return resolvedPluginResource{}, fmt.Errorf("KFX URI %q does not resolve to a resource", rawURI)
}

func kfxReferenceCandidates(rawURI string) ([]string, error) {
	rawURI = strings.TrimSpace(rawURI)
	if rawURI == "" {
		return nil, errors.New("empty KFX resource URI")
	}
	parsed, err := url.Parse(rawURI)
	if err != nil {
		return nil, fmt.Errorf("invalid resource URI %q: %w", rawURI, err)
	}
	var name string
	switch strings.ToLower(parsed.Scheme) {
	case "":
		name = rawURI
	case "kfx":
		name = parsed.Host + parsed.Path
		if parsed.Opaque != "" {
			name = parsed.Opaque
		}
	default:
		return nil, fmt.Errorf("resource URI %q uses unsupported scheme %q", rawURI, parsed.Scheme)
	}
	name, err = url.PathUnescape(name)
	if err != nil {
		return nil, fmt.Errorf("invalid escaped resource URI %q: %w", rawURI, err)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("resource URI %q has no resource name", rawURI)
	}
	candidates := []string{name}
	if trimmed := strings.TrimPrefix(name, "/"); trimmed != name && trimmed != "" {
		candidates = append(candidates, trimmed)
	}
	return candidates, nil
}

func matchesPluginReference(candidates []string, value string) bool {
	for _, candidate := range candidates {
		if value == candidate {
			return true
		}
	}
	return false
}

func (builder *epubBuilder) pluginHyperlink(rawURI string) (string, error) {
	if builder.book.privacy != nil && builder.book.privacy.matches(rawURI) {
		return "", errors.New("plugin hyperlink contains redacted personal information")
	}
	if href := safeExternalURL(rawURI); href != "" {
		return href, nil
	}
	parsed, err := url.Parse(strings.TrimSpace(rawURI))
	if err != nil {
		return "", err
	}
	if strings.EqualFold(parsed.Scheme, "kfx") || parsed.Scheme == "" {
		asset, err := builder.addPluginURI(rawURI, "any")
		if err != nil {
			return "", err
		}
		return "../" + asset.path, nil
	}
	return "", fmt.Errorf("unsupported hyperlink URI scheme %q", parsed.Scheme)
}

func pluginMediaType(data []byte, declaredMIME, expectedKind string) (extension, mediaType string, ok bool) {
	if extension, mediaType, ok = imageMediaType(data); ok {
		return extension, mediaType, expectedKind == "image" || expectedKind == "any"
	}
	declaredMIME = strings.ToLower(strings.TrimSpace(strings.SplitN(declaredMIME, ";", 2)[0]))
	known := map[string][2]string{
		"audio/aac":   {"aac", "audio/aac"},
		"audio/mp4":   {"m4a", "audio/mp4"},
		"audio/mpeg":  {"mp3", "audio/mpeg"},
		"audio/ogg":   {"ogg", "audio/ogg"},
		"audio/wav":   {"wav", "audio/wav"},
		"audio/x-wav": {"wav", "audio/wav"},
		"audio/webm":  {"webm", "audio/webm"},
		"video/mp4":   {"mp4", "video/mp4"},
		"video/mpeg":  {"mpeg", "video/mpeg"},
		"video/ogg":   {"ogv", "video/ogg"},
		"video/webm":  {"webm", "video/webm"},
	}
	if pair, found := known[declaredMIME]; found && mediaKindAllowed(pair[1], expectedKind) {
		return pair[0], pair[1], true
	}
	switch {
	case bytes.HasPrefix(data, []byte("ID3")), len(data) >= 2 && data[0] == 0xff && data[1]&0xe0 == 0xe0:
		return "mp3", "audio/mpeg", expectedKind == "audio" || expectedKind == "any"
	case len(data) >= 12 && bytes.Equal(data[4:8], []byte("ftyp")):
		if expectedKind == "audio" {
			return "m4a", "audio/mp4", true
		}
		return "mp4", "video/mp4", expectedKind == "video" || expectedKind == "any"
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WAVE")):
		return "wav", "audio/wav", expectedKind == "audio" || expectedKind == "any"
	case bytes.HasPrefix(data, []byte("OggS")):
		if bytes.Contains(data[:min(len(data), 256)], []byte("theora")) {
			return "ogv", "video/ogg", expectedKind == "video" || expectedKind == "any"
		}
		return "ogg", "audio/ogg", expectedKind == "audio" || expectedKind == "any"
	case len(data) >= 4 && bytes.Equal(data[:4], []byte{0x1a, 0x45, 0xdf, 0xa3}):
		if expectedKind == "audio" {
			return "webm", "audio/webm", true
		}
		return "webm", "video/webm", expectedKind == "video" || expectedKind == "any"
	case len(data) >= 4 && (bytes.Equal(data[:4], []byte{0x00, 0x00, 0x01, 0xba}) || bytes.Equal(data[:4], []byte{0x00, 0x00, 0x01, 0xb3})):
		return "mpeg", "video/mpeg", expectedKind == "video" || expectedKind == "any"
	default:
		return "", "", false
	}
}

func mediaKindAllowed(mediaType, expectedKind string) bool {
	return expectedKind == "any" || strings.HasPrefix(mediaType, expectedKind+"/")
}

func requiredPluginMap(parent map[string]any, name string) (map[string]any, error) {
	value, ok := pluginMap(parent[name])
	if !ok {
		return nil, fmt.Errorf("missing or invalid %q struct", name)
	}
	return value, nil
}

func pluginMap(value any) (map[string]any, bool) {
	result, ok := value.(map[string]any)
	return result, ok
}

func requiredPluginList(parent map[string]any, name string) ([]any, error) {
	value, ok := parent[name].([]any)
	if !ok {
		return nil, fmt.Errorf("missing or invalid %q list", name)
	}
	return value, nil
}

func requiredPluginString(parent map[string]any, name string) (string, error) {
	value, ok := pluginString(parent[name])
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("missing or invalid %q string", name)
	}
	return value, nil
}

func pluginString(value any) (string, bool) {
	switch value := value.(type) {
	case string:
		return value, true
	case *string:
		if value != nil {
			return *value, true
		}
	case amazonion.SymbolToken:
		if value.Text != nil {
			return *value.Text, true
		}
	case *amazonion.SymbolToken:
		if value != nil && value.Text != nil {
			return *value.Text, true
		}
	}
	return "", false
}

func pluginNumber(value any) (float64, bool) {
	var number float64
	switch value := value.(type) {
	case int:
		number = float64(value)
	case int8:
		number = float64(value)
	case int16:
		number = float64(value)
	case int32:
		number = float64(value)
	case int64:
		number = float64(value)
	case uint:
		number = float64(value)
	case uint8:
		number = float64(value)
	case uint16:
		number = float64(value)
	case uint32:
		number = float64(value)
	case uint64:
		number = float64(value)
	case float32:
		number = float64(value)
	case float64:
		number = value
	case big.Int:
		number, _ = new(big.Float).SetInt(&value).Float64()
	case *big.Int:
		if value == nil {
			return 0, false
		}
		number, _ = new(big.Float).SetInt(value).Float64()
	case amazonion.Decimal:
		parsed, err := strconv.ParseFloat((&value).String(), 64)
		if err != nil {
			return 0, false
		}
		number = parsed
	case *amazonion.Decimal:
		if value == nil {
			return 0, false
		}
		parsed, err := strconv.ParseFloat(value.String(), 64)
		if err != nil {
			return 0, false
		}
		number = parsed
	default:
		return 0, false
	}
	return number, !math.IsNaN(number) && !math.IsInf(number, 0)
}

func pluginBoundsStyle(bounds map[string]any) (string, error) {
	type boundProperty struct {
		field string
		css   string
	}
	properties := []boundProperty{{"x", "left"}, {"y", "top"}, {"h", "height"}, {"w", "width"}}
	styles := make([]string, 0, len(properties)+1)
	positioned := false
	for _, property := range properties {
		raw, exists := bounds[property.field]
		if !exists {
			continue
		}
		value, ok := pluginMap(raw)
		if !ok {
			return "", fmt.Errorf("%q is not a struct", property.field)
		}
		number, ok := pluginNumber(value["value"])
		if !ok {
			return "", fmt.Errorf("%q has no finite numeric value", property.field)
		}
		unit, ok := pluginString(value["unit"])
		if !ok {
			return "", fmt.Errorf("%q has no unit", property.field)
		}
		unit = strings.ToLower(strings.TrimSpace(unit))
		if unit == "percent" {
			unit = "%"
		}
		switch unit {
		case "%", "px", "pt", "em", "rem", "vw", "vh":
		default:
			return "", fmt.Errorf("%q uses unsupported CSS unit %q", property.field, unit)
		}
		styles = append(styles, property.css+":"+strconv.FormatFloat(number, 'f', -1, 64)+unit)
		if property.field == "x" || property.field == "y" {
			positioned = true
		}
	}
	if positioned {
		styles = append(styles, "position:absolute")
	}
	return strings.Join(styles, ";"), nil
}

func appendPluginStyle(attributes, style string) string {
	if style == "" {
		return attributes
	}
	marker := ` style="`
	index := strings.Index(attributes, marker)
	if index < 0 {
		return attributes + marker + escapeXML(style) + `"`
	}
	valueStart := index + len(marker)
	valueEnd := strings.IndexByte(attributes[valueStart:], '"')
	if valueEnd < 0 {
		return attributes
	}
	valueEnd += valueStart
	separator := ""
	if valueEnd > valueStart && attributes[valueEnd-1] != ';' {
		separator = ";"
	}
	return attributes[:valueEnd] + separator + escapeXML(style) + attributes[valueEnd:]
}
