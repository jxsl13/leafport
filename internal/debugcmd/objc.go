package debugcmd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"

	gomacho "github.com/blacktop/go-macho"
	"github.com/blacktop/go-macho/types"
	"github.com/blacktop/go-macho/types/objc"
	"github.com/spf13/pflag"
)

func runDebugObjC(arguments []string, stdout, stderr io.Writer) error {
	flags := pflag.NewFlagSet("debug objc", pflag.ContinueOnError)
	flags.SetOutput(stderr)
	binaryPath := flags.String("binary", "/Applications/Amazon Kindle.app/Contents/MacOS/Kindle", "Mach-O path")
	classPattern := flags.String("class", "(?i)account|auth|drm|keychain|secret", "Go regular expression matching class names")
	methodPattern := flags.String("method", "", "optional Go regular expression matching method names")
	showCalls := flags.Bool("calls", false, "resolve direct Objective-C selector calls made by matching methods")
	showReferences := flags.Bool("references", false, "resolve Objective-C class and constant-string references")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: leafport debug objc [--binary PATH] [--class REGEX] [--method REGEX] [--calls] [--references]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("debug objc: unexpected arguments: %v", flags.Args())
	}
	expression, err := regexp.Compile(*classPattern)
	if err != nil {
		return fmt.Errorf("debug objc: invalid --class expression: %w", err)
	}
	var methodExpression *regexp.Regexp
	if *methodPattern != "" {
		methodExpression, err = regexp.Compile(*methodPattern)
		if err != nil {
			return fmt.Errorf("debug objc: invalid --method expression: %w", err)
		}
	}
	file, closeFile, err := openGoMachOARM64(*binaryPath)
	if err != nil {
		return fmt.Errorf("debug objc: %w", err)
	}
	defer closeFile()
	classes, err := file.GetObjCClasses()
	if err != nil {
		return fmt.Errorf("debug objc: parse classes: %w", err)
	}
	baseAddress := file.GetBaseAddress()
	methodEnds := objectiveCMethodEnds(classes, baseAddress)
	var selectorCalls map[uint64]string
	if *showCalls {
		selectorCalls, err = objectiveCSelectorStubs(file)
		if err != nil {
			return fmt.Errorf("debug objc: resolve selector stubs: %w", err)
		}
	}
	var references map[uint64]string
	if *showReferences {
		references, err = objectiveCReferences(file)
		if err != nil {
			return fmt.Errorf("debug objc: resolve references: %w", err)
		}
	}
	sort.Slice(classes, func(i, j int) bool { return classes[i].Name < classes[j].Name })
	matched := 0
	for _, class := range classes {
		if !expression.MatchString(class.Name) {
			continue
		}
		matched++
		fmt.Fprintf(stdout, "%s : %s @ %#x\n", class.Name, class.SuperClass, class.ClassPtr)
		writeObjCMethods(stdout, "+", class.ClassMethods, methodExpression, baseAddress, methodEnds, selectorCalls, references, file)
		writeObjCMethods(stdout, "-", class.InstanceMethods, methodExpression, baseAddress, methodEnds, selectorCalls, references, file)
	}
	if matched == 0 {
		return fmt.Errorf("debug objc: --class %q matched no classes", *classPattern)
	}
	return nil
}

func writeObjCMethods(
	output io.Writer,
	marker string,
	methods []objc.Method,
	methodExpression *regexp.Regexp,
	baseAddress uint64,
	methodEnds map[uint64]uint64,
	selectorCalls map[uint64]string,
	references map[uint64]string,
	file *gomacho.File,
) {
	sort.Slice(methods, func(i, j int) bool { return methods[i].Name < methods[j].Name })
	for _, method := range methods {
		if methodExpression != nil && !methodExpression.MatchString(method.Name) {
			continue
		}
		address := absoluteObjectiveCAddress(method.ImpVMAddr, baseAddress)
		fmt.Fprintf(output, "  %s[%#x] %s %s\n", marker, address, method.Name, method.Types)
		end := methodEnds[address]
		if selectorCalls != nil {
			for _, call := range objectiveCMethodCalls(file, address, end, selectorCalls) {
				fmt.Fprintf(output, "      %#x -> %s\n", call.address, call.selector)
			}
		}
		if references != nil {
			for _, reference := range objectiveCMethodReferences(file, address, end, references) {
				fmt.Fprintf(output, "      %#x => %s\n", reference.address, reference.name)
			}
		}
	}
}

type objectiveCCall struct {
	address  uint64
	selector string
}

type objectiveCReference struct {
	address uint64
	name    string
}

func absoluteObjectiveCAddress(address, base uint64) uint64 {
	if address < base {
		return address + base
	}
	return address
}

func objectiveCMethodEnds(classes []objc.Class, base uint64) map[uint64]uint64 {
	var addresses []uint64
	for _, class := range classes {
		for _, methods := range [][]objc.Method{class.ClassMethods, class.InstanceMethods} {
			for _, method := range methods {
				addresses = append(addresses, absoluteObjectiveCAddress(method.ImpVMAddr, base))
			}
		}
	}
	slices.Sort(addresses)
	ends := make(map[uint64]uint64, len(addresses))
	for index, address := range addresses {
		end := address + 4096
		if index+1 < len(addresses) && addresses[index+1] > address {
			end = min(end, addresses[index+1])
		}
		ends[address] = end
	}
	return ends
}

