// Package debugcmd provides Leafport's version-analysis maintenance commands.
package debugcmd

import (
	"bytes"
	"context"
	"debug/macho"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/spf13/pflag"
	"golang.org/x/arch/arm64/arm64asm"

	"github.com/jxsl13/leafport/internal/compatibility"
	"github.com/jxsl13/leafport/internal/kfxconvert"
	"github.com/jxsl13/leafport/internal/machoutil"
)

// Run executes one debug subcommand.
func Run(ctx context.Context, arguments []string, stdout, stderr io.Writer) error {
	if len(arguments) == 0 || arguments[0] == "help" || arguments[0] == "--help" {
		printDebugUsage(stdout)
		return nil
	}
	switch arguments[0] {
	case "analyze":
		return runDebugAnalyze(arguments[1:], stdout, stderr)
	case "disasm":
		return runDebugDisasm(arguments[1:], stdout, stderr)
	case "dylibify":
		return runDebugDylibify(arguments[1:], stderr)
	case "capture":
		return runDebugCapture(ctx, arguments[1:], stdout, stderr)
	case "credentials":
		return runDebugCredentials(arguments[1:], stdout, stderr)
	case "kfx":
		return runDebugKFX(arguments[1:], stdout, stderr)
	case "pdf":
		return runDebugPDF(arguments[1:], stdout, stderr)
	case "objc":
		return runDebugObjC(arguments[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown debug subcommand %q (use: debug --help)", arguments[0])
	}
}

func printDebugUsage(writer io.Writer) {
	fmt.Fprintln(writer, `Usage:
  leafport debug analyze [--binary PATH]
  leafport debug disasm [flags]
  leafport debug dylibify --in SOURCE --out DESTINATION
  leafport debug capture --id BOOK_ID --out ARCHIVE
  leafport debug credentials --preferences PATH [--root PATH]
  leafport debug kfx --in ARCHIVE
  leafport debug pdf --in FILE
  leafport debug objc [--binary PATH] [--class REGEX]

Debug subcommands:
  analyze   fingerprint a build and report compatibility anchors
  disasm    inspect a small arm64 Mach-O address range or find references
  dylibify  make a disposable MH_DYLIB copy of a thin 64-bit Mach-O
  capture   retain one decrypted KFX archive for maintenance analysis
  credentials safely test reader-owned files against the stored secret fingerprint
  kfx       validate and classify a decrypted KFX archive
  pdf       validate PDF pages, outlines, and link annotations
  objc      list per-class Objective-C methods and implementation addresses`)
}

func runDebugKFX(arguments []string, stdout, stderr io.Writer) error {
	flags := pflag.NewFlagSet("debug kfx", pflag.ContinueOnError)
	flags.SetOutput(stderr)
	input := flags.String("in", "", "decrypted .kfx-zip archive")
	typeList := flags.String("types", "", "comma-separated entity type IDs to dump")
	limit := flags.Int("limit", 20, "maximum dumped entities")
	showPlan := flags.Bool("plan", false, "derive fixed-layout resource order")
	showFeatures := flags.Bool("features", false, "list semantic feature IDs without book text")
	extractResources := flags.String("extract-resources", "", "write untouched raw resources to a private analysis directory")
	dependency := flags.Uint64("dependency", 0, "dump dependencies for one numeric fragment ID")
	node := flags.Uint64("node", 0, "dump content nodes with one numeric location ID")
	pdfOut := flags.String("pdf-out", "", "write reconstructed fixed-layout PDF")
	epubOut := flags.String("epub-out", "", "write reconstructed reflowable EPUB")
	cbzOut := flags.String("cbz-out", "", "write image-backed fixed-layout pages as CBZ")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: leafport debug kfx --in ARCHIVE")
		flags.PrintDefaults()
	}
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *input == "" {
		return errors.New("debug kfx requires --in ARCHIVE")
	}
	inspection, err := kfxconvert.InspectArchive(*input)
	if err != nil {
		return fmt.Errorf("debug kfx: %w", err)
	}
	fmt.Fprintf(stdout, "Containers: %d\nEntities: %d\nRaw media: %d\nPDF resources: %d\nImage resources: %d\nFont resources: %d\n",
		inspection.Containers, inspection.Entities, inspection.RawMedia,
		inspection.PDFResources, inspection.ImageResources, inspection.FontResources)
	decision, err := kfxconvert.PreferredFormat(*input)
	if err != nil {
		return fmt.Errorf("debug kfx: choose output format: %w", err)
	}
	fmt.Fprintf(stdout, "Preferred final format: %s (%s)\n", decision.Format, decision.Reason)
	if *showPlan {
		pages, planErr := kfxconvert.FixedLayoutPages(*input)
		if planErr != nil {
			return fmt.Errorf("debug kfx: derive page plan: %w", planErr)
		}
		pdfPages := 0
		locations := make(map[string]bool, len(pages))
		for _, page := range pages {
			if page.Format == 565 {
				pdfPages++
			}
			locations[page.Location] = true
		}
		fmt.Fprintf(stdout, "Ordered page resources: %d (%d PDF, %d raster across %d raw resources)\n",
			len(pages), pdfPages, len(pages)-pdfPages, len(locations))
	}
	if *showFeatures {
		features, featureErr := kfxconvert.InspectFeatures(*input)
		if featureErr != nil {
			return fmt.Errorf("debug kfx: inspect features: %w", featureErr)
		}
		printFeatureIDs := func(name string, values []uint32) {
			items := make([]string, len(values))
			for index, value := range values {
				items[index] = fmt.Sprintf("$%d", value)
			}
			fmt.Fprintf(stdout, "%s: %s\n", name, strings.Join(items, ", "))
		}
		printFeatureIDs("Entity types", features.EntityTypes)
		printFeatureIDs("Content types", features.ContentTypes)
		printFeatureIDs("Layouts", features.Layouts)
		printFeatureIDs("Style fields", features.StyleFields)
		printFeatureIDs("Content fields", features.ContentFields)
		printFeatureIDs("Annotation types", features.AnnotationTypes)
		printFeatureIDs("Classifications", features.Classifications)
		printFeatureIDs("Resource formats", features.ResourceFormats)
	}
	if *extractResources != "" {
		resources, extractErr := kfxconvert.ExtractRawResources(*input, *extractResources)
		if extractErr != nil {
			return fmt.Errorf("debug kfx: extract resources: %w", extractErr)
		}
		for _, resource := range resources {
			fmt.Fprintf(stdout, "Resource: %s -> %s (%d bytes)\n", resource.Source, resource.Path, resource.Size)
		}
	}
	if *typeList != "" {
		var types []uint32
		for item := range strings.SplitSeq(*typeList, ",") {
			value, parseErr := strconv.ParseUint(strings.TrimSpace(item), 10, 32)
			if parseErr != nil {
				return fmt.Errorf("debug kfx: invalid entity type %q", item)
			}
			types = append(types, uint32(value))
		}
		if err := kfxconvert.DumpEntities(*input, types, *limit, stdout); err != nil {
			return fmt.Errorf("debug kfx: %w", err)
		}
	}
	if *dependency != 0 {
		if err := kfxconvert.DumpDependency(*input, *dependency, stdout); err != nil {
			return fmt.Errorf("debug kfx: %w", err)
		}
	}
	if *node != 0 {
		if *node > uint64(^uint32(0)) {
			return fmt.Errorf("debug kfx: node ID %d exceeds uint32", *node)
		}
		if err := kfxconvert.DumpNode(*input, uint32(*node), stdout); err != nil {
			return fmt.Errorf("debug kfx: %w", err)
		}
	}
	if *pdfOut != "" {
		result, err := kfxconvert.ConvertToPDF(*input, *pdfOut)
		if err != nil {
			return fmt.Errorf("debug kfx: %w", err)
		}
		fmt.Fprintf(stdout, "PDF: %s (%d pages from %d resource groups; exact copy: %t)\n",
			*pdfOut, result.Pages, result.Resources, result.ExactCopy)
		if result.BrokenLinksRemoved != 0 {
			fmt.Fprintf(stdout, "Removed nonfunctional embedded-PDF links: %d\n", result.BrokenLinksRemoved)
		}
	}
	if *epubOut != "" {
		result, err := kfxconvert.ConvertToEPUB(*input, *epubOut, kfxconvert.Metadata{})
		if err != nil {
			return fmt.Errorf("debug kfx: %w", err)
		}
		fmt.Fprintf(stdout, "EPUB: %s (%d sections, %d images, %d media, %d fonts)\n",
			*epubOut, result.Sections, result.Images, result.Media, result.Fonts)
	}
	if *cbzOut != "" {
		result, err := kfxconvert.ConvertToCBZ(*input, *cbzOut)
		if err != nil {
			return fmt.Errorf("debug kfx: %w", err)
		}
		fmt.Fprintf(stdout, "CBZ: %s (%d pages from %d resource groups)\n",
			*cbzOut, result.Pages, result.Resources)
	}
	return nil
}

func runDebugAnalyze(arguments []string, stdout, stderr io.Writer) error {
	flags := pflag.NewFlagSet("debug analyze", pflag.ContinueOnError)
	flags.SetOutput(stderr)
	binaryPath := flags.String("binary", "/Applications/Amazon Kindle.app/Contents/MacOS/Kindle", "Mach-O path")
	showCandidates := flags.Bool("candidates", false, "list related Objective-C metadata for re-analysis")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: leafport debug analyze [--binary PATH]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("debug analyze: unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	data, err := os.ReadFile(*binaryPath)
	if err != nil {
		return fmt.Errorf("debug analyze: %w", err)
	}
	fingerprint, err := machoutil.FingerprintARM64(data)
	if err != nil {
		return fmt.Errorf("debug analyze: %w", err)
	}
	file, closeFile, err := openArm64(*binaryPath)
	if err != nil {
		return fmt.Errorf("debug analyze: %w", err)
	}
	defer closeFile()

	fmt.Fprintf(stdout, "Binary: %s\n", *binaryPath)
	if fingerprint.UUID == "" {
		fmt.Fprintln(stdout, "UUID: unavailable")
	} else {
		fmt.Fprintf(stdout, "UUID: %s\n", fingerprint.UUID)
	}
	fmt.Fprintf(stdout, "__text SHA-256: %s\n", fingerprint.TextSHA256)
	profile, exact := compatibility.Resolve(fingerprint.UUID, fingerprint.TextSHA256)
	if exact {
		fmt.Fprintf(stdout, "Compatibility profile: %s (%s)\n", profile.Name, profile.AppVersion)
	} else {
		fmt.Fprintf(stdout, "Compatibility profile: %s (runtime validation required)\n", profile.Name)
	}
	printSymbolStatus(stdout, file, []string{
		"EVP_DecryptInit_ex", "EVP_CIPHER_key_length", "EVP_CIPHER_CTX_key_length",
	})
	classes := objectiveCMetadata(file, "__objc_classname")
	methods := objectiveCMetadata(file, "__objc_methname")
	printMetadataStatus(stdout, "Objective-C classes", classes,
		[]string{"KRFDRMDataProvider", "KRFBook"})
	printMetadataStatus(stdout, "Credential classes", classes,
		[]string{
			"AuthenticationManager", "Keychain", "SecItemKeychainImpl",
			"FileKeychainImpl", "KeychainUpgradeMigration", "SetAccountSecretTodoCommand",
		})
	printMetadataStatus(stdout, "Objective-C selectors", methods, []string{
		"initWithURL:DRMDataProvider:containers:error:",
		"setResourceBundlePath:", "setICUDataDirectory:",
	})
	printMetadataStatus(stdout, "Credential selectors", methods, []string{
		"sharedInstance", "cacheInformation", "accountSecret", "setAccountSecret:",
		"valueForKey:", "setValueOnBackgroundQueue:forKey:",
		"parseAccountSecrets", "migrateAndCleanKeychain",
		"initWithAccountSecrets:kindleSerialNumber:voucherList:",
		"initWithpids:accountSecrets:deviceNumber:voucherPaths:containerPath:",
	})
	printByteAnchorStatus(stdout, data, "Credential storage anchors", []string{
		"kindle.accountsecret.item", "com.amazon.Lassen.KeychainUI",
		"HashedAccountSecret", "userDataDict.dat", "ckcidenabled",
	})
	if *showCandidates {
		printCandidates(stdout, "Class candidates", classes,
			[]string{"account", "auth", "drm", "keychain", "krf", "secret", "voucher"})
		printCandidates(stdout, "Selector candidates", methods,
			[]string{"account", "auth", "drm", "icudata", "keychain", "kindleserial", "resourcebundle", "secret", "voucher"})
	}
	return nil
}

func printByteAnchorStatus(writer io.Writer, data []byte, label string, names []string) {
	fmt.Fprintf(writer, "%s:\n", label)
	for _, name := range names {
		fmt.Fprintf(writer, "  %s = %s\n", name, presence(bytes.Contains(data, []byte(name))))
	}
}

func printSymbolStatus(writer io.Writer, file *macho.File, names []string) {
	fmt.Fprintln(writer, "Crypto symbols:")
	available := make(map[string]bool)
	if file.Symtab != nil {
		for _, symbol := range file.Symtab.Syms {
			available[strings.TrimPrefix(symbol.Name, "_")] = true
		}
	}
	for _, name := range names {
		fmt.Fprintf(writer, "  %s = %s\n", name, presence(available[name]))
	}
}

func objectiveCMetadata(file *macho.File, sectionName string) []string {
	seen := make(map[string]bool)
	for _, section := range file.Sections {
		if section.Name != sectionName {
			continue
		}
		data, err := section.Data()
		if err != nil {
			continue
		}
		for _, raw := range bytes.Split(data, []byte{0}) {
			value := string(raw)
			if value != "" && len(value) <= 512 && utf8.ValidString(value) {
				seen[value] = true
			}
		}
	}
	values := make([]string, 0, len(seen))
	for value := range seen {
		values = append(values, value)
	}
	slices.Sort(values)
	return values
}

func printMetadataStatus(writer io.Writer, label string, values, required []string) {
	available := make(map[string]bool, len(values))
	for _, value := range values {
		available[value] = true
	}
	fmt.Fprintf(writer, "%s:\n", label)
	for _, name := range required {
		fmt.Fprintf(writer, "  %s = %s\n", name, presence(available[name]))
	}
}

func printCandidates(writer io.Writer, label string, values, keywords []string) {
	var matches []string
	for _, value := range values {
		lower := strings.ToLower(value)
		for _, keyword := range keywords {
			if strings.Contains(lower, keyword) {
				matches = append(matches, value)
				break
			}
		}
	}
	const limit = 40
	fmt.Fprintf(writer, "%s:", label)
	if len(matches) == 0 {
		fmt.Fprintln(writer, " none")
		return
	}
	fmt.Fprintln(writer)
	for index, match := range matches {
		if index == limit {
			fmt.Fprintf(writer, "  ... and %d more\n", len(matches)-limit)
			break
		}
		fmt.Fprintf(writer, "  %s\n", match)
	}
}

func presence(present bool) string {
	if present {
		return "present"
	}
	return "missing"
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
