//go:build !darwin

package runtimebridge

import "errors"

type Capture struct {
	Key  []byte
	Uses int
}

func OpenBook(_, _, _, _ string) (Capture, error) {
	return Capture{}, errors.New("Kindle runtime bridge requires macOS")
}
