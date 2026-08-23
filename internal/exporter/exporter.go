// Package exporter runs the native bridge and writes decrypted book archives.
package exporter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"leafport/internal/kfxdrm"
	"leafport/internal/library"
	"leafport/internal/machoutil"
	"leafport/internal/runtimebridge"
)

// RuntimeCommand identifies the private invocation used by the disposable
// bridge copy. It is not part of the user-facing CLI.
const RuntimeCommand = "__leafport_runtime"

// Config contains process-wide dependencies for one export operation.
type Config struct {
	AppPath string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
}

// Export decrypts one book through a short-lived, target-local bridge process.
func Export(ctx context.Context, config Config, output library.Output, workRoot string) (err error) {
	if output.Book.Preferences == "" {
		return errors.New("reader preferences path is unavailable")
	}
	if _, err := os.Stat(output.Book.Preferences); err != nil {
		return fmt.Errorf("required component %s: %w", output.Book.Preferences, err)
	}
	temporary, err := os.MkdirTemp(workRoot, library.SafeName(output.Book.ID)+"-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(temporary)) }()
	if err := os.Chmod(temporary, 0o700); err != nil {
		return err
	}
	runtimeDylib, err := prepareRuntime(config.AppPath, temporary)
	if err != nil {
		return err
	}
	bridge, err := prepareCatalystSelf(temporary)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(temporary, "Library/Caches/logs"), 0o700); err != nil {
		return err
	}
	arguments := []string{
		RuntimeCommand, runtimeDylib, output.Book.Path, output.Book.Preferences,
		filepath.Join(config.AppPath, "Contents/Resources"), output.Path,
	}
	command := exec.CommandContext(ctx, bridge, arguments...)
	command.Dir = filepath.Dir(output.Path)
	command.Env = replaceEnvironment(os.Environ(), "CFFIXED_USER_HOME", temporary)
	command.Stdin = config.Stdin
	command.Stdout = config.Stdout
	command.Stderr = config.Stderr
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("disposable Go Mac Catalyst bridge: %w", err)
	}
	return nil
}

// RunRuntime handles the hidden bridge invocation inside the disposable copy.
func RunRuntime(arguments []string, output io.Writer) error {
	if len(arguments) != 5 {
		return errors.New("invalid internal runtime arguments")
	}
	runtimeDylib, bookPath, preferences, resources, destination :=
		arguments[0], arguments[1], arguments[2], arguments[3], arguments[4]
	capture, err := runtimebridge.OpenBook(runtimeDylib, bookPath, preferences, resources)
	if err != nil {
		return fmt.Errorf("reader runtime bridge failed: %w", err)
	}
	defer clear(capture.Key)
	fmt.Fprintf(output, "Captured one content key across %d decrypt operations\n", capture.Uses)
	stats, err := kfxdrm.DecryptBundle(bookPath, destination, capture.Key)
	if err != nil {
		return fmt.Errorf("standalone KFX decryption failed: %w", err)
	}
	info, err := os.Stat(destination)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "Completed: %s (%d bytes; %d DRMION records, %d pages)\n",
		destination, info.Size(), stats.EncryptedRecords, stats.Pages)
	return nil
}

func replaceEnvironment(environment []string, name, value string) []string {
	prefix := name + "="
	result := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		if !strings.HasPrefix(item, prefix) {
			result = append(result, item)
		}
	}
	return append(result, prefix+value)
}

func prepareRuntime(appPath, temporary string) (string, error) {
	runtimeDylib := filepath.Join(temporary, "LeafportRuntime.dylib")
	data, err := os.ReadFile(filepath.Join(appPath, "Contents/MacOS/Kindle"))
	if err != nil {
		return "", err
	}
	data, err = machoutil.ThinARM64(data)
	if err != nil {
		return "", err
	}
	if err := machoutil.Dylibify(data); err != nil {
		return "", err
	}
	data, err = machoutil.AdHocSign(data, "LeafportRuntime")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(runtimeDylib, data, 0o700); err != nil {
		return "", err
	}
	return runtimeDylib, nil
}

func prepareCatalystSelf(temporary string) (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		return "", err
	}
	data, err = machoutil.ThinARM64(data)
	if err != nil {
		return "", fmt.Errorf("prepare Go runtime bridge: %w", err)
	}
	if err := machoutil.SetMacCatalystPlatform(data); err != nil {
		return "", fmt.Errorf("prepare Go runtime bridge: %w", err)
	}
	data, err = machoutil.AdHocSign(data, "LeafportBridge")
	if err != nil {
		return "", fmt.Errorf("sign Go runtime bridge: %w", err)
	}
	destination := filepath.Join(temporary, "leafport-bridge")
	if err := os.WriteFile(destination, data, 0o700); err != nil {
		return "", err
	}
	return destination, nil
}
