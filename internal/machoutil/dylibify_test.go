package machoutil

import (
	"encoding/binary"
	"os"
	"testing"

	"github.com/blacktop/go-macho/pkg/codesign"
	cs "github.com/blacktop/go-macho/pkg/codesign/types"
)

func TestDylibify(t *testing.T) {
	data := make([]byte, 4096)
	binary.LittleEndian.PutUint32(data[0:4], mhMagic64)
	binary.LittleEndian.PutUint32(data[12:16], mhExecute)
	binary.LittleEndian.PutUint32(data[16:20], 1)
	binary.LittleEndian.PutUint32(data[20:24], 152)
	command := data[32:184]
	binary.LittleEndian.PutUint32(command[0:4], lcSegment64)
	binary.LittleEndian.PutUint32(command[4:8], 152)
	binary.LittleEndian.PutUint32(command[64:68], 1)
	binary.LittleEndian.PutUint32(command[72+48:72+52], 1024)

	if err := Dylibify(data); err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint32(data[12:16]); got != mhDylib {
		t.Fatalf("filetype = %d, want %d", got, mhDylib)
	}
	if got := binary.LittleEndian.Uint32(data[16:20]); got != 2 {
		t.Fatalf("ncmds = %d, want 2", got)
	}
	if got := binary.LittleEndian.Uint32(data[184:188]); got != lcIDDylib {
		t.Fatalf("new load command = %#x, want %#x", got, lcIDDylib)
	}
}

func TestDylibifyRejectsInvalidInput(t *testing.T) {
	if err := Dylibify(make([]byte, 32)); err == nil {
		t.Fatal("expected invalid Mach-O error")
	}
}

func TestThinARM64Universal(t *testing.T) {
	thin := make([]byte, 64)
	binary.LittleEndian.PutUint32(thin[0:4], mhMagic64)
	binary.LittleEndian.PutUint32(thin[4:8], cpuArm64)
	universal := make([]byte, 128)
	binary.BigEndian.PutUint32(universal[0:4], fatMagic)
	binary.BigEndian.PutUint32(universal[4:8], 1)
	binary.BigEndian.PutUint32(universal[8:12], cpuArm64)
	binary.BigEndian.PutUint32(universal[16:20], 64)
	binary.BigEndian.PutUint32(universal[20:24], uint32(len(thin)))
	copy(universal[64:], thin)
	got, err := ThinARM64(universal)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(thin) || binary.LittleEndian.Uint32(got[4:8]) != cpuArm64 {
		t.Fatalf("unexpected arm64 slice")
	}
}

func TestPatchAndSignCurrentGoExecutable(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	data, err = ThinARM64(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetMacCatalystPlatform(data); err != nil {
		t.Fatal(err)
	}
	data, err = AdHocSign(data, "GoBridgeTest")
	if err != nil {
		t.Fatal(err)
	}
	offset, size := codeSignatureRange(t, data)
	signature, err := codesign.ParseCodeSignature(data[offset : offset+size])
	if err != nil {
		t.Fatal(err)
	}
	if len(signature.CodeDirectories) != 1 || signature.CodeDirectories[0].Header.Flags&cs.ADHOC == 0 {
		t.Fatalf("generated signature is not ad hoc")
	}
}

func codeSignatureRange(t *testing.T, data []byte) (uint32, uint32) {
	t.Helper()
	ncmds := binary.LittleEndian.Uint32(data[16:20])
	offset := uint32(32)
	for index := uint32(0); index < ncmds; index++ {
		command := binary.LittleEndian.Uint32(data[offset : offset+4])
		size := binary.LittleEndian.Uint32(data[offset+4 : offset+8])
		if command == lcCodeSignature {
			return binary.LittleEndian.Uint32(data[offset+8 : offset+12]),
				binary.LittleEndian.Uint32(data[offset+12 : offset+16])
		}
		offset += size
	}
	t.Fatal("LC_CODE_SIGNATURE is missing")
	return 0, 0
}
