package machoutil

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	lcBuildVersion      = 0x32
	platformMacCatalyst = 6
)

// SetMacCatalystPlatform changes LC_BUILD_VERSION on a thin Mach-O copy. This
// is used only for a disposable copy of the current Go executable so dyld can
// load Kindle's Mac Catalyst runtime into a platform-compatible process.
func SetMacCatalystPlatform(data []byte) error {
	if len(data) < 32 || binary.LittleEndian.Uint32(data[:4]) != mhMagic64 {
		return errors.New("input is not a thin little-endian 64-bit Mach-O")
	}
	ncmds := binary.LittleEndian.Uint32(data[16:20])
	commandsEnd := uint64(32) + uint64(binary.LittleEndian.Uint32(data[20:24]))
	if commandsEnd > uint64(len(data)) {
		return errors.New("truncated Mach-O load commands")
	}
	offset := uint64(32)
	found := false
	for index := range ncmds {
		if offset+8 > commandsEnd {
			return fmt.Errorf("truncated load command %d", index)
		}
		command := binary.LittleEndian.Uint32(data[offset : offset+4])
		commandSize := binary.LittleEndian.Uint32(data[offset+4 : offset+8])
		if commandSize < 8 || offset+uint64(commandSize) > commandsEnd {
			return fmt.Errorf("invalid load command %d", index)
		}
		if command == lcBuildVersion {
			if commandSize < 24 {
				return errors.New("truncated LC_BUILD_VERSION")
			}
			binary.LittleEndian.PutUint32(data[offset+8:offset+12], platformMacCatalyst)
			// Match the minimum used by the previously verified bridge.
			binary.LittleEndian.PutUint32(data[offset+12:offset+16], 15<<16)
			found = true
		}
		offset += uint64(commandSize)
	}
	if !found {
		return errors.New("Mach-O has no LC_BUILD_VERSION command")
	}
	return nil
}
