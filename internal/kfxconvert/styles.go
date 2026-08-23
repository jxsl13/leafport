package kfxconvert

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

type inlineStyleEvent struct {
	start int
	end   int
	class string
	href  string
}

type inlineRuby struct {
	start      int
	end        int
	annotation string
}

func (builder *epubBuilder) indexSections(sectionIDs []uint32) {
	builder.nodeSections = make(map[uint32]int)
	builder.sectionSections = make(map[uint32]int)
	builder.nodePositions = make(map[uint32]int)
	builder.sectionNodeCount = make(map[int]int)
	builder.nodeTextRunes = make(map[uint32]int)
	for index, sectionID := range sectionIDs {
		// Fixed-layout navigation commonly targets the section entity itself,
		// while reflowable navigation usually targets a node within its story.
		// Keep both forms addressable through the same section index.
		builder.sectionSections[sectionID] = index + 1
		section := builder.book.sections[sectionID]
		seen := make(map[uint32]bool)
		for _, storyID := range uniqueSymbols(ionFieldValue(section, 141), 176) {
			builder.indexStory(storyID, index+1, seen)
		}
	}
}

func (builder *epubBuilder) indexStory(storyID uint32, section int, seen map[uint32]bool) {
	if seen[storyID] {
		return
	}
	seen[storyID] = true
	story := builder.book.storylines[storyID]
	var visit func(*ionValue)
	visit = func(value *ionValue) {
		if value == nil {
			return
		}
		if value.kind == ionStruct {
			if id, ok := ionID(ionFieldValue(value, 155)); ok && id <= uint64(^uint32(0)) {
				builder.nodeSections[uint32(id)] = section
				builder.sectionNodeCount[section]++
				builder.nodePositions[uint32(id)] = builder.sectionNodeCount[section]
				if text, err := builder.book.nodeText(value); err == nil {
					builder.nodeTextRunes[uint32(id)] = len([]rune(text))
				}
			}
			// KFX content branches are mutually exclusive and ordered: direct
			// text, child content, then a named storyline. Index exactly the
			// branch the renderer uses, otherwise anchors can be assigned to a
			// section where their target is never emitted.
			if ionFieldValue(value, 145) != nil {
				return
			}
			if children := ionFieldValue(value, 146); children != nil {
				visit(children)
				return
			}
			if nested, ok := ionSymbolID(ionFieldValue(value, 176)); ok && nested <= uint64(^uint32(0)) && uint32(nested) != storyID {
				builder.indexStory(uint32(nested), section, seen)
			}
			return
		}
		for _, child := range value.children {
			visit(child)
		}
	}
	visit(ionFieldValue(story, 146))
}

func (builder *epubBuilder) nodeAttributes(node *ionValue) string {
	var attributes strings.Builder
	if id, ok := ionID(ionFieldValue(node, 155)); ok && id <= uint64(^uint32(0)) {
		attributes.WriteString(` id="kfx-node-`)
		attributes.WriteString(strconv.FormatUint(id, 10))
		attributes.WriteByte('"')
	}
	if class := styleClass(node); class != "" {
		attributes.WriteString(` class="`)
		attributes.WriteString(class)
		attributes.WriteByte('"')
	}
	if styleID, ok := ionSymbolID(ionFieldValue(node, 157)); ok && styleID <= uint64(^uint32(0)) {
		if language := builder.book.styleLanguage(uint32(styleID), make(map[uint32]bool)); validLanguageTag(language) {
			attributes.WriteString(` lang="`)
			attributes.WriteString(escapeXML(language))
			attributes.WriteString(`" xml:lang="`)
			attributes.WriteString(escapeXML(language))
			attributes.WriteByte('"')
		}
	}
	if span, ok := ionInteger(ionFieldValue(node, 148)); ok && span > 1 {
		attributes.WriteString(` colspan="`)
		attributes.WriteString(strconv.FormatInt(span, 10))
		attributes.WriteByte('"')
	}
	if span, ok := ionInteger(ionFieldValue(node, 149)); ok && span > 1 {
		attributes.WriteString(` rowspan="`)
		attributes.WriteString(strconv.FormatInt(span, 10))
		attributes.WriteByte('"')
	}
	for _, annotation := range ionListValues(ionFieldValue(node, 683)) {
		if kind, ok := ionSymbolID(ionFieldValue(annotation, 687)); !ok || kind != 584 {
			continue
		}
		if label, err := builder.book.nodeText(annotation); err == nil && strings.TrimSpace(label) != "" && label != "no accessible name found." {
			attributes.WriteString(` aria-label="`)
			attributes.WriteString(escapeXML(strings.TrimSpace(label)))
			attributes.WriteByte('"')
			break
		}
	}
	if style := directNodeStyle(node); style != "" {
		attributes.WriteString(` style="`)
		attributes.WriteString(escapeXML(style))
		attributes.WriteByte('"')
	}
	return attributes.String()
}

