// Package debugcmd provides Leafport's version-analysis maintenance commands.
package debugcmd

import (
	"debug/macho"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/pflag"
	"golang.org/x/arch/arm64/arm64asm"

	"leafport/internal/machoutil"
)

// Run executes one debug subcommand.
func Run(arguments []string, stdout, stderr io.Writer) error {
	if len(arguments) == 0 || arguments[0] == "help" || arguments[0] == "--help" {
		printDebugUsage(stdout)
		return nil
	}
	switch arguments[0] {
	case "disasm":
		return runDebugDisasm(arguments[1:], stdout, stderr)
	case "dylibify":
		return runDebugDylibify(arguments[1:], stderr)
	default:
		return fmt.Errorf("unknown debug subcommand %q (use: debug --help)", arguments[0])
	}
}

func printDebugUsage(writer io.Writer) {
	fmt.Fprintln(writer, `Usage:
  leafport debug disasm [flags]
  leafport debug dylibify --in SOURCE --out DESTINATION

Debug subcommands:
  disasm    inspect a small arm64 Mach-O address range or find references
  dylibify  make a disposable MH_DYLIB copy of a thin 64-bit Mach-O`)
}

func runDebugDylibify(arguments []string, stderr io.Writer) error {
	flags := pflag.NewFlagSet("debug dylibify", pflag.ContinueOnError)
	flags.SetOutput(stderr)
	inPath := flags.String("in", "", "source thin Mach-O")
	outPath := flags.String("out", "", "destination dylib copy")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: leafport debug dylibify --in SOURCE --out DESTINATION")
		flags.PrintDefaults()
	}
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("debug dylibify: unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if *inPath == "" || *outPath == "" || *inPath == *outPath {
		return errors.New("debug dylibify requires distinct --in and --out paths")
	}
	data, err := os.ReadFile(*inPath)
	if err != nil {
		return fmt.Errorf("debug dylibify: %w", err)
	}
	if err := machoutil.Dylibify(data); err != nil {
		return fmt.Errorf("debug dylibify: %w", err)
	}
	if err := os.WriteFile(*outPath, data, 0o700); err != nil {
		return fmt.Errorf("debug dylibify: %w", err)
	}
	return nil
}

func runDebugDisasm(arguments []string, stdout, stderr io.Writer) error {
	flags := pflag.NewFlagSet("debug disasm", pflag.ContinueOnError)
	flags.SetOutput(stderr)
	binaryPath := flags.String("binary", "/Applications/Amazon Kindle.app/Contents/MacOS/Kindle", "Mach-O path")
	startText := flags.String("start", "", "start virtual address (hex)")
	byteCount := flags.Int("bytes", 128, "number of bytes")
	xrefText := flags.String("xref", "", "find arm64 ADRP+LDR/ADD references to this virtual address")
	xrefRangeText := flags.String("xref-range", "", "find ADRP+LDR/ADD references in start:end virtual-address range")
	callXrefText := flags.String("call-xref", "", "find direct BL/B calls to this virtual address")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: leafport debug disasm [flags]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("debug disasm: unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if *startText == "" && *xrefText == "" && *xrefRangeText == "" && *callXrefText == "" {
		return errors.New("debug disasm requires --start, --xref, --xref-range, or --call-xref")
	}
	if *byteCount <= 0 {
		return errors.New("debug disasm --bytes must be positive")
	}

	file, closeFile, err := openArm64(*binaryPath)
	if err != nil {
		return fmt.Errorf("debug disasm: %w", err)
	}
	defer closeFile()
	if *xrefText != "" {
		target, err := parseHex(*xrefText)
		if err != nil {
			return fmt.Errorf("debug disasm: invalid --xref address: %w", err)
		}
		for _, pc := range findXrefs(file, target) {
			fmt.Fprintf(stdout, "%#x -> %#x\n", pc, target)
		}
		return nil
	}
	if *xrefRangeText != "" {
		parts := strings.Split(*xrefRangeText, ":")
		if len(parts) != 2 {
			return errors.New("debug disasm: --xref-range must be start:end")
		}
		start, err := parseHex(parts[0])
		if err != nil {
			return fmt.Errorf("debug disasm: invalid range start: %w", err)
		}
		end, err := parseHex(parts[1])
		if err != nil || end <= start {
			return errors.New("debug disasm: invalid range end")
		}
		for _, ref := range findXrefRange(file, start, end) {
			fmt.Fprintf(stdout, "%#x -> %#x\n", ref.pc, ref.target)
		}
		return nil
	}
	if *callXrefText != "" {
		target, err := parseHex(*callXrefText)
		if err != nil {
			return fmt.Errorf("debug disasm: invalid call target: %w", err)
		}
		for _, pc := range findDirectCalls(file, target) {
			fmt.Fprintf(stdout, "%#x -> %#x\n", pc, target)
		}
		return nil
	}

	start, err := parseHex(*startText)
	if err != nil {
		return fmt.Errorf("debug disasm: invalid start address: %w", err)
	}
	data, err := dataAt(file, start, *byteCount)
	if err != nil {
		return fmt.Errorf("debug disasm: %w", err)
	}
	for offset := 0; offset+4 <= len(data); offset += 4 {
		pc := start + uint64(offset)
		instruction, err := arm64asm.Decode(data[offset : offset+4])
		if err != nil {
			fmt.Fprintf(stdout, "%#x  % x  <decode error: %v>\n", pc, data[offset:offset+4], err)
			continue
		}
		fmt.Fprintf(stdout, "%#x  % x  %s\n", pc, data[offset:offset+4], arm64asm.GNUSyntax(instruction))
	}
	return nil
}

