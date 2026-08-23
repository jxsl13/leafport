package kfxconvert

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"
)

var ionVersionMarker = []byte{0xe0, 0x01, 0x00, 0xea}

const maxIonDepth = 256

type ionKind byte

const (
	ionNull ionKind = iota
	ionBool
	ionInt
	ionFloat
	ionDecimal
	ionTimestamp
	ionSymbol
	ionString
	ionCLOB
	ionBLOB
	ionList
	ionSExpr
	ionStruct
)

type ionField struct {
	id    uint64
	value *ionValue
}

type ionValue struct {
	kind        ionKind
	annotations []uint64
	boolean     bool
	integer     int64
	unsigned    uint64
	floating    float64
	text        string
	data        []byte
	children    []*ionValue
	fields      []ionField
}

func parseIonValues(data []byte) ([]*ionValue, error) {
	parser := ionParser{data: data, budget: len(data)*4 + 1024}
	return parser.parseSequence(len(data), false, 0)
}

type ionParser struct {
	data   []byte
	pos    int
	budget int
}

func (p *ionParser) parseSequence(end int, structFields bool, depth int) ([]*ionValue, error) {
	if depth > maxIonDepth {
		return nil, errors.New("Ion nesting exceeds limit")
	}
	if end < p.pos || end > len(p.data) {
		return nil, fmt.Errorf("invalid Ion boundary %d", end)
	}
	var values []*ionValue
	for p.pos < end {
		if !structFields && end-p.pos >= len(ionVersionMarker) &&
			bytes.Equal(p.data[p.pos:p.pos+len(ionVersionMarker)], ionVersionMarker) {
			p.pos += len(ionVersionMarker)
			continue
		}
		var fieldID uint64
		var err error
		if structFields {
			fieldID, err = p.readVarUInt(end)
			if err != nil {
				return nil, fmt.Errorf("read Ion field ID: %w", err)
			}
		}
		value, nop, err := p.parseValue(end, depth+1)
		if err != nil {
			return nil, err
		}
		if nop {
			continue
		}
		if structFields {
			// A struct is represented by the parent value's fields, so callers
			// temporarily receive the field ID as a synthetic annotation.
			value.annotations = append([]uint64{fieldID}, value.annotations...)
		}
		values = append(values, value)
	}
	return values, nil
}

