package kfxconvert

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

const mathMLNamespace = "http://www.w3.org/1998/Math/MathML"

func (builder *epubBuilder) mathMLAnnotation(node *ionValue) (string, bool, error) {
	for _, annotation := range ionListValues(ionFieldValue(node, 683)) {
		kind, ok := ionSymbolID(ionFieldValue(annotation, 687))
		if !ok || kind != 690 {
			continue
		}
		raw, err := builder.book.nodeTextRaw(annotation)
		if err != nil {
			return "", true, fmt.Errorf("read MathML annotation: %w", err)
		}
		mathML, err := sanitizeMathMLWithRedactor(raw, builder.book.privacy)
		if err != nil {
			return "", true, err
		}
		return mathML, true, nil
	}
	return "", false, nil
}

func sanitizeMathML(raw string) (string, error) {
	return sanitizeMathMLWithRedactor(raw, nil)
}

func sanitizeMathMLWithRedactor(raw string, redactor *personalRedactor) (string, error) {
	decoder := xml.NewDecoder(strings.NewReader(strings.TrimSpace(raw)))
	decoder.Strict = true
	var output bytes.Buffer
	encoder := xml.NewEncoder(&output)
	depth := 0
	seenRoot := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("parse MathML annotation: %w", err)
		}
		switch value := token.(type) {
		case xml.Directive:
			return "", errors.New("MathML annotation contains a forbidden directive")
		case xml.StartElement:
			if !seenRoot {
				seenRoot = true
				if value.Name.Local != "math" {
					return "", fmt.Errorf("MathML annotation root is %q, expected math", value.Name.Local)
				}
				if value.Name.Space == "" {
					value.Name.Space = mathMLNamespace
				}
			}
			if value.Name.Space != "" && value.Name.Space != mathMLNamespace {
				return "", fmt.Errorf("MathML annotation contains foreign element namespace %q", value.Name.Space)
			}
			value.Name.Space = mathMLNamespace
			attributes := value.Attr[:0]
			hasAltText := false
			for _, attribute := range value.Attr {
				name := strings.ToLower(attribute.Name.Local)
				if name == "amzn-src-id" || name == "class" || strings.HasPrefix(name, "on") {
					continue
				}
				hasAltText = hasAltText || (depth == 0 && name == "alttext")
				if redactor != nil && redactor.matches(attribute.Value) {
					attribute.Value = redactor.redact(attribute.Value)
				}
				attributes = append(attributes, attribute)
			}
			if depth == 0 && !hasAltText {
				attributes = append(attributes, xml.Attr{Name: xml.Name{Local: "alttext"}, Value: ""})
			}
			value.Attr = attributes
			if err := encoder.EncodeToken(value); err != nil {
				return "", err
			}
			depth++
		case xml.EndElement:
			depth--
			value.Name.Space = mathMLNamespace
			if err := encoder.EncodeToken(value); err != nil {
				return "", err
			}
		case xml.CharData:
			if redactor != nil {
				token = xml.CharData(redactor.redact(string(value)))
			}
			if err := encoder.EncodeToken(token); err != nil {
				return "", err
			}
		default:
			if err := encoder.EncodeToken(token); err != nil {
				return "", err
			}
		}
	}
	if !seenRoot || depth != 0 {
		return "", errors.New("MathML annotation is empty or unbalanced")
	}
	if err := encoder.Flush(); err != nil {
		return "", err
	}
	return output.String(), nil
}
