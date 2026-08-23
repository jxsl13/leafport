//go:build darwin

package runtimebridge

import (
	"encoding/binary"
	"testing"
)

func TestValidateRelocatablePrologue(t *testing.T) {
	prologue := make([]byte, inlineHookSize)
	for index, instruction := range []uint32{
		0xd10103ff, // sub sp, sp, #0x40
		0xa9037bfd, // stp x29, x30, [sp, #0x30]
		0x9100c3fd, // add x29, sp, #0x30
		0xd503201f, // nop
	} {
		binary.LittleEndian.PutUint32(prologue[index*4:], instruction)
	}
	if err := validateRelocatablePrologue(prologue); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRelocatablePrologueRejectsUnsafeInstructions(t *testing.T) {
	tests := map[string]uint32{
		"relative branch":  0x14000000, // b .
		"register return":  0xd65f03c0, // ret
		"relative address": 0x90000000, // adrp x0, .
	}
	for name, unsafeInstruction := range tests {
		t.Run(name, func(t *testing.T) {
			prologue := make([]byte, inlineHookSize)
			for offset := 0; offset < len(prologue); offset += 4 {
				binary.LittleEndian.PutUint32(prologue[offset:], 0xd503201f)
			}
			binary.LittleEndian.PutUint32(prologue, unsafeInstruction)
			if err := validateRelocatablePrologue(prologue); err == nil {
				t.Fatal("unsafe prologue was accepted")
			}
		})
	}
}

func TestValidateRelocatablePrologueRejectsWrongSize(t *testing.T) {
	if err := validateRelocatablePrologue(make([]byte, inlineHookSize-4)); err == nil {
		t.Fatal("short prologue was accepted")
	}
}
