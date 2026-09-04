package kfxconvert

import "bytes"

// fixedLayoutPagesWithCover guarantees that every page-oriented output starts
// with an internal cover. A declared KFX cover wins; otherwise the first page
// with non-empty payload is the fallback. Existing pages are moved, not copied,
// so the cover is never duplicated.
func fixedLayoutPagesWithCover(book *decodedBook, pages []Page) ([]Page, int) {
	ordered := append([]Page(nil), pages...)
	coverID := uint32(0)
	if book != nil {
		coverID = book.coverResourceID()
	}
	if coverID != 0 {
		for index, page := range ordered {
			if page.ResourceID != coverID {
				continue
			}
			if index > 0 {
				ordered = movePageFirst(ordered, index)
			}
			return ordered, 0
		}
		if resource, data, ok := book.resolveResource(coverID); ok && len(bytes.TrimSpace(data)) != 0 {
			cover := Page{
				ResourceID: coverID, Format: resource.format, Location: resource.location,
				PageIndex: resource.pageIndex, Width: resource.width, Height: resource.height, Data: data,
			}
			return append([]Page{cover}, ordered...), 0
		}
	}
	for index, page := range ordered {
		if len(bytes.TrimSpace(page.Data)) == 0 {
			continue
		}
		if index > 0 {
			ordered = movePageFirst(ordered, index)
		}
		return ordered, 0
	}
	return ordered, -1
}

func movePageFirst(pages []Page, index int) []Page {
	selected := pages[index]
	copy(pages[1:index+1], pages[:index])
	pages[0] = selected
	return pages
}
