package machoutil

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/blacktop/go-macho/pkg/codesign"
	cs "github.com/blacktop/go-macho/pkg/codesign/types"
)

const lcCodeSignature = 0x1d

// AdHocSign replaces the existing embedded signature of a thin arm64 Mach-O.
// The operation is performed entirely in memory and never invokes codesign.
func AdHocSign(data []byte, identifier string) ([]byte, error) {
	if identifier == "" {
		return nil, fmt.Errorf("code-signing identifier is empty")
	}
	if len(data) < 32 || binary.LittleEndian.Uint32(data[:4]) != mhMagic64 {
		return nil, fmt.Errorf("input is not a thin little-endian 64-bit Mach-O")
	}
	ncmds := binary.LittleEndian.Uint32(data[16:20])
	commandsEnd := uint64(32) + uint64(binary.LittleEndian.Uint32(data[20:24]))
	if commandsEnd > uint64(len(data)) {
		return nil, fmt.Errorf("truncated Mach-O load commands")
	}
	var signatureOffset, signatureSize uint32
	var textOffset, textSize uint64
	var signatureCommand, linkeditCommand uint64
	var linkeditFileOffset uint64
	offset := uint64(32)
	for index := uint32(0); index < ncmds; index++ {
		if offset+8 > commandsEnd {
			return nil, fmt.Errorf("truncated load command %d", index)
		}
		command := binary.LittleEndian.Uint32(data[offset : offset+4])
		commandSize := binary.LittleEndian.Uint32(data[offset+4 : offset+8])
		if commandSize < 8 || offset+uint64(commandSize) > commandsEnd {
			return nil, fmt.Errorf("invalid load command %d", index)
		}
		switch command {
		case lcCodeSignature:
			if commandSize < 16 {
				return nil, fmt.Errorf("truncated LC_CODE_SIGNATURE")
			}
			signatureCommand = offset
			signatureOffset = binary.LittleEndian.Uint32(data[offset+8 : offset+12])
			signatureSize = binary.LittleEndian.Uint32(data[offset+12 : offset+16])
		case lcSegment64:
			if commandSize >= 72 {
				name := string(bytes.TrimRight(data[offset+8:offset+24], "\x00"))
				switch name {
				case "__TEXT":
					textOffset = binary.LittleEndian.Uint64(data[offset+40 : offset+48])
					textSize = binary.LittleEndian.Uint64(data[offset+48 : offset+56])
				case "__LINKEDIT":
					linkeditCommand = offset
					linkeditFileOffset = binary.LittleEndian.Uint64(data[offset+40 : offset+48])
				}
			}
		}
		offset += uint64(commandSize)
	}
	if signatureOffset == 0 || signatureSize == 0 {
		return nil, fmt.Errorf("Mach-O has no embedded code-signature slot")
	}
	signatureEnd := uint64(signatureOffset) + uint64(signatureSize)
	if signatureEnd > uint64(len(data)) || uint64(signatureOffset) < commandsEnd {
		return nil, fmt.Errorf("invalid embedded code-signature range")
	}
	if signatureCommand == 0 || linkeditCommand == 0 || linkeditFileOffset > uint64(signatureOffset) {
		return nil, fmt.Errorf("Mach-O code-signature or __LINKEDIT command is invalid")
	}
	config := &codesign.Config{
		ID:         identifier,
		Flags:      cs.ADHOC,
		CodeSize:   uint64(signatureOffset),
		TextOffset: textOffset,
		TextSize:   textSize,
	}
	config.InitSlotHashes()
	// The source signature uses larger hashing pages than the Go signer. Grow
	// the terminal __LINKEDIT/code-signature slot before hashing the header.
	required := align(uint64(codesign.EstimateCodeSignatureSize(config)), 0x4000)
	if required < uint64(signatureSize) {
		required = uint64(signatureSize)
	}
	desiredEnd := uint64(signatureOffset) + required
	if desiredEnd > uint64(len(data)) {
		data = append(data, make([]byte, desiredEnd-uint64(len(data)))...)
	}
	binary.LittleEndian.PutUint32(data[signatureCommand+12:signatureCommand+16], uint32(required))
	linkeditFileSize := desiredEnd - linkeditFileOffset
	binary.LittleEndian.PutUint64(data[linkeditCommand+48:linkeditCommand+56], linkeditFileSize)
	binary.LittleEndian.PutUint64(data[linkeditCommand+32:linkeditCommand+40], align(linkeditFileSize, 0x4000))
	signatureEnd = desiredEnd
	signature, err := codesign.Sign(bytes.NewReader(data[:signatureOffset]), config)
	if err != nil {
		return nil, fmt.Errorf("create ad-hoc code signature: %w", err)
	}
	if len(signature) > int(required) {
		return nil, fmt.Errorf("ad-hoc signature needs %d bytes; slot has %d", len(signature), required)
	}
	clear(data[signatureOffset:signatureEnd])
	copy(data[signatureOffset:signatureEnd], signature)
	return data, nil
}

func align(value, alignment uint64) uint64 {
	return (value + alignment - 1) &^ (alignment - 1)
}
