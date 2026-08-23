//go:build !darwin

package bookmeta

import (
	"context"
	"errors"
)

func Title(_ context.Context, _, _ string) (string, error) {
	return "", errors.New("Kindle metadata lookup requires macOS")
}