func objectiveCSelectorStubs(file *gomacho.File) (map[uint64]string, error) {
	selectorReferences, err := file.GetObjCSelectorReferences()
	if err != nil {
		return nil, err
	}
	section := file.Section("__TEXT", "__objc_stubs")
	if section == nil {
		return nil, errors.New("Mach-O contains no __TEXT.__objc_stubs section")
	}
	data, err := section.Data()
	if err != nil {
		return nil, err
	}
	stubs := make(map[uint64]string)
	for offset := 0; offset+8 <= len(data); offset += 4 {
		adrp := binary.LittleEndian.Uint32(data[offset : offset+4])
		load := binary.LittleEndian.Uint32(data[offset+4 : offset+8])
		if adrp&0x9f00001f != 0x90000001 || load&0xffc003ff != 0xf9400021 {
			continue
		}
		address := section.Addr + uint64(offset)
		referenceAddress := adrpTarget(address, adrp) + uint64((load>>10)&0xfff)*8
		if selector, ok := selectorReferences[referenceAddress]; ok {
			stubs[address] = selector.Name
		}
	}
	return stubs, nil
}

func objectiveCReferences(file *gomacho.File) (map[uint64]string, error) {
	references := make(map[uint64]string)
	classes, err := file.GetObjCClassReferences()
	if err != nil {
		return nil, err
	}
	for address, class := range classes {
		references[address] = "class " + class.Name
	}
	strings, err := file.GetCFStrings()
	if err != nil {
		return nil, err
	}
	for _, value := range strings {
		references[value.Address] = fmt.Sprintf("string %q", value.Name)
	}
	return references, nil
}

func objectiveCMethodCalls(file *gomacho.File, start, end uint64, stubs map[uint64]string) []objectiveCCall {
	if end <= start {
		return nil
	}
	section := file.Section("__TEXT", "__text")
	if section == nil || start < section.Addr || start >= section.Addr+section.Size {
		return nil
	}
	if end > section.Addr+section.Size {
		end = section.Addr + section.Size
	}
	data, err := section.Data()
	if err != nil {
		return nil
	}
	startOffset := start - section.Addr
	endOffset := end - section.Addr
	var calls []objectiveCCall
	for offset := startOffset; offset+4 <= endOffset; offset += 4 {
		instruction := binary.LittleEndian.Uint32(data[offset : offset+4])
		if instruction&0xfc000000 != 0x94000000 {
			continue
		}
		immediate := int64(instruction & 0x03ffffff)
		if immediate&(1<<25) != 0 {
			immediate -= 1 << 26
		}
		address := section.Addr + offset
		target := uint64(int64(address) + (immediate << 2))
		if selector, ok := stubs[target]; ok {
			calls = append(calls, objectiveCCall{address: address, selector: selector})
		}
	}
	return calls
}

func objectiveCMethodReferences(file *gomacho.File, start, end uint64, names map[uint64]string) []objectiveCReference {
	if end <= start {
		return nil
	}
	section := file.Section("__TEXT", "__text")
	if section == nil || start < section.Addr || start >= section.Addr+section.Size {
		return nil
	}
	if end > section.Addr+section.Size {
		end = section.Addr + section.Size
	}
	data, err := section.Data()
	if err != nil {
		return nil
	}
	startOffset := start - section.Addr
	endOffset := end - section.Addr
	var references []objectiveCReference
	seen := make(map[uint64]bool)
	for offset := startOffset; offset+8 <= endOffset; offset += 4 {
		adrp := binary.LittleEndian.Uint32(data[offset : offset+4])
		if adrp&0x9f000000 != 0x90000000 {
			continue
		}
		pc := section.Addr + offset
		page := adrpTarget(pc, adrp)
		register := adrp & 0x1f
		for lookahead := uint64(1); lookahead <= 6 && offset+lookahead*4+4 <= endOffset; lookahead++ {
			next := binary.LittleEndian.Uint32(data[offset+lookahead*4 : offset+lookahead*4+4])
			var target uint64
			switch {
			case next&0xffc00000 == 0xf9400000 && (next>>5)&0x1f == register:
				target = page + uint64((next>>10)&0xfff)*8
			case next&0xffc00000 == 0x91000000 && (next>>5)&0x1f == register:
				target = page + uint64((next>>10)&0xfff)
			default:
				continue
			}
			if name, ok := names[target]; ok && !seen[pc] {
				references = append(references, objectiveCReference{address: pc, name: name})
				seen[pc] = true
			}
			break
		}
	}
	return references
}

func openGoMachOARM64(path string) (*gomacho.File, func() error, error) {
	fat, err := gomacho.OpenFat(path)
	if err == nil {
		for _, architecture := range fat.Arches {
			if architecture.CPU == types.CPUArm64 {
				return architecture.File, fat.Close, nil
			}
		}
		_ = fat.Close()
		return nil, func() error { return nil }, errors.New("Mach-O contains no arm64 architecture")
	}
	if !errors.Is(err, gomacho.ErrNotFat) {
		return nil, func() error { return nil }, err
	}
	file, err := gomacho.Open(path)
	if err != nil {
		return nil, func() error { return nil }, err
	}
	if file.CPU != types.CPUArm64 {
		_ = file.Close()
		return nil, func() error { return nil }, fmt.Errorf("Mach-O architecture is %s, not arm64", file.CPU)
	}
	return file, file.Close, nil
}
