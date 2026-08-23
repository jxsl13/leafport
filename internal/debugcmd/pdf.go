package debugcmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
	"github.com/spf13/pflag"
)

func runDebugPDF(arguments []string, stdout, stderr io.Writer) error {
	flags := pflag.NewFlagSet("debug pdf", pflag.ContinueOnError)
	flags.SetOutput(stderr)
	input := flags.String("in", "", "PDF file to inspect")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: leafport debug pdf --in FILE")
		flags.PrintDefaults()
	}
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *input == "" {
		return errors.New("debug pdf requires --in FILE")
	}
	api.DisableConfigDir()
	configuration := model.NewDefaultConfiguration()
	configuration.ValidationMode = model.ValidationRelaxed
	if err := api.ValidateFile(*input, configuration); err != nil {
		return fmt.Errorf("debug pdf: validate: %w", err)
	}
	file, err := os.Open(*input)
	if err != nil {
		return err
	}
	defer file.Close()
	pages, err := api.PageCount(file, configuration)
	if err != nil {
		return fmt.Errorf("debug pdf: count pages: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	bookmarks, err := api.Bookmarks(file, configuration)
	if err != nil {
		return fmt.Errorf("debug pdf: read outlines: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	annotations, err := api.Annotations(file, nil, configuration)
	if err != nil {
		return fmt.Errorf("debug pdf: read annotations: %w", err)
	}
	linkCount, leafportLinks := 0, 0
	for _, page := range annotations {
		for _, annotation := range page[model.AnnLink].Map {
			linkCount++
			if strings.HasPrefix(annotation.ID(), "leafport-link-") {
				leafportLinks++
			}
		}
	}
	targets, err := inspectPDFLinkTargets(*input, pages)
	if err != nil {
		return fmt.Errorf("debug pdf: inspect link targets: %w", err)
	}
	fmt.Fprintf(stdout, "PDF pages: %d\nOutline entries: %d\nLink annotations: %d (%d reconstructed from KFX)\n",
		pages, countPDFBookmarks(bookmarks), linkCount, leafportLinks)
	fmt.Fprintf(stdout, "Link targets: %d external, %d direct internal, %d named internal, %d unresolved\n",
		targets.external, targets.direct, targets.named, targets.unresolved)
	if len(targets.unresolvedKinds) != 0 {
		keys := make([]string, 0, len(targets.unresolvedKinds))
		for key := range targets.unresolvedKinds {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var details []string
		for _, key := range keys {
			details = append(details, fmt.Sprintf("%s=%d", key, targets.unresolvedKinds[key]))
		}
		fmt.Fprintf(stdout, "Unresolved kinds: %s\n", strings.Join(details, ", "))
	}
	return nil
}

func countPDFBookmarks(bookmarks []pdfcpu.Bookmark) int {
	count := 0
	for _, bookmark := range bookmarks {
		count += 1 + countPDFBookmarks(bookmark.Kids)
	}
	return count
}

type pdfLinkTargets struct {
	external        int
	direct          int
	named           int
	unresolved      int
	unresolvedKinds map[string]int
}

func inspectPDFLinkTargets(path string, pageCount int) (pdfLinkTargets, error) {
	result := pdfLinkTargets{unresolvedKinds: make(map[string]int)}
	file, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer file.Close()
	configuration := model.NewDefaultConfiguration()
	configuration.ValidationMode = model.ValidationRelaxed
	context, err := api.ReadAndValidate(file, configuration)
	if err != nil {
		return result, err
	}
	for pageNumber := 1; pageNumber <= pageCount; pageNumber++ {
		page, _, _, err := context.PageDict(pageNumber, false)
		if err != nil {
			return result, err
		}
		annotations, err := context.DereferenceArray(page["Annots"])
		if err != nil {
			return result, err
		}
		for _, annotationObject := range annotations {
			annotation, err := context.DereferenceDict(annotationObject)
			if err != nil {
				return result, err
			}
			if annotation == nil || annotation.NameEntry("Subtype") == nil || *annotation.NameEntry("Subtype") != "Link" {
				continue
			}
			destination, hasDestination := annotation["Dest"]
			if !hasDestination {
				action, actionErr := context.DereferenceDict(annotation["A"])
				if actionErr != nil {
					result.unresolved++
					result.unresolvedKinds["invalid-action"]++
					continue
				}
				if action != nil && action.NameEntry("S") != nil && *action.NameEntry("S") == "URI" {
					result.external++
					continue
				}
				if action == nil || action.NameEntry("S") == nil || *action.NameEntry("S") != "GoTo" {
					result.unresolved++
					kind := "missing-action"
					if action != nil && action.NameEntry("S") != nil {
						kind = "action-" + *action.NameEntry("S")
					}
					result.unresolvedKinds[kind]++
					continue
				}
				destination, hasDestination = action["D"]
			}
			if !hasDestination {
				result.unresolved++
				result.unresolvedKinds["missing-destination"]++
				continue
			}
			page, pageErr := pdfcpu.PageNrFromDestination(context, destination)
			if pageErr != nil || page < 1 || page > pageCount {
				result.unresolved++
				result.unresolvedKinds[fmt.Sprintf("destination-%T", destination)]++
				continue
			}
			switch destination.(type) {
			case types.Array:
				result.direct++
			default:
				result.named++
			}
		}
	}
	return result, nil
}
