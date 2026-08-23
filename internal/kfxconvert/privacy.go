package kfxconvert

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// PrivacyOptions enables removal of personal information during
// reconstruction. Patterns use Go regular-expression syntax and are applied
// to user-visible text and publication metadata.
type PrivacyOptions struct {
	DetectPersonal bool
	Patterns       []string
	compiled       []*regexp.Regexp
}

// PrivacyResult reports privacy work without exposing the values found.
type PrivacyResult struct {
	Enabled         bool
	AutomaticValues int
	MetadataFields  int
}

type personalRedactor struct {
	patterns []*regexp.Regexp
}

var metadataEmailPattern = regexp.MustCompile(`(?i)[a-z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+`)

// ValidatePrivacyOptions validates patterns before any output is created.
func ValidatePrivacyOptions(options PrivacyOptions) error {
	_, err := compiledPrivacyPatterns(options)
	return err
}

// PreparePrivacyOptions validates and compiles explicit patterns once for a
// multi-book conversion. The returned value remains safe to pass by value.
func PreparePrivacyOptions(options PrivacyOptions) (PrivacyOptions, error) {
	options.Patterns = append([]string(nil), options.Patterns...)
	compiled, err := compilePrivacyPatterns(options.Patterns)
	if err != nil {
		return PrivacyOptions{}, err
	}
	options.compiled = compiled
	return options, nil
}

func compilePrivacyPatterns(patterns []string) ([]*regexp.Regexp, error) {
	compiledPatterns := make([]*regexp.Regexp, 0, len(patterns))
	for index, pattern := range patterns {
		if strings.TrimSpace(pattern) == "" {
			return nil, fmt.Errorf("--redact pattern %d is empty", index+1)
		}
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid --redact pattern %d: %w", index+1, err)
		}
		if compiled.MatchString("") {
			return nil, fmt.Errorf("--redact pattern %d matches empty text", index+1)
		}
		compiledPatterns = append(compiledPatterns, compiled)
	}
	return compiledPatterns, nil
}

func compiledPrivacyPatterns(options PrivacyOptions) ([]*regexp.Regexp, error) {
	if len(options.compiled) == len(options.Patterns) {
		matches := true
		for index, compiled := range options.compiled {
			if compiled == nil || compiled.String() != options.Patterns[index] {
				matches = false
				break
			}
		}
		if matches {
			return options.compiled, nil
		}
	}
	return compilePrivacyPatterns(options.Patterns)
}

// RedactText applies explicit patterns and contextual automatic email
// detection to text that lives outside the KFX container, such as a library
// title used for an output file name. A bare email is not assumed to identify
// the owner because it can legitimately belong to an author or publisher.
func RedactText(value string, options PrivacyOptions) (string, error) {
	patterns, err := compiledPrivacyPatterns(options)
	if err != nil {
		return "", err
	}
	redactor := &personalRedactor{patterns: append([]*regexp.Regexp(nil), patterns...)}
	if options.DetectPersonal && hasPersonalContext(value) {
		seen := make(map[string]struct{})
		for _, email := range metadataEmailPattern.FindAllString(value, -1) {
			if _, exists := seen[strings.ToLower(email)]; exists {
				continue
			}
			seen[strings.ToLower(email)] = struct{}{}
			redactor.patterns = append(redactor.patterns,
				regexp.MustCompile(`(?i:`+regexp.QuoteMeta(email)+`)`))
		}
	}
	return redactor.redact(value), nil
}