func directNodeStyle(node *ionValue) string {
	kind, _ := ionSymbolID(ionFieldValue(node, 159))
	if kind != 278 {
		return ""
	}
	var properties []string
	if collapse, ok := ionBoolean(ionFieldValue(node, 150)); ok {
		if collapse {
			properties = append(properties, "border-collapse:collapse")
		} else {
			properties = append(properties, "border-collapse:separate")
		}
	}
	horizontal, horizontalOK := cssDimension(ionFieldValue(node, 457))
	vertical, verticalOK := cssDimension(ionFieldValue(node, 456))
	if horizontalOK || verticalOK {
		if !horizontalOK {
			horizontal = "0"
		}
		if !verticalOK {
			vertical = "0"
		}
		properties = append(properties, "border-spacing:"+horizontal+" "+vertical)
	}
	return strings.Join(properties, ";")
}

func tableColumnGroup(table *ionValue) string {
	columns := ionListValues(ionFieldValue(table, 152))
	if len(columns) == 0 {
		return ""
	}
	var output strings.Builder
	output.WriteString("<colgroup>")
	for _, column := range columns {
		output.WriteString("<col")
		if span, ok := ionInteger(ionFieldValue(column, 118)); ok && span > 1 {
			fmt.Fprintf(&output, ` span="%d"`, span)
		}
		if width, ok := cssDimension(ionFieldValue(column, 56)); ok {
			output.WriteString(` style="width:`)
			output.WriteString(escapeXML(width))
			output.WriteByte('"')
		}
		output.WriteString("/>")
	}
	output.WriteString("</colgroup>")
	return output.String()
}

func styleClass(value *ionValue) string {
	id, ok := ionSymbolID(ionFieldValue(value, 157))
	if !ok || id > uint64(^uint32(0)) {
		return ""
	}
	return "kfx-s" + strconv.FormatUint(id, 10)
}

func (builder *epubBuilder) renderStyledText(node *ionValue, text string) (string, error) {
	runes := []rune(text)
	eventsValue := ionFieldValue(node, 142)
	anchorIDs := builder.inlineTextAnchors(node, len(runes))
	if ((eventsValue == nil || eventsValue.kind != ionList) && len(anchorIDs) == 0) || len(runes) == 0 {
		content := escapeText(text)
		if href := builder.linkTarget(ionFieldValue(node, 179)); href != "" {
			return `<a href="` + escapeXML(href) + `">` + content + `</a>`, nil
		}
		return content, nil
	}
	eventValues := []*ionValue(nil)
	if eventsValue != nil && eventsValue.kind == ionList {
		eventValues = eventsValue.children
	}
	events := make([]inlineStyleEvent, 0, len(eventValues))
	var rubies []inlineRuby
	for _, item := range eventValues {
		offset, offsetOK := ionInteger(ionFieldValue(item, 143))
		length, lengthOK := ionInteger(ionFieldValue(item, 144))
		if !offsetOK || !lengthOK || offset < 0 || length <= 0 || offset >= int64(len(runes)) {
			continue
		}
		end := offset + length
		if end > int64(len(runes)) {
			end = int64(len(runes))
		}
		class := styleClass(item)
		if _, dropcap := ionInteger(ionFieldValue(item, 125)); dropcap {
			class = strings.TrimSpace(class + " kfx-dropcap")
		}
		event := inlineStyleEvent{
			start: int(offset), end: int(end), class: class,
			href: builder.linkTarget(ionFieldValue(item, 179)),
		}
		if ionFieldValue(item, 757) != nil {
			ruby, err := builder.rubySegments(item, event.start, event.end)
			if err != nil {
				return "", err
			}
			rubies = append(rubies, ruby...)
		}
		if event.class != "" || event.href != "" {
			events = append(events, event)
		}
	}
	if len(events) == 0 && len(rubies) == 0 && len(anchorIDs) == 0 {
		return escapeText(text), nil
	}
	sort.Slice(rubies, func(i, j int) bool {
		if rubies[i].start == rubies[j].start {
			return rubies[i].end < rubies[j].end
		}
		return rubies[i].start < rubies[j].start
	})
	var output strings.Builder
	cursor := 0
	for _, ruby := range rubies {
		if ruby.start < cursor || ruby.end <= ruby.start || ruby.end > len(runes) {
			return "", fmt.Errorf("overlapping or invalid ruby range %d:%d", ruby.start, ruby.end)
		}
		output.WriteString(renderInlineRange(runes, cursor, ruby.start, events, anchorIDs))
		base := renderInlineRange(runes, ruby.start, ruby.end, events, anchorIDs)
		output.WriteString(`<ruby><rb>` + base + `</rb><rt>` + escapeText(ruby.annotation) + `</rt></ruby>`)
		cursor = ruby.end
	}
	output.WriteString(renderInlineRange(runes, cursor, len(runes), events, anchorIDs))
	for _, anchorID := range anchorIDs[len(runes)] {
		fmt.Fprintf(&output, `<span id="kfx-anchor-%d"></span>`, anchorID)
	}
	return output.String(), nil
}

