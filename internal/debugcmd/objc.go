package debugcmd

import (
	"errors"
	"fmt"
	"io"
	"regexp"
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
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: leafport debug objc [--binary PATH] [--class REGEX]")
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
	file, closeFile, err := openGoMachOARM64(*binaryPath)
	if err != nil {
		return fmt.Errorf("debug objc: %w", err)
	}
	defer closeFile()
	classes, err := file.GetObjCClasses()
	if err != nil {
		return fmt.Errorf("debug objc: parse classes: %w", err)
	}
	sort.Slice(classes, func(i, j int) bool { return classes[i].Name < classes[j].Name })
	matched := 0
	for _, class := range classes {
		if !expression.MatchString(class.Name) {
			continue
		}
		matched++
		fmt.Fprintf(stdout, "%s : %s @ %#x\n", class.Name, class.SuperClass, class.ClassPtr)
		writeObjCMethods(stdout, "+", class.ClassMethods)
		writeObjCMethods(stdout, "-", class.InstanceMethods)
	}
	if matched == 0 {
		return fmt.Errorf("debug objc: --class %q matched no classes", *classPattern)
	}
	return nil
}

func writeObjCMethods(output io.Writer, marker string, methods []objc.Method) {
	sort.Slice(methods, func(i, j int) bool { return methods[i].Name < methods[j].Name })
	for _, method := range methods {
		fmt.Fprintf(output, "  %s[%#x] %s %s\n", marker, method.ImpVMAddr, method.Name, method.Types)
	}
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
