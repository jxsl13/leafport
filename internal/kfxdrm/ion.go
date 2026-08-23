package kfxdrm

import (
	"bytes"
	"fmt"
	"slices"
)

const (
	ionNull       = 0x0
	ionBool       = 0x1
	ionPositive   = 0x2
	ionNegative   = 0x3
	ionFloat      = 0x4
	ionDecimal    = 0x5
	ionTimestamp  = 0x6
	ionSymbol     = 0x7
	ionString     = 0x8
	ionCLOB       = 0x9
	ionBLOB       = 0xa
	ionList       = 0xb
	ionSExpr      = 0xc
	ionStruct     = 0xd
	ionAnnotation = 0xe
)

var ionVersionMarker = []byte{0xe0, 0x01, 0x00, 0xea}

// ionValue is the small Binary Ion subset used by DRMION envelopes. Symbol
// IDs are kept numeric because ProtectedData is a shared, fixed table.
type ionValue struct {
	typeID      byte
	fieldID     uint64
	annotations []uint64
	data        []byte
	children    []*ionValue
}

func parseIon(data []byte) ([]*ionValue, error) {
	parser := ionParser{data: data}
	return parser.parseSequence(len(data), false)
}

type ionParser struct {
	data []byte
	pos  int
}

func (p *ionParser) parseSequence(end int, structFields bool) ([]*ionValue, error) {
	if end < p.pos || end > len(p.data) {
		return nil, fmt.Errorf("invalid Ion container boundary %d", end)
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
		value, err := p.parseValue(end, fieldID)
		if err != nil {
			return nil, err
		}
		// Type 0 with a non-null length is an Ion NOP pad.
		if value.typeID != ionNull || value.data == nil {
			values = append(values, value)
		}
	}
	if p.pos != end {
		return nil, fmt.Errorf("ion value exceeded container by %d bytes", p.pos-end)
	}
	return values, nil
}

func (p *ionParser) parseValue(parentEnd int, fieldID uint64) (*ionValue, error) {
	if p.pos >= parentEnd {
		return nil, fmt.Errorf("missing Ion type descriptor at offset %d", p.pos)
	}
	descriptor := p.data[p.pos]
	p.pos++
	typeID, lengthCode := descriptor>>4, descriptor&0x0f

	if typeID == ionAnnotation && lengthCode == 0 {
		return nil, fmt.Errorf("unexpected Ion version marker at offset %d", p.pos-1)
	}

	isNull := lengthCode == 0x0f
	length := 0
	var err error
	switch {
	case isNull:
	case typeID == ionBool:
		if lengthCode > 1 {
			return nil, fmt.Errorf("invalid Ion boolean length code %d", lengthCode)
		}
	case typeID == ionStruct && lengthCode == 1:
		length, err = p.readLength(parentEnd)
	case lengthCode == 0x0e:
		length, err = p.readLength(parentEnd)
	default:
		length = int(lengthCode)
	}
	if err != nil {
		return nil, err
	}
	if length < 0 || p.pos > parentEnd-length {
		return nil, fmt.Errorf("ion value at offset %d has invalid length %d", p.pos-1, length)
	}
	valueEnd := p.pos + length
	value := &ionValue{typeID: typeID, fieldID: fieldID}

	if isNull || typeID == ionBool {
		return value, nil
	}
	if typeID == ionAnnotation {
		annotationBytes, readErr := p.readVarUInt(valueEnd)
		if readErr != nil {
			return nil, fmt.Errorf("read Ion annotation length: %w", readErr)
		}
		if annotationBytes > uint64(valueEnd-p.pos) {
			return nil, fmt.Errorf("ion annotation table exceeds wrapper")
		}
		annotationEnd := p.pos + int(annotationBytes)
		for p.pos < annotationEnd {
			annotation, annotationErr := p.readVarUInt(annotationEnd)
			if annotationErr != nil {
				return nil, fmt.Errorf("read Ion annotation: %w", annotationErr)
			}
			value.annotations = append(value.annotations, annotation)
		}
		inner, innerErr := p.parseValue(valueEnd, fieldID)
		if innerErr != nil {
			return nil, fmt.Errorf("parse annotated Ion value: %w", innerErr)
		}
		if p.pos != valueEnd {
			return nil, fmt.Errorf("annotated Ion value left %d trailing bytes", valueEnd-p.pos)
		}
		inner.annotations = append(value.annotations, inner.annotations...)
		return inner, nil
	}

	switch typeID {
	case ionList, ionSExpr, ionStruct:
		value.children, err = p.parseSequence(valueEnd, typeID == ionStruct)
	default:
		value.data = p.data[p.pos:valueEnd]
		p.pos = valueEnd
	}
	if err != nil {
		return nil, err
	}
	return value, nil
}

func (p *ionParser) readLength(end int) (int, error) {
	value, err := p.readVarUInt(end)
	if err != nil {
		return 0, fmt.Errorf("read Ion length: %w", err)
	}
	maxInt := uint64(^uint(0) >> 1)
	if value > maxInt {
		return 0, fmt.Errorf("ion length %d overflows int", value)
	}
	return int(value), nil
}

func (p *ionParser) readVarUInt(end int) (uint64, error) {
	var value uint64
	for range 10 {
		if p.pos >= end {
			return 0, fmt.Errorf("truncated VarUInt at offset %d", p.pos)
		}
		b := p.data[p.pos]
		p.pos++
		if value > (^uint64(0) >> 7) {
			return 0, fmt.Errorf("VarUInt overflow")
		}
		value = value<<7 | uint64(b&0x7f)
		if b&0x80 != 0 {
			return value, nil
		}
	}
	return 0, fmt.Errorf("VarUInt is too long")
}

func (v *ionValue) hasAnnotation(symbolID uint64) bool {
	return slices.Contains(v.annotations, symbolID)
}
