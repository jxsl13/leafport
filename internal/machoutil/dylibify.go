// Package machoutil contains narrowly scoped Mach-O transformations used by
// the macOS Kindle runtime bridge.
package machoutil

import (
	"encoding/binary"
	"fmt"
)

const (
	mhMagic64   = 0xfeedfacf
	mhExecute   = 2
	mhDylib     = 6
	lcSegment64 = 0x19
	lcIDDylib   = 0x0d
)

// Dylibify changes a thin, little-endian 64-bit MH_EXECUTE image into an
// MH_DYLIB image and adds an LC_ID_DYLIB command in existing header padding.
// The input slice is modified in place.
func Dylibify(data []byte) error {
	if len(data) < 32 || binary.LittleEndian.Uint32(data) != mhMagic64 {
		return fmt.Errorf("input is not a thin little-endian 64-bit Mach-O")
	}
	if got := binary.LittleEndian.Uint32(data[12:16]); got != mhExecute {
		return fmt.Errorf("input file type is %d, not MH_EXECUTE", got)
	}
	ncmds := binary.LittleEndian.Uint32(data[16:20])
	sizeofcmds := binary.LittleEndian.Uint32(data[20:24])
	commandsEnd := uint64(32 + sizeofcmds)
	if commandsEnd > uint64(len(data)) {
		return fmt.Errorf("truncated load commands")
	}

	firstSection := uint64(len(data))
	off := uint64(32)
	for i := range ncmds {
		if off+8 > uint64(len(data)) {
			return fmt.Errorf("truncated load command %d", i)
		}
		cmd := binary.LittleEndian.Uint32(data[off : off+4])
		cmdsize := binary.LittleEndian.Uint32(data[off+4 : off+8])
		if cmdsize < 8 || off+uint64(cmdsize) > commandsEnd {
			return fmt.Errorf("invalid load command %d", i)
		}
		if cmd == lcSegment64 {
			if cmdsize < 72 {
				return fmt.Errorf("truncated segment command")
			}
			nsects := binary.LittleEndian.Uint32(data[off+64 : off+68])
			sectionOff := off + 72
			for j := uint32(0); j < nsects; j++ {
				if sectionOff+80 > off+uint64(cmdsize) {
					return fmt.Errorf("truncated section table")
				}
				fileOffset := binary.LittleEndian.Uint32(data[sectionOff+48 : sectionOff+52])
				if fileOffset != 0 && uint64(fileOffset) < firstSection {
					firstSection = uint64(fileOffset)
				}
				sectionOff += 80
			}
		}
		off += uint64(cmdsize)
	}
	if off != commandsEnd {
		return fmt.Errorf("load-command size mismatch")
	}

	name := []byte("LeafportRuntime.dylib\x00")
	cmdsize := uint32((24 + len(name) + 7) &^ 7)
	if commandsEnd+uint64(cmdsize) > firstSection {
		return fmt.Errorf("not enough Mach-O header padding for LC_ID_DYLIB")
	}
	command := data[commandsEnd : commandsEnd+uint64(cmdsize)]
	clear(command)
	binary.LittleEndian.PutUint32(command[0:4], lcIDDylib)
	binary.LittleEndian.PutUint32(command[4:8], cmdsize)
	binary.LittleEndian.PutUint32(command[8:12], 24)
	copy(command[24:], name)

	binary.LittleEndian.PutUint32(data[12:16], mhDylib)
	binary.LittleEndian.PutUint32(data[16:20], ncmds+1)
	binary.LittleEndian.PutUint32(data[20:24], sizeofcmds+cmdsize)
	return nil
}
