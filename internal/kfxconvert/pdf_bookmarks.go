package kfxconvert

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

// writePDFBookmarks uses direct destination arrays. pdfcpu's higher-level
// bookmark writer keys named destinations by the visible title, which makes
// repeated headings resolve to the last page using that title.
func writePDFBookmarks(path string, bookmarks []pdfcpu.Bookmark) (err error) {
	input, err := os.Open(path)
	if err != nil {
		return err
	}
	configuration := pdfConfiguration()
	configuration.Cmd = model.ADDBOOKMARKS
	context, readErr := api.ReadValidateAndOptimize(input, configuration)
	closeErr := input.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	if _, err := pdfcpu.RemoveBookmarks(context); err != nil {
		return err
	}
	catalog, err := context.Catalog()
	if err != nil {
		return err
	}
	outlines := types.Dict{"Type": types.Name("Outlines")}
	outlinesReference, err := context.IndRefForNewObject(outlines)
	if err != nil {
		return err
	}
	first, last, count, err := createDirectOutlineItems(context, bookmarks, *outlinesReference)
	if err != nil {
		return err
	}
	if first == nil || last == nil {
		return errors.New("no usable PDF bookmarks")
	}
	outlines["First"] = *first
	outlines["Last"] = *last
	outlines["Count"] = types.Integer(count)
	catalog["Outlines"] = *outlinesReference

	temporary, err := os.CreateTemp(filepath.Dir(path), ".leafport-bookmarks-*.pdf")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	complete := false
	defer func() {
		if temporary != nil {
			err = errors.Join(err, temporary.Close())
		}
		if !complete {
			err = errors.Join(err, os.Remove(temporaryPath))
		}
	}()
	if err = api.WriteContext(context, temporary); err != nil {
		return err
	}
	if err = temporary.Sync(); err != nil {
		return err
	}
	if err = temporary.Close(); err != nil {
		temporary = nil
		return err
	}
	temporary = nil
	if err = os.Chmod(temporaryPath, 0o600); err != nil {
		return err
	}
	if err = os.Rename(temporaryPath, path); err != nil {
		return err
	}
	complete = true
	return nil
}

func createDirectOutlineItems(context *model.Context, bookmarks []pdfcpu.Bookmark, parent types.IndirectRef) (first, last *types.IndirectRef, total int, err error) {
	var previous *types.IndirectRef
	var previousDictionary types.Dict
	for _, bookmark := range bookmarks {
		if bookmark.PageFrom < 1 {
			return nil, nil, 0, fmt.Errorf("bookmark %q has invalid page %d", bookmark.Title, bookmark.PageFrom)
		}
		_, pageReference, _, pageErr := context.PageDict(bookmark.PageFrom, false)
		if pageErr != nil {
			return nil, nil, 0, pageErr
		}
		title, titleErr := types.EscapedUTF16String(bookmark.Title)
		if titleErr != nil {
			return nil, nil, 0, titleErr
		}
		dictionary := types.Dict{
			"Dest":   types.Array{*pageReference, types.Name("Fit")},
			"Parent": parent,
			"Title":  types.StringLiteral(*title),
		}
		if bookmark.Color != nil {
			dictionary["C"] = types.Array{
				types.Float(bookmark.Color.R), types.Float(bookmark.Color.G), types.Float(bookmark.Color.B),
			}
		}
		if style := bookmark.Style(); style != 0 {
			dictionary["F"] = types.Integer(style)
		}
		reference, referenceErr := context.IndRefForNewObject(dictionary)
		if referenceErr != nil {
			return nil, nil, 0, referenceErr
		}
		if first == nil {
			first = reference
		}
		if previous != nil {
			dictionary["Prev"] = *previous
			previousDictionary["Next"] = *reference
		}
		if len(bookmark.Kids) != 0 {
			childFirst, childLast, childCount, childErr := createDirectOutlineItems(context, bookmark.Kids, *reference)
			if childErr != nil {
				return nil, nil, 0, childErr
			}
			if childFirst != nil {
				dictionary["First"] = *childFirst
				dictionary["Last"] = *childLast
				dictionary["Count"] = types.Integer(childCount)
				total += childCount
			}
		}
		total++
		previous, previousDictionary = reference, dictionary
	}
	return first, previous, total, nil
}
