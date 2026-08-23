//go:build !darwin

package bookmeta

import (
	"context"
	"errors"
)

type Store struct{}

func Open(_ context.Context, _ string) (*Store, error) {
	return nil, errors.New("Kindle metadata lookup requires macOS")
}

func (store *Store) Close() error { return nil }

func (store *Store) Title(_ context.Context, _ string) (string, error) {
	return "", errors.New("Kindle metadata lookup requires macOS")
}

func Title(_ context.Context, _, _ string) (string, error) {
	return "", errors.New("Kindle metadata lookup requires macOS")
}