func applyPrivacy(book *decodedBook, options PrivacyOptions) (PrivacyResult, error) {
	result := PrivacyResult{Enabled: options.DetectPersonal || len(options.Patterns) != 0}
	if !result.Enabled {
		return result, nil
	}
	patterns, err := compiledPrivacyPatterns(options)
	if err != nil {
		return PrivacyResult{}, err
	}

	automatic := make(map[string]struct{})
	if options.DetectPersonal {
		for category, entries := range book.metadata {
			for key, values := range entries {
				personalField := personalMetadataField(category, key)
				for _, value := range values {
					if personalField || hasPersonalContext(value) {
						for _, email := range metadataEmailPattern.FindAllString(value, -1) {
							addAutomaticValue(automatic, email)
						}
					}
					if personalField {
						addAutomaticValue(automatic, value)
					}
				}
				if personalField {
					delete(entries, key)
					delete(book.metadataRaw[category], key)
					result.MetadataFields++
				}
			}
		}
	}
	values := make([]string, 0, len(automatic))
	for value := range automatic {
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	redactor := &personalRedactor{patterns: append([]*regexp.Regexp(nil), patterns...)}
	for _, value := range values {
		redactor.patterns = append(redactor.patterns,
			regexp.MustCompile(`(?i:`+regexp.QuoteMeta(value)+`)`))
	}
	book.privacy = redactor
	result.AutomaticValues = len(values)

	for id, values := range book.contents {
		for index, value := range values {
			book.contents[id][index] = redactor.redact(value)
		}
	}
	for category, entries := range book.metadata {
		for key, values := range entries {
			for index, value := range values {
				book.metadata[category][key][index] = redactor.redact(value)
			}
			for _, raw := range book.metadataRaw[category][key] {
				if raw != nil && raw.kind == ionString {
					raw.text = redactor.redact(raw.text)
				}
			}
		}
	}
	for id, item := range book.anchors {
		if redactor.matches(item.externalURL) {
			item.externalURL = ""
			book.anchors[id] = item
		}
	}
	return result, nil
}

func addAutomaticValue(values map[string]struct{}, value string) {
	value = strings.TrimSpace(value)
	if len([]rune(value)) < 3 || len(value) > 512 {
		return
	}
	values[value] = struct{}{}
}

func hasPersonalContext(value string) bool {
	value = strings.ToLower(value)
	for _, phrase := range []string{
		"licensed to", "licensed for", "registered to", "delivered to",
		"personalized for", "personalised for", "prepared for",
		"purchased by", "bought by",
		"lizenziert für", "registriert auf", "bereitgestellt für",
		"personalisiert für", "gekauft von",
	} {
		if strings.Contains(value, phrase) {
			return true
		}
	}
	return false
}

func personalMetadataField(category, key string) bool {
	category = normalizeMetadataName(category)
	key = normalizeMetadataName(key)
	for _, prefix := range []string{"customer_", "account_", "owner_", "purchaser_", "buyer_", "registered_user_", "watermark_"} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	switch key {
	case "customer", "account", "owner", "purchaser", "buyer", "registered_user",
		"customer_id", "account_id", "owner_id", "user_id", "email", "email_address":
		return true
	}
	category = "_" + category + "_"
	for _, token := range []string{"personalization", "personalisation", "customer", "account", "purchaser", "watermark"} {
		if strings.Contains(category, "_"+token+"_") {
			return true
		}
	}
	return false
}

func normalizeMetadataName(value string) string {
	var output strings.Builder
	underscore := false
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			output.WriteRune(r)
			underscore = false
		} else if !underscore {
			output.WriteByte('_')
			underscore = true
		}
	}
	return strings.Trim(output.String(), "_")
}

func (redactor *personalRedactor) redact(value string) string {
	if redactor == nil {
		return value
	}
	for _, pattern := range redactor.patterns {
		value = pattern.ReplaceAllStringFunc(value, redactRunes)
	}
	return value
}

func (redactor *personalRedactor) matches(value string) bool {
	if redactor == nil || value == "" {
		return false
	}
	for _, pattern := range redactor.patterns {
		if pattern.MatchString(value) {
			return true
		}
	}
	return false
}

func redactRunes(value string) string {
	var output strings.Builder
	for _, r := range value {
		if unicode.IsSpace(r) {
			output.WriteRune(r)
		} else {
			output.WriteRune('█')
		}
	}
	return output.String()
}

func (book *decodedBook) redactText(value string) string {
	if book == nil || book.privacy == nil {
		return value
	}
	return book.privacy.redact(value)
}
