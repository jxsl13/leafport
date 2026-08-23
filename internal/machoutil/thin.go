package machoutil

import (
	"encoding/binary"
	"errors"
)

const (
	fatMagic   = 0xcafebabe
	fatMagic64 = 0xcafebabf
	cpuArm64   = 0x0100000c
)

// ThinARM64 returns an independent arm64 slice from a universal Mach-O. A
// thin arm64 input is copied unchanged.
func ThinARM64(data []byte) ([]byte, error) {
	if len(data) < 8 {
		return nil, errors.New("truncated Mach-O")
	}
	if binary.LittleEndian.Uint32(data[:4]) == mhMagic64 {
		if binary.LittleEndian.Uint32(data[4:8]) != cpuArm64 {
			return nil, errors.New("thin Mach-O is not arm64")
		}
		return append([]byte(nil), data...), nil
	}
	magic := binary.BigEndian.Uint32(data[:4])
	if magic != fatMagic && magic != fatMagic64 {
		return nil, errors.New("input is not a supported Mach-O")
	}
	count := binary.BigEndian.Uint32(data[4:8])
	recordSize := uint64(20)
	if magic == fatMagic64 {
		recordSize = 32
	}
	tableEnd := uint64(8) + uint64(count)*recordSize
	if tableEnd > uint64(len(data)) {
		return nil, errors.New("truncated universal Mach-O table")
	}
	for index := range count {
		record := uint64(8) + uint64(index)*recordSize
		if binary.BigEndian.Uint32(data[record:record+4]) != cpuArm64 {
			continue
		}
		var offset, size uint64
		if magic == fatMagic64 {
			offset = binary.BigEndian.Uint64(data[record+8 : record+16])
			size = binary.BigEndian.Uint64(data[record+16 : record+24])
		} else {
			offset = uint64(binary.BigEndian.Uint32(data[record+8 : record+12]))
			size = uint64(binary.BigEndian.Uint32(data[record+12 : record+16]))
		}
		if offset > uint64(len(data)) || size > uint64(len(data))-offset {
			return nil, errors.New("arm64 slice exceeds file bounds")
		}
		result := append([]byte(nil), data[offset:offset+size]...)
		if len(result) < 8 || binary.LittleEndian.Uint32(result[:4]) != mhMagic64 ||
			binary.LittleEndian.Uint32(result[4:8]) != cpuArm64 {
			return nil, errors.New("universal arm64 entry is not a thin arm64 Mach-O")
		}
		return result, nil
	}
	return nil, errors.New("universal Mach-O has no arm64 slice")
}
