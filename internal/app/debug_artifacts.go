package app

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jxsl13/leafport/internal/exporter"
)

func prepareDebugRoot(target string, now time.Time) (string, error) {
	base := filepath.Join(target, "debug")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("create debug directory: %w", err)
	}
	if err := os.Chmod(base, 0o700); err != nil {
		return "", fmt.Errorf("protect debug directory: %w", err)
	}
	root, err := os.MkdirTemp(base, now.UTC().Format("20060102T150405.000000000Z")+"-")
	if err != nil {
		return "", fmt.Errorf("create debug run directory: %w", err)
	}
	return root, nil
}

func preserveEncryptedBundle(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("source bundle is not a directory: %s", source)
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return err
	}
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("source entry escapes bundle: %s", path)
		}
		target := filepath.Join(destination, relative)
		mode := entry.Type()
		switch {
		case mode.IsDir():
			return os.Mkdir(target, 0o700)
		case mode.IsRegular():
			return copyPrivateFile(path, target)
		case mode&os.ModeSymlink != 0:
			return fmt.Errorf("refusing symbolic link in source bundle: %s", path)
		default:
			return fmt.Errorf("unsupported source entry %s (%s)", path, mode)
		}
	})
}

func preserveDecryptedArchive(destination string, archive exporter.Archive) (err error) {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		err = errors.Join(err, output.Close())
		if !complete {
			err = errors.Join(err, os.Remove(destination))
		}
	}()
	if archive.Path != "" {
		input, openErr := os.Open(archive.Path)
		if openErr != nil {
			return openErr
		}
		_, copyErr := io.Copy(output, input)
		closeErr := input.Close()
		if copyErr != nil || closeErr != nil {
			return errors.Join(copyErr, closeErr)
		}
	} else if _, err := output.Write(archive.Data); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	complete = true
	return nil
}

func copyPrivateFile(source, destination string) (err error) {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, input.Close()) }()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		err = errors.Join(err, output.Close())
		if !complete {
			err = errors.Join(err, os.Remove(destination))
		}
	}()
	if _, err := io.Copy(output, input); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	complete = true
	return nil
}
