// Package kfxconvert reads DRM-free KFX containers and converts them to
// standard publication formats.
package kfxconvert

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	kfxContainerHeaderSize = 18
	kfxIndexEntrySize      = 24
	kfxRawMediaType        = 417
	kfxRawFontType         = 418
)

// Entity is one indexed fragment from a KFX container.
type Entity struct {
	ID      uint32
	Type    uint32
	Payload []byte
}

// Container is a validated KFX v1/v2 CONT container.
type Container struct {
	Version         uint16
	DocumentSymbols []string
	Entities        []Entity
}

// ParseContainer validates and reads one complete KFX container.
func ParseContainer(data []byte) (Container, error) {
	var container Container
	if len(data) < kfxContainerHeaderSize {
		return container, errors.New("KFX container is shorter than its fixed header")
	}
	if !bytes.Equal(data[:4], []byte("CONT")) {
		return container, errors.New("KFX container magic is missing")
	}
	container.Version = binary.LittleEndian.Uint16(data[4:6])
	if container.Version != 1 && container.Version != 2 {
		return container, fmt.Errorf("unsupported KFX container version %d", container.Version)
	}
	headerLength := uint64(binary.LittleEndian.Uint32(data[6:10]))
	infoOffset := uint64(binary.LittleEndian.Uint32(data[10:14]))
	infoLength := uint64(binary.LittleEndian.Uint32(data[14:18]))
	infoData, err := boundedSlice(data, infoOffset, infoLength)
	if err != nil {
		return container, fmt.Errorf("KFX container info: %w", err)
	}
	values, err := parseIonValues(infoData)
	if err != nil || len(values) == 0 {
		if err == nil {
			err = errors.New("missing Ion value")
		}
		return container, fmt.Errorf("parse KFX container info: %w", err)
	}
	info := values[len(values)-1]
	if symbolsOffset, offsetOK := ionUint(ionFieldValue(info, 415)); offsetOK {
		if symbolsLength, lengthOK := ionUint(ionFieldValue(info, 416)); lengthOK && symbolsLength > 0 {
			symbolData, sliceErr := boundedSlice(data, symbolsOffset, symbolsLength)
			if sliceErr != nil {
				return container, fmt.Errorf("KFX document symbols: %w", sliceErr)
			}
			container.DocumentSymbols, err = parseDocumentSymbols(symbolData)
			if err != nil {
				return container, fmt.Errorf("parse KFX document symbols: %w", err)
			}
		}
	}
	indexOffset, ok := ionUint(ionFieldValue(info, 413))
	if !ok {
		return container, errors.New("KFX container info has no index offset")
	}
	indexLength, ok := ionUint(ionFieldValue(info, 414))
	if !ok || indexLength%kfxIndexEntrySize != 0 {
		return container, errors.New("KFX container info has an invalid index length")
	}
	index, err := boundedSlice(data, indexOffset, indexLength)
	if err != nil {
		return container, fmt.Errorf("KFX entity index: %w", err)
	}
	container.Entities = make([]Entity, 0, len(index)/kfxIndexEntrySize)
	for offset := 0; offset < len(index); offset += kfxIndexEntrySize {
		entry := index[offset : offset+kfxIndexEntrySize]
		entityOffset := binary.LittleEndian.Uint64(entry[8:16])
		entityLength := binary.LittleEndian.Uint64(entry[16:24])
		if headerLength > ^uint64(0)-entityOffset {
			return container, errors.New("KFX entity offset overflows")
		}
		serialized, sliceErr := boundedSlice(data, headerLength+entityOffset, entityLength)
		if sliceErr != nil {
			return container, fmt.Errorf("KFX entity %d: %w", len(container.Entities), sliceErr)
		}
		payload, payloadErr := entityPayload(serialized)
		if payloadErr != nil {
			return container, fmt.Errorf("KFX entity %d: %w", len(container.Entities), payloadErr)
		}
		container.Entities = append(container.Entities, Entity{
			ID: binary.LittleEndian.Uint32(entry[:4]), Type: binary.LittleEndian.Uint32(entry[4:8]),
			Payload: payload,
		})
	}
	return container, nil
}

func parseDocumentSymbols(data []byte) ([]string, error) {
	values, err := parseIonValues(data)
	if err != nil || len(values) == 0 {
		if err == nil {
			err = errors.New("missing Ion symbol-table value")
		}
		return nil, err
	}
	symbols := ionFieldValue(values[len(values)-1], 7)
	if symbols == nil || symbols.kind != ionList {
		return nil, errors.New("Ion symbol table has no symbols list")
	}
	result := make([]string, 0, len(symbols.children))
	for _, symbol := range symbols.children {
		if symbol.kind != ionString {
			return nil, errors.New("Ion symbol table contains a non-string symbol")
		}
		result = append(result, symbol.text)
	}
	return result, nil
}

func entityPayload(data []byte) ([]byte, error) {
	if len(data) < 10 || !bytes.Equal(data[:4], []byte("ENTY")) {
		return nil, errors.New("ENTY header is missing")
	}
	if version := binary.LittleEndian.Uint16(data[4:6]); version != 1 {
		return nil, fmt.Errorf("unsupported ENTY version %d", version)
	}
	headerLength := uint64(binary.LittleEndian.Uint32(data[6:10]))
	if headerLength < 10 || headerLength > uint64(len(data)) {
		return nil, fmt.Errorf("invalid ENTY header length %d", headerLength)
	}
	return data[headerLength:], nil
}

func boundedSlice(data []byte, offset, length uint64) ([]byte, error) {
	if offset > uint64(len(data)) || length > uint64(len(data))-offset {
		return nil, fmt.Errorf("range offset=%d length=%d exceeds %d bytes", offset, length, len(data))
	}
	return data[int(offset):int(offset+length)], nil
}