func renderInlineRange(runes []rune, rangeStart, rangeEnd int, events []inlineStyleEvent, anchorIDs map[int][]uint32) string {
	if rangeStart >= rangeEnd {
		return ""
	}
	boundaries := []int{rangeStart, rangeEnd}
	for offset := range anchorIDs {
		if offset >= rangeStart && offset < rangeEnd {
			boundaries = append(boundaries, offset)
		}
	}
	for _, event := range events {
		if event.start > rangeStart && event.start < rangeEnd {
			boundaries = append(boundaries, event.start)
		}
		if event.end > rangeStart && event.end < rangeEnd {
			boundaries = append(boundaries, event.end)
		}
	}
	sort.Ints(boundaries)
	boundaries = uniqueInts(boundaries)
	var output strings.Builder
	for index := 0; index+1 < len(boundaries); index++ {
		start, end := boundaries[index], boundaries[index+1]
		for _, anchorID := range anchorIDs[start] {
			fmt.Fprintf(&output, `<span id="kfx-anchor-%d"></span>`, anchorID)
		}
		fragment := escapeText(string(runes[start:end]))
		var active []inlineStyleEvent
		for _, event := range events {
			if event.start <= start && event.end >= end {
				active = append(active, event)
			}
		}
		sort.SliceStable(active, func(i, j int) bool {
			if active[i].start == active[j].start {
				return active[i].end > active[j].end
			}
			return active[i].start < active[j].start
		})
		linkUsed := false
		for item := len(active) - 1; item >= 0; item-- {
			event := active[item]
			if event.class != "" {
				fragment = `<span class="` + event.class + `">` + fragment + `</span>`
			}
			if event.href != "" && !linkUsed {
				fragment = `<a href="` + escapeXML(event.href) + `">` + fragment + `</a>`
				linkUsed = true
			}
		}
		output.WriteString(fragment)
	}
	return output.String()
}

func (builder *epubBuilder) rubySegments(event *ionValue, eventStart, eventEnd int) ([]inlineRuby, error) {
	rubyName, ok := ionSymbolID(ionFieldValue(event, 757))
	if !ok || rubyName > uint64(^uint32(0)) {
		return nil, errors.New("ruby style event has no valid content fragment")
	}
	type rubyRange struct {
		start int
		end   int
		id    uint32
	}
	var ranges []rubyRange
	if rubyID, ok := ionID(ionFieldValue(event, 758)); ok && rubyID <= uint64(^uint32(0)) {
		ranges = append(ranges, rubyRange{start: eventStart, end: eventEnd, id: uint32(rubyID)})
	} else {
		for _, item := range ionListValues(ionFieldValue(event, 759)) {
			offset, offsetOK := ionInteger(ionFieldValue(item, 143))
			length, lengthOK := ionInteger(ionFieldValue(item, 144))
			rubyID, idOK := ionID(ionFieldValue(item, 758))
			if !offsetOK || !lengthOK || !idOK || offset < 0 || length <= 0 || rubyID > uint64(^uint32(0)) {
				return nil, errors.New("ruby style event has an invalid range")
			}
			ranges = append(ranges, rubyRange{
				start: eventStart + int(offset), end: eventStart + int(offset+length), id: uint32(rubyID),
			})
		}
	}
	if len(ranges) == 0 {
		return nil, errors.New("ruby style event has no annotation IDs")
	}
	result := make([]inlineRuby, 0, len(ranges))
	expected := eventStart
	for _, item := range ranges {
		if item.start != expected || item.end > eventEnd {
			return nil, fmt.Errorf("ruby ranges do not exactly cover style event %d:%d", eventStart, eventEnd)
		}
		annotation, err := builder.rubyAnnotation(uint32(rubyName), item.id)
		if err != nil {
			return nil, err
		}
		result = append(result, inlineRuby{start: item.start, end: item.end, annotation: annotation})
		expected = item.end
	}
	if expected != eventEnd {
		return nil, fmt.Errorf("ruby ranges end at %d; expected %d", expected, eventEnd)
	}
	return result, nil
}

