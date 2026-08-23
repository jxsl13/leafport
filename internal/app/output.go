package app

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	batchOutputWidth = 96
	detailIndent     = "        "
	detailLabelWidth = 8
)

// batchReporter keeps progress readable without relying on terminal colors or
// cursor control, so redirected output remains useful too.
type batchReporter struct {
	output  io.Writer
	started bool
}

func (reporter *batchReporter) begin(index, total int, title, id string) {
	if reporter.started {
		fmt.Fprintln(reporter.output)
	}
	reporter.started = true
	prefix := fmt.Sprintf("[%d/%d] ", index, total)
	writeWrapped(reporter.output, prefix, strings.Repeat(" ", utf8.RuneCountInString(prefix)), title, batchOutputWidth)
	reporter.field("ID", id)
}

func (reporter *batchReporter) field(label, value string) {
	prefix := fmt.Sprintf("%s%-*s ", detailIndent, detailLabelWidth, label)
	continuation := strings.Repeat(" ", utf8.RuneCountInString(prefix))
	writeWrapped(reporter.output, prefix, continuation, value, batchOutputWidth)
}

func (reporter *batchReporter) finish() {
	if reporter.started {
		fmt.Fprintln(reporter.output)
	}
}

func writeWrapped(output io.Writer, prefix, continuation, value string, width int) {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		fmt.Fprintln(output, prefix)
		return
	}
	firstWidth := max(1, width-utf8.RuneCountInString(prefix))
	continuationWidth := max(1, width-utf8.RuneCountInString(continuation))
	lines := wrapWords(value, firstWidth, continuationWidth)
	for index, line := range lines {
		if index == 0 {
			fmt.Fprintln(output, prefix+line)
		} else {
			fmt.Fprintln(output, continuation+line)
		}
	}
}

func wrapWords(value string, firstWidth, continuationWidth int) []string {
	words := strings.Fields(value)
	if len(words) == 0 {
		return nil
	}
	var lines []string
	current := ""
	limit := firstWidth
	for _, word := range words {
		for utf8.RuneCountInString(word) > limit {
			if current != "" {
				lines = append(lines, current)
				current = ""
				limit = continuationWidth
				continue
			}
			head, tail := splitLongWord(word, limit)
			lines = append(lines, head)
			word = tail
			limit = continuationWidth
		}
		candidate := word
		if current != "" {
			candidate = current + " " + word
		}
		if utf8.RuneCountInString(candidate) <= limit {
			current = candidate
			continue
		}
		lines = append(lines, current)
		current = word
		limit = continuationWidth
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}

func splitLongWord(word string, limit int) (string, string) {
	runes := []rune(word)
	cut := limit
	// Paths and identifiers remain easier to scan when wrapping happens at a
	// component or word boundary. Avoid a very short line just to use one.
	for index := min(limit, len(runes)); index > limit/2; index-- {
		switch runes[index-1] {
		case '/', '\\', '-', '_':
			cut = index
			index = 0
		}
	}
	return string(runes[:cut]), string(runes[cut:])
}