func findDirectCalls(file *macho.File, target uint64) []uint64 {
	var result []uint64
	for _, section := range file.Sections {
		if section.Name != "__text" {
			continue
		}
		data, err := section.Data()
		if err != nil {
			continue
		}
		for offset := 0; offset+4 <= len(data); offset += 4 {
			instruction := binary.LittleEndian.Uint32(data[offset : offset+4])
			if instruction&0xfc000000 != 0x94000000 && instruction&0xfc000000 != 0x14000000 {
				continue
			}
			pc := section.Addr + uint64(offset)
			immediate := int64(instruction & 0x03ffffff)
			if immediate&(1<<25) != 0 {
				immediate -= 1 << 26
			}
			if uint64(int64(pc)+(immediate<<2)) == target {
				result = append(result, pc)
			}
		}
	}
	return result
}

type xref struct {
	pc     uint64
	target uint64
}

func findXrefRange(file *macho.File, start, end uint64) []xref {
	var result []xref
	for _, section := range file.Sections {
		if section.Name != "__text" && section.Name != "__objc_stubs" {
			continue
		}
		data, err := section.Data()
		if err != nil {
			continue
		}
		for offset := 0; offset+8 <= len(data); offset += 4 {
			adrp := binary.LittleEndian.Uint32(data[offset : offset+4])
			if adrp&0x9f000000 != 0x90000000 {
				continue
			}
			pc := section.Addr + uint64(offset)
			page := adrpTarget(pc, adrp)
			register := adrp & 0x1f
			for lookahead := 1; lookahead <= 6 && offset+lookahead*4+4 <= len(data); lookahead++ {
				next := binary.LittleEndian.Uint32(data[offset+lookahead*4 : offset+lookahead*4+4])
				var address uint64
				switch {
				case next&0xffc00000 == 0xf9400000 && (next>>5)&0x1f == register:
					address = page + uint64((next>>10)&0xfff)*8
				case next&0xffc00000 == 0x91000000 && (next>>5)&0x1f == register:
					address = page + uint64((next>>10)&0xfff)
				default:
					continue
				}
				if address >= start && address < end {
					result = append(result, xref{pc: pc, target: address})
				}
				break
			}
		}
	}
	return result
}

func findXrefs(file *macho.File, target uint64) []uint64 {
	var result []uint64
	for _, section := range file.Sections {
		if section.Name != "__text" && section.Name != "__objc_stubs" {
			continue
		}
		data, err := section.Data()
		if err != nil {
			continue
		}
		for offset := 0; offset+8 <= len(data); offset += 4 {
			adrp := binary.LittleEndian.Uint32(data[offset : offset+4])
			if adrp&0x9f000000 != 0x90000000 {
				continue
			}
			pc := section.Addr + uint64(offset)
			page := adrpTarget(pc, adrp)
			register := adrp & 0x1f
			next := binary.LittleEndian.Uint32(data[offset+4 : offset+8])
			var address uint64
			switch {
			case next&0xffc00000 == 0xf9400000 && (next>>5)&0x1f == register:
				address = page + uint64((next>>10)&0xfff)*8
			case next&0xffc00000 == 0x91000000 && (next>>5)&0x1f == register:
				address = page + uint64((next>>10)&0xfff)
			default:
				continue
			}
			if address == target {
				result = append(result, pc)
			}
		}
	}
	return result
}

func adrpTarget(pc uint64, instruction uint32) uint64 {
	immediate := int64(((instruction >> 5) & 0x7ffff) << 2)
	immediate |= int64((instruction >> 29) & 0x3)
	if immediate&(1<<20) != 0 {
		immediate -= 1 << 21
	}
	return uint64(int64(pc&^0xfff) + (immediate << 12))
}

func parseHex(value string) (uint64, error) {
	return strconv.ParseUint(strings.TrimPrefix(value, "0x"), 16, 64)
}

func openArm64(path string) (*macho.File, func(), error) {
	fat, err := macho.OpenFat(path)
	if err == nil {
		for _, architecture := range fat.Arches {
			if architecture.Cpu == macho.CpuArm64 {
				return architecture.File, func() { _ = fat.Close() }, nil
			}
		}
		_ = fat.Close()
		return nil, func() {}, errors.New("universal binary has no arm64 slice")
	}
	thin, err := macho.Open(path)
	if err != nil {
		return nil, func() {}, err
	}
	if thin.Cpu != macho.CpuArm64 {
		_ = thin.Close()
		return nil, func() {}, fmt.Errorf("thin Mach-O is %s, not arm64", thin.Cpu)
	}
	return thin, func() { _ = thin.Close() }, nil
}

func dataAt(file *macho.File, address uint64, count int) ([]byte, error) {
	for _, section := range file.Sections {
		if address < section.Addr || address >= section.Addr+section.Size {
			continue
		}
		data, err := section.Data()
		if err != nil {
			return nil, err
		}
		offset := address - section.Addr
		end := offset + uint64(count)
		if end > uint64(len(data)) {
			end = uint64(len(data))
		}
		return data[offset:end], nil
	}
	return nil, fmt.Errorf("address %#x is not in a section", address)
}