func (builder *epubBuilder) rubyAnnotation(fragmentID, rubyID uint32) (string, error) {
	fragment := builder.book.entities[756][fragmentID]
	if fragment == nil {
		return "", fmt.Errorf("missing ruby-content fragment $%d", fragmentID)
	}
	for _, candidate := range ionListValues(ionFieldValue(fragment, 146)) {
		if templateID, ok := ionSymbolID(candidate); ok && templateID <= uint64(^uint32(0)) {
			candidate = builder.book.templates[uint32(templateID)]
		}
		id, ok := ionID(ionFieldValue(candidate, 758))
		if !ok || id != uint64(rubyID) {
			continue
		}
		text := strings.TrimSpace(builder.collectAnnotationText(candidate, make(map[*ionValue]bool)))
		if text == "" {
			return "", fmt.Errorf("ruby annotation $%d in fragment $%d is empty", rubyID, fragmentID)
		}
		return text, nil
	}
	return "", fmt.Errorf("missing ruby annotation $%d in fragment $%d", rubyID, fragmentID)
}

func (builder *epubBuilder) collectAnnotationText(value *ionValue, seen map[*ionValue]bool) string {
	if value == nil || seen[value] {
		return ""
	}
	seen[value] = true
	if value.kind == ionSymbol && value.unsigned <= uint64(^uint32(0)) {
		return builder.collectAnnotationText(builder.book.templates[uint32(value.unsigned)], seen)
	}
	if value.kind == ionString {
		return builder.book.redactText(value.text)
	}
	if value.kind == ionStruct {
		if ionFieldValue(value, 145) != nil {
			text, _ := builder.book.nodeText(value)
			return text
		}
		if children := ionFieldValue(value, 146); children != nil {
			return builder.collectAnnotationText(children, seen)
		}
		if storyID, ok := ionSymbolID(ionFieldValue(value, 176)); ok && storyID <= uint64(^uint32(0)) {
			return builder.collectAnnotationText(ionFieldValue(builder.book.storylines[uint32(storyID)], 146), seen)
		}
	}
	var output strings.Builder
	for _, child := range value.children {
		output.WriteString(builder.collectAnnotationText(child, seen))
	}
	return output.String()
}

func (builder *epubBuilder) inlineTextAnchors(node *ionValue, textLength int) map[int][]uint32 {
	nodeID, ok := ionID(ionFieldValue(node, 155))
	if !ok || nodeID > uint64(^uint32(0)) || builder.book == nil {
		return nil
	}
	result := make(map[int][]uint32)
	for anchorID, item := range builder.book.anchors {
		if !item.targetSymbol && item.targetNode == uint32(nodeID) && item.offset > 0 && item.offset <= textLength {
			result[item.offset] = append(result[item.offset], anchorID)
		}
	}
	for offset := range result {
		sort.Slice(result[offset], func(i, j int) bool { return result[offset][i] < result[offset][j] })
	}
	return result
}