func (p *ionParser) parseValue(parentEnd, depth int) (*ionValue, bool, error) {
	if p.budget == 0 {
		return nil, false, errors.New("Ion value count exceeds limit")
	}
	p.budget--
	if p.pos >= parentEnd {
		return nil, false, fmt.Errorf("missing Ion descriptor at offset %d", p.pos)
	}
	descriptorOffset := p.pos
	descriptor := p.data[p.pos]
	p.pos++
	typeID, lengthCode := descriptor>>4, descriptor&0x0f
	if typeID == 0x0f {
		return nil, false, fmt.Errorf("reserved Ion type at offset %d", descriptorOffset)
	}
	if typeID == 0x0e && lengthCode == 0 {
		return nil, false, fmt.Errorf("unexpected Ion version marker at offset %d", descriptorOffset)
	}
	if lengthCode == 0x0f {
		return &ionValue{kind: ionNull}, false, nil
	}
	if typeID == 0x01 {
		if lengthCode > 1 {
			return nil, false, fmt.Errorf("invalid Ion bool at offset %d", descriptorOffset)
		}
		return &ionValue{kind: ionBool, boolean: lengthCode == 1}, false, nil
	}

	length := int(lengthCode)
	var err error
	if lengthCode == 0x0e || typeID == 0x0d && lengthCode == 1 {
		length, err = p.readLength(parentEnd)
		if err != nil {
			return nil, false, err
		}
	}
	if length < 0 || p.pos > parentEnd-length {
		return nil, false, fmt.Errorf("Ion value at offset %d exceeds its container", descriptorOffset)
	}
	valueEnd := p.pos + length
	if typeID == 0 {
		p.pos = valueEnd
		return nil, true, nil
	}

	if typeID == 0x0e {
		annotationBytes, readErr := p.readVarUInt(valueEnd)
		if readErr != nil || annotationBytes > uint64(valueEnd-p.pos) {
			return nil, false, fmt.Errorf("invalid Ion annotation at offset %d", descriptorOffset)
		}
		annotationEnd := p.pos + int(annotationBytes)
		var annotations []uint64
		for p.pos < annotationEnd {
			annotation, annotationErr := p.readVarUInt(annotationEnd)
			if annotationErr != nil {
				return nil, false, annotationErr
			}
			annotations = append(annotations, annotation)
		}
		inner, nop, innerErr := p.parseValue(valueEnd, depth+1)
		if innerErr != nil || nop || p.pos != valueEnd {
			if innerErr != nil {
				return nil, false, innerErr
			}
			return nil, false, fmt.Errorf("invalid annotated Ion value at offset %d", descriptorOffset)
		}
		inner.annotations = append(annotations, inner.annotations...)
		return inner, false, nil
	}

	payload := p.data[p.pos:valueEnd]
	value := &ionValue{}
	switch typeID {
	case 0x02, 0x03:
		unsigned, readErr := unsignedBigEndian(payload)
		if readErr != nil || unsigned > math.MaxInt64 {
			return nil, false, fmt.Errorf("Ion integer at offset %d overflows int64", descriptorOffset)
		}
		value.kind = ionInt
		value.unsigned = unsigned
		value.integer = int64(unsigned)
		if typeID == 0x03 {
			value.integer = -value.integer
		}
		p.pos = valueEnd
	case 0x04:
		value.kind = ionFloat
		switch len(payload) {
		case 0:
			value.floating = 0
		case 4:
			value.floating = float64(math.Float32frombits(binary.BigEndian.Uint32(payload)))
		case 8:
			value.floating = math.Float64frombits(binary.BigEndian.Uint64(payload))
		default:
			return nil, false, fmt.Errorf("Ion float at offset %d has invalid length %d", descriptorOffset, len(payload))
		}
		p.pos = valueEnd
	case 0x05:
		value.kind = ionDecimal
		value.data = append([]byte(nil), payload...)
		p.pos = valueEnd
	case 0x06:
		value.kind = ionTimestamp
		value.data = append([]byte(nil), payload...)
		p.pos = valueEnd
	case 0x07:
		unsigned, readErr := unsignedBigEndian(payload)
		if readErr != nil {
			return nil, false, readErr
		}
		value.kind = ionSymbol
		value.unsigned = unsigned
		p.pos = valueEnd
	case 0x08:
		if !utf8.Valid(payload) {
			return nil, false, fmt.Errorf("invalid UTF-8 Ion string at offset %d", descriptorOffset)
		}
		value.kind = ionString
		value.text = string(payload)
		p.pos = valueEnd
	case 0x09:
		value.kind = ionCLOB
		value.data = append([]byte(nil), payload...)
		p.pos = valueEnd
	case 0x0a:
		value.kind = ionBLOB
		value.data = append([]byte(nil), payload...)
		p.pos = valueEnd
	case 0x0b, 0x0c:
		value.kind = ionList
		if typeID == 0x0c {
			value.kind = ionSExpr
		}
		value.children, err = p.parseSequence(valueEnd, false, depth+1)
	case 0x0d:
		value.kind = ionStruct
		children, parseErr := p.parseSequence(valueEnd, true, depth+1)
		if parseErr != nil {
			return nil, false, parseErr
		}
		for _, child := range children {
			if len(child.annotations) == 0 {
				return nil, false, errors.New("Ion struct field is missing its ID")
			}
			value.fields = append(value.fields, ionField{id: child.annotations[0], value: child})
			child.annotations = child.annotations[1:]
		}
	default:
		return nil, false, fmt.Errorf("unsupported Ion type %#x", typeID)
	}
	if err != nil {
		return nil, false, err
	}
	if p.pos != valueEnd {
		return nil, false, fmt.Errorf("Ion value at offset %d left trailing bytes", descriptorOffset)
	}
	return value, false, nil
}

func (p *ionParser) readLength(end int) (int, error) {
	value, err := p.readVarUInt(end)
	if err != nil {
		return 0, err
	}
	if value > uint64(^uint(0)>>1) {
		return 0, fmt.Errorf("Ion length %d overflows int", value)
	}
	return int(value), nil
}

func (p *ionParser) readVarUInt(end int) (uint64, error) {
	var value uint64
	for range 10 {
		if p.pos >= end {
			return 0, fmt.Errorf("truncated Ion VarUInt at offset %d", p.pos)
		}
		b := p.data[p.pos]
		p.pos++
		if value > math.MaxUint64>>7 {
			return 0, errors.New("Ion VarUInt overflow")
		}
		value = value<<7 | uint64(b&0x7f)
		if b&0x80 != 0 {
			return value, nil
		}
	}
	return 0, errors.New("Ion VarUInt is too long")
}

func unsignedBigEndian(data []byte) (uint64, error) {
	if len(data) > 8 {
		return 0, errors.New("Ion integer exceeds 64 bits")
	}
	var padded [8]byte
	copy(padded[len(padded)-len(data):], data)
	return binary.BigEndian.Uint64(padded[:]), nil
}

func ionFieldValue(value *ionValue, id uint64) *ionValue {
	if value == nil || value.kind != ionStruct {
		return nil
	}
	for _, field := range value.fields {
		if field.id == id {
			return field.value
		}
	}
	return nil
}

func ionUint(value *ionValue) (uint64, bool) {
	if value == nil || value.kind != ionInt || value.integer < 0 {
		return 0, false
	}
	return value.unsigned, true
}

func ionNumber(value *ionValue) (float64, bool) {
	if value == nil {
		return 0, false
	}
	switch value.kind {
	case ionInt:
		return float64(value.integer), true
	case ionFloat:
		return value.floating, true
	default:
		return 0, false
	}
}