func uniqueInts(values []int) []int {
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func (builder *epubBuilder) linkTarget(value *ionValue) string {
	if raw, ok := ionText(value); ok {
		if builder.book.privacy != nil && builder.book.privacy.matches(raw) {
			return ""
		}
		return safeExternalURL(raw)
	}
	id, ok := ionSymbolID(value)
	if !ok || id > uint64(^uint32(0)) {
		return ""
	}
	anchor, ok := builder.book.anchors[uint32(id)]
	if !ok {
		return ""
	}
	if anchor.externalURL != "" {
		return safeExternalURL(anchor.externalURL)
	}
	section := builder.nodeSections[anchor.targetNode]
	if anchor.targetSymbol {
		section = builder.sectionSections[anchor.targetNode]
	}
	if section == 0 {
		return ""
	}
	if anchor.targetSymbol {
		return fmt.Sprintf("section-%04d.xhtml", section)
	}
	if anchor.offset > 0 && anchor.offset <= builder.nodeTextRunes[anchor.targetNode] {
		return fmt.Sprintf("section-%04d.xhtml#kfx-anchor-%d", section, id)
	}
	return fmt.Sprintf("section-%04d.xhtml#kfx-node-%d", section, anchor.targetNode)
}

func (builder *epubBuilder) navigationItems() []epubNavigationItem {
	root := builder.book.navigation
	if root == nil || root.kind != ionList {
		return nil
	}
	for _, readingOrder := range root.children {
		containers := ionFieldValue(readingOrder, 392)
		if containers == nil || containers.kind != ionList {
			continue
		}
		for _, container := range containers.children {
			kind, ok := ionSymbolID(ionFieldValue(container, 235))
			if !ok || kind != 212 {
				continue
			}
			return builder.parseNavigationEntries(ionFieldValue(container, 247))
		}
	}
	return nil
}

func (builder *epubBuilder) parseNavigationEntries(value *ionValue) []epubNavigationItem {
	if value == nil || value.kind != ionList {
		return nil
	}
	var result []epubNavigationItem
	for _, entry := range value.children {
		label, _ := ionText(ionFieldValue(ionFieldValue(entry, 241), 244))
		label = builder.book.redactText(label)
		position := ionFieldValue(entry, 246)
		targetValue := ionFieldValue(position, 155)
		target, targetOK := ionID(targetValue)
		section := 0
		positionInSection := 0
		targetsSection := false
		if targetOK && target <= uint64(^uint32(0)) {
			_, targetsSection = ionSymbolID(targetValue)
			if targetsSection {
				section = builder.sectionSections[uint32(target)]
			} else {
				section = builder.nodeSections[uint32(target)]
				positionInSection = builder.nodePositions[uint32(target)]
			}
		}
		children := builder.parseNavigationEntries(ionFieldValue(entry, 247))
		if strings.TrimSpace(label) == "" || section == 0 {
			result = append(result, children...)
			continue
		}
		href := fmt.Sprintf("text/section-%04d.xhtml", section)
		if !targetsSection {
			href += fmt.Sprintf("#kfx-node-%d", target)
		}
		result = append(result, epubNavigationItem{
			label:    shortNavigationTitle(label, 120),
			href:     href,
			section:  section,
			position: positionInSection,
			children: children,
		})
	}
	// EPUB navigation targets must occur in reading order. KFX navigation
	// containers occasionally list front matter out of spine order, so derive
	// the portable order from the rendered section and node positions.
	sort.SliceStable(result, func(left, right int) bool {
		if result[left].section != result[right].section {
			return result[left].section < result[right].section
		}
		return result[left].position < result[right].position
	})
	return result
}

func safeExternalURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "mailto", "tel":
		return parsed.String()
	default:
		return ""
	}
}

func (builder *epubBuilder) styleSheet() string {
	ids := make([]int, 0, len(builder.book.styles))
	for id := range builder.book.styles {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	var css strings.Builder
	css.WriteString(defaultEPUBCSS)
	for _, face := range builder.fontFaces {
		css.WriteString("@font-face {font-family:")
		css.WriteString(cssString(face.family))
		css.WriteString(";src:url(")
		css.WriteString(cssString(face.path))
		css.WriteString(");")
		if face.style != "" {
			fmt.Fprintf(&css, "font-style:%s;", face.style)
		}
		if face.weight != "" {
			fmt.Fprintf(&css, "font-weight:%s;", face.weight)
		}
		if face.stretch != "" {
			fmt.Fprintf(&css, "font-stretch:%s;", face.stretch)
		}
		css.WriteString("}\n")
	}
	for _, rawID := range ids {
		id := uint32(rawID)
		properties := builder.book.styleProperties(id, make(map[uint32]bool))
		if len(properties) == 0 {
			continue
		}
		names := make([]string, 0, len(properties))
		for name := range properties {
			names = append(names, name)
		}
		sort.Strings(names)
		fmt.Fprintf(&css, ".kfx-s%d {", id)
		for _, name := range names {
			fmt.Fprintf(&css, "%s:%s;", name, properties[name])
		}
		css.WriteString("}\n")
	}
	return css.String()
}

func (book *decodedBook) styleProperties(id uint32, stack map[uint32]bool) map[string]string {
	if stack[id] {
		return nil
	}
	stack[id] = true
	defer delete(stack, id)
	style := book.styles[id]
	properties := make(map[string]string)
	if parent, ok := ionSymbolID(ionFieldValue(style, 158)); ok && parent <= uint64(^uint32(0)) {
		for name, value := range book.styleProperties(uint32(parent), stack) {
			properties[name] = value
		}
	}
	setDimension := func(field uint64, name string) {
		if value, ok := cssDimension(ionFieldValue(style, field)); ok {
			properties[name] = value
		}
	}
	if family, ok := ionText(ionFieldValue(style, 11)); ok {
		if family = safeFontFamily(family); family != "" {
			properties["font-family"] = family
		}
	}
	if value, ok := ionSymbolID(ionFieldValue(style, 12)); ok {
		if mapped := map[uint64]string{350: "normal", 381: "oblique", 382: "italic"}[value]; mapped != "" {
			properties["font-style"] = mapped
		}
	}
	if value, ok := ionSymbolID(ionFieldValue(style, 13)); ok {
		if mapped := map[uint64]string{350: "normal", 355: "100", 356: "200", 357: "300", 358: "400", 359: "500", 360: "600", 361: "700", 362: "800", 363: "900"}[value]; mapped != "" {
			properties["font-weight"] = mapped
		}
	}
	if value, ok := ionSymbolID(ionFieldValue(style, 15)); ok {
		if mapped := map[uint64]string{350: "normal", 365: "condensed", 366: "semi-condensed", 367: "semi-expanded", 368: "expanded"}[value]; mapped != "" {
			properties["font-stretch"] = mapped
		}
	}
	setDimension(16, "font-size")
	setDimension(32, "letter-spacing")
	setDimension(36, "text-indent")
	setDimension(39, "margin-top")
	setDimension(40, "margin-bottom")
	setDimension(42, "line-height")
	for field, name := range map[uint64]string{
		47: "margin-top", 48: "margin-left", 49: "margin-bottom", 50: "margin-right",
		51: "padding", 52: "padding-top", 53: "padding-left", 54: "padding-bottom", 55: "padding-right",
		56: "width", 57: "height", 62: "min-height", 63: "min-width", 64: "max-height", 65: "max-width",
		93: "border-width", 94: "border-top-width", 95: "border-left-width", 96: "border-bottom-width", 97: "border-right-width",
		459: "border-top-left-radius", 460: "border-top-right-radius",
		461: "border-bottom-left-radius", 462: "border-bottom-right-radius",
	} {
		setDimension(field, name)
	}
	if value, ok := ionSymbolID(ionFieldValue(style, 34)); ok {
		if mapped := map[uint64]string{59: "left", 61: "right", 320: "center", 321: "justify"}[value]; mapped != "" {
			properties["text-align"] = mapped
		}
	}
	if value, ok := ionSymbolID(ionFieldValue(style, 41)); ok {
		if mapped := map[uint64]string{350: "none", 372: "uppercase", 373: "lowercase", 374: "capitalize"}[value]; mapped != "" {
			properties["text-transform"] = mapped
		}
	}
	if value, ok := ionSymbolID(ionFieldValue(style, 31)); ok {
		if mapped := map[uint64]string{370: "super", 371: "sub"}[value]; mapped != "" {
			properties["vertical-align"] = mapped
		}
	}
	if value, ok := ionSymbolID(ionFieldValue(style, 44)); ok {
		if mapped := map[uint64]string{60: "bottom", 320: "middle", 350: "baseline", 371: "sub", 370: "super", 449: "text-bottom", 447: "text-top", 58: "top"}[value]; mapped != "" {
			properties["vertical-align"] = mapped
		}
	}
	decorations := make([]string, 0, 2)
	if value, ok := ionSymbolID(ionFieldValue(style, 23)); ok {
		if mapped := textDecoration(value, "underline"); mapped != "" {
			decorations = append(decorations, mapped)
		}
	}
	if value, ok := ionSymbolID(ionFieldValue(style, 27)); ok {
		if mapped := textDecoration(value, "line-through"); mapped != "" {
			decorations = append(decorations, mapped)
		}
	}
	if value, ok := ionSymbolID(ionFieldValue(style, 554)); ok {
		if mapped := textDecoration(value, "overline"); mapped != "" {
			decorations = append(decorations, mapped)
		}
	}
	if len(decorations) != 0 {
		properties["text-decoration"] = strings.Join(decorations, " ")
	}
	if color, ok := cssColor(ionFieldValue(style, 19)); ok {
		properties["color"] = color
	}
	if color, ok := cssColor(ionFieldValue(style, 21)); ok {
		properties["background-color"] = color
	}
	if color, ok := cssColor(ionFieldValue(style, 70)); ok {
		properties["background-color"] = color
	}
	if opacity, ok := ionNumber(ionFieldValue(style, 20)); ok && opacity >= 0 && opacity <= 1 {
		properties["opacity"] = strconv.FormatFloat(opacity, 'g', 4, 64)
	}
	if value, ok := ionSymbolID(ionFieldValue(style, 88)); ok {
		if mapped := borderStyle(value); mapped != "" {
			properties["border-style"] = mapped
		}
	}
	if color, ok := cssColor(ionFieldValue(style, 83)); ok {
		properties["border-color"] = color
	}
	for field, name := range map[uint64]string{
		84: "border-top-color", 85: "border-left-color", 86: "border-bottom-color", 87: "border-right-color",
	} {
		if color, ok := cssColor(ionFieldValue(style, field)); ok {
			properties[name] = color
		}
	}
	for field, name := range map[uint64]string{
		89: "border-top-style", 90: "border-left-style", 91: "border-bottom-style", 92: "border-right-style",
	} {
		if value, ok := ionSymbolID(ionFieldValue(style, field)); ok {
			if mapped := borderStyle(value); mapped != "" {
				properties[name] = mapped
			}
		}
	}
	for field, name := range map[uint64]string{133: "break-after", 134: "break-before", 135: "break-inside"} {
		if value, ok := ionSymbolID(ionFieldValue(style, field)); ok {
			if mapped := map[uint64]string{352: "page", 383: "auto", 353: "avoid"}[value]; mapped != "" {
				properties[name] = mapped
			}
		}
	}
	if value, ok := ionSymbolID(ionFieldValue(style, 140)); ok {
		if mapped := map[uint64]string{59: "left", 61: "right"}[value]; mapped != "" {
			properties["float"] = mapped
		}
	}
	if value, ok := ionSymbolID(ionFieldValue(style, 583)); ok {
		if mapped := map[uint64]string{349: "normal", 369: "small-caps"}[value]; mapped != "" {
			properties["font-variant"] = mapped
		}
	}
	if value, ok := ionSymbolID(ionFieldValue(style, 546)); ok {
		if mapped := map[uint64]string{378: "border-box", 377: "content-box", 379: "padding-box"}[value]; mapped != "" {
			properties["box-sizing"] = mapped
		}
	}
	if value, ok := ionSymbolID(ionFieldValue(style, 633)); ok {
		if mapped := map[uint64]string{350: "baseline", 60: "bottom", 320: "middle", 58: "top"}[value]; mapped != "" {
			properties["vertical-align"] = mapped
		}
	}
	if value, ok := ionSymbolID(ionFieldValue(style, 580)); ok {
		switch value {
		case 59:
			properties["margin-right"] = "auto"
		case 61:
			properties["margin-left"] = "auto"
		case 320:
			properties["margin-left"] = "auto"
			properties["margin-right"] = "auto"
		}
	}
	if shadow, ok := cssBoxShadow(ionFieldValue(style, 496)); ok {
		properties["box-shadow"] = shadow
	}
	if link, linkOK := nestedStyleColor(ionFieldValue(style, 577)); linkOK {
		if visited, visitedOK := nestedStyleColor(ionFieldValue(style, 576)); visitedOK && visited == link {
			properties["color"] = link
		}
	}
	if value, ok := ionSymbolID(ionFieldValue(style, 100)); ok {
		if mapped := listStyle(value); mapped != "" {
			properties["list-style-type"] = mapped
		}
	}
	return properties
}

func cssDimension(value *ionValue) (string, bool) {
	if value == nil || value.kind != ionStruct {
		return "", false
	}
	unit, unitOK := ionSymbolID(ionFieldValue(value, 306))
	number, numberOK := ionNumber(ionFieldValue(value, 307))
	if !unitOK || !numberOK {
		return "", false
	}
	suffix := map[uint64]string{
		308: "em", 309: "ex", 310: "em", 311: "vw", 312: "vh", 313: "vmin",
		314: "%", 315: "cm", 316: "mm", 317: "in", 318: "pt", 319: "px",
		505: "rem", 506: "ch", 507: "vmax",
	}[unit]
	if suffix == "" {
		return "", false
	}
	return strconv.FormatFloat(number, 'f', -1, 64) + suffix, true
}

func cssColor(value *ionValue) (string, bool) {
	number, ok := ionUint(value)
	if !ok || number > uint64(^uint32(0)) {
		return "", false
	}
	alpha := byte(number >> 24)
	if alpha < 2 {
		return fmt.Sprintf("rgba(%d,%d,%d,0)", byte(number>>16), byte(number>>8), byte(number)), true
	}
	if alpha > 253 {
		return fmt.Sprintf("#%06x", number&0xffffff), true
	}
	opacity := float64(uint16(alpha)+1) / 256
	return fmt.Sprintf("rgba(%d,%d,%d,%s)", byte(number>>16), byte(number>>8), byte(number),
		strconv.FormatFloat(opacity, 'f', 3, 64)), true
}

func nestedStyleColor(value *ionValue) (string, bool) {
	return cssColor(ionFieldValue(value, 19))
}

func cssBoxShadow(value *ionValue) (string, bool) {
	if value == nil || value.kind != ionStruct {
		return "", false
	}
	var parts []string
	for _, field := range []uint64{499, 500, 501, 502} {
		if dimension, ok := cssDimension(ionFieldValue(value, field)); ok {
			parts = append(parts, dimension)
		} else if field <= 500 {
			return "", false
		}
	}
	if color, ok := cssColor(ionFieldValue(value, 498)); ok {
		parts = append(parts, color)
	}
	if inset, ok := ionBoolean(ionFieldValue(value, 336)); ok && inset {
		parts = append(parts, "inset")
	}
	return strings.Join(parts, " "), len(parts) >= 2
}

func textDecoration(value uint64, line string) string {
	switch value {
	case 328:
		return line
	case 329:
		return line + " double"
	case 330:
		return line + " dashed"
	case 331:
		return line + " dotted"
	default:
		return ""
	}
}

func (book *decodedBook) styleLanguage(id uint32, seen map[uint32]bool) string {
	if seen[id] {
		return ""
	}
	seen[id] = true
	style := book.styles[id]
	if language, ok := ionText(ionFieldValue(style, 10)); ok {
		return strings.TrimSpace(language)
	}
	if parent, ok := ionSymbolID(ionFieldValue(style, 158)); ok && parent <= uint64(^uint32(0)) {
		return book.styleLanguage(uint32(parent), seen)
	}
	return ""
}

func validLanguageTag(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if !(unicode.IsLetter(character) || unicode.IsDigit(character) || character == '-') {
			return false
		}
	}
	return true
}

func (book *decodedBook) styleHasLayoutHint(id uint32, hint uint64, seen map[uint32]bool) bool {
	if seen[id] {
		return false
	}
	seen[id] = true
	style := book.styles[id]
	for _, value := range ionListValues(ionFieldValue(style, 761)) {
		if symbol, ok := ionSymbolID(value); ok && symbol == hint {
			return true
		}
	}
	if parent, ok := ionSymbolID(ionFieldValue(style, 158)); ok && parent <= uint64(^uint32(0)) {
		return book.styleHasLayoutHint(uint32(parent), hint, seen)
	}
	return false
}

func safeFontFamily(value string) string {
	var result strings.Builder
	for _, character := range value {
		if unicode.IsLetter(character) || unicode.IsDigit(character) ||
			strings.ContainsRune(" -_,\"'", character) {
			result.WriteRune(character)
		}
	}
	parts := strings.Split(result.String(), ",")
	filtered := parts[:0]
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" && part != "default" {
			filtered = append(filtered, part)
		}
	}
	return strings.Join(filtered, ", ")
}

func cssString(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	value = strings.NewReplacer("\r", " ", "\n", " ", "\f", " ").Replace(value)
	return `"` + value + `"`
}

func borderStyle(value uint64) string {
	return map[uint64]string{
		328: "solid", 329: "double", 330: "dashed", 331: "dotted", 334: "groove",
		335: "ridge", 336: "inset", 337: "outset", 349: "none",
	}[value]
}

func listStyle(value uint64) string {
	return map[uint64]string{
		340: "disc", 341: "square", 342: "circle", 343: "decimal", 344: "lower-roman",
		345: "upper-roman", 346: "lower-alpha", 347: "upper-alpha", 349: "none",
	}[value]
}
