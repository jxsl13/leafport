// Package exporter runs the native bridge and writes decrypted book archives.
package exporter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jxsl13/leafport/internal/kfxdrm"
	"github.com/jxsl13/leafport/internal/library"
	"github.com/jxsl13/leafport/internal/machoutil"
	"github.com/jxsl13/leafport/internal/runtimebridge"
)

// RuntimeCommand identifies the private invocation used by the disposable
// bridge copy. It is not part of the user-facing CLI.
const RuntimeCommand = "__leafport_runtime"

const archiveMemoryLimit int64 = 1 << 30

// DoctorRuntimeCommand identifies the private compatibility-check invocation.
const DoctorRuntimeCommand = "__leafport_doctor_runtime"

// Config contains process-wide dependencies for one export operation.
type Config struct {
	AppPath       string
	Preferences   string
	AccountSecret string
	Stdin         io.Reader
	Stdout        io.Writer
	Stderr        io.Writer
}

// Archive is a decrypted intermediate held either in memory or, above the
// one-GiB limit, in a private file below the batch work directory.
type Archive struct {
	Data []byte
	Path string
	Size int64
}

// Export decrypts one book through a short-lived bridge process. The KFX
// archive stays in memory up to one GiB and spills privately above that limit.
func Export(ctx context.Context, config Config, output library.Output, workRoot string) (result Archive, err error) {
	if output.Book.Preferences == "" {
		return result, errors.New("reader preferences path is unavailable")
	}
	if _, err := os.Stat(output.Book.Preferences); err != nil {
		return result, fmt.Errorf("required component %s: %w", output.Book.Preferences, err)
	}
	temporary, err := os.MkdirTemp(workRoot, library.SafeName(output.Book.ID)+"-")
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(temporary)) }()
	if err := os.Chmod(temporary, 0o700); err != nil {
		return result, err
	}
	runtimeDylib, err := prepareRuntime(config.AppPath, temporary)
	if err != nil {
		return result, err
	}
	bridge, err := prepareCatalystSelf(config.AppPath, temporary, false)
	if err != nil {
		return result, err
	}
	if err := os.MkdirAll(filepath.Join(temporary, "Library/Caches/logs"), 0o700); err != nil {
		return result, err
	}
	if err := prepareReaderHome(temporary, output.Book.Preferences); err != nil {
		return result, err
	}
	readPipe, writePipe, err := os.Pipe()
	if err != nil {
		return result, err
	}
	defer readPipe.Close()
	defer writePipe.Close()
	secretDescriptor := "0"
	var secretRead, secretWrite *os.File
	if config.AccountSecret != "" {
		secretRead, secretWrite, err = os.Pipe()
		if err != nil {
			return result, err
		}
		defer secretRead.Close()
		defer secretWrite.Close()
		secretDescriptor = "4"
	}
	arguments := []string{
		RuntimeCommand, runtimeDylib, output.Book.Path, output.Book.Preferences,
		filepath.Join(config.AppPath, "Contents/Resources"), "3", secretDescriptor,
	}
	command := exec.CommandContext(ctx, bridge, arguments...)
	command.Dir = temporary
	command.Env = replaceEnvironment(removeEnvironment(os.Environ(), "LEAFPORT_ACCOUNT_SECRET"), "CFFIXED_USER_HOME", temporary)
	command.ExtraFiles = []*os.File{writePipe}
	if secretRead != nil {
		command.ExtraFiles = append(command.ExtraFiles, secretRead)
	}
	command.Stdin = config.Stdin
	command.Stdout = config.Stdout
	var runtimeStderr bytes.Buffer
	command.Stderr = &runtimeStderr
	if err := command.Start(); err != nil {
		return result, err
	}
	if err := writePipe.Close(); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return result, err
	}
	if secretRead != nil {
		if err := secretRead.Close(); err != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			return result, err
		}
		if _, err := io.WriteString(secretWrite, config.AccountSecret); err != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			return result, err
		}
		if err := secretWrite.Close(); err != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			return result, err
		}
	}
	type readResult struct {
		archive Archive
		err     error
	}
	resultChannel := make(chan readResult, 1)
	go func() {
		spill := &archiveSpillWriter{limit: archiveMemoryLimit, workRoot: workRoot}
		_, readErr := io.Copy(spill, readPipe)
		archive, finishErr := spill.finish()
		resultChannel <- readResult{archive: archive, err: errors.Join(readErr, finishErr)}
	}()
	waitErr := command.Wait()
	read := <-resultChannel
	if diagnostics := filterRuntimeDiagnostics(runtimeStderr.String()); diagnostics != "" && config.Stderr != nil {
		fmt.Fprint(config.Stderr, diagnostics)
	}
	if waitErr != nil {
		if read.archive.Path != "" {
			_ = os.Remove(read.archive.Path)
		}
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, fmt.Errorf("disposable Go Mac Catalyst bridge: %w", waitErr)
	}
	if read.err != nil {
		if read.archive.Path != "" {
			_ = os.Remove(read.archive.Path)
		}
		return result, fmt.Errorf("read decrypted archive from bridge: %w", read.err)
	}
	if read.archive.Size == 0 {
		return result, errors.New("disposable bridge returned an empty archive")
	}
	return read.archive, nil
}

func filterRuntimeDiagnostics(value string) string {
	var output strings.Builder
	for _, line := range strings.SplitAfter(value, "\n") {
		if index := strings.Index(line, "leafport:"); index >= 0 {
			output.WriteString(line[index:])
			if !strings.HasSuffix(line, "\n") {
				output.WriteByte('\n')
			}
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "objc[") ||
			strings.Contains(trimmed, "[BugsnagPerformance]") ||
			strings.HasPrefix(trimmed, "No Observer retrieved") ||
			strings.HasPrefix(trimmed, "FastMetricsClientMobile:") {
			continue
		}
		output.WriteString(line)
		if !strings.HasSuffix(line, "\n") {
			output.WriteByte('\n')
		}
	}
	return output.String()
}

type archiveSpillWriter struct {
	limit    int64
	workRoot string
	buffer   bytes.Buffer
	file     *os.File
	path     string
	size     int64
}

func (writer *archiveSpillWriter) Write(data []byte) (int, error) {
	if writer.file == nil && writer.size+int64(len(data)) > writer.limit {
		file, err := os.CreateTemp(writer.workRoot, ".leafport-archive-*.kfx-zip")
		if err != nil {
			return 0, err
		}
		writer.file = file
		writer.path = file.Name()
		if _, err := writer.buffer.WriteTo(file); err != nil {
			return 0, err
		}
	}
	var (
		written int
		err     error
	)
	if writer.file != nil {
		written, err = writer.file.Write(data)
	} else {
		written, err = writer.buffer.Write(data)
	}
	writer.size += int64(written)
	return written, err
}

func (writer *archiveSpillWriter) finish() (Archive, error) {
	if writer.file == nil {
		return Archive{Data: writer.buffer.Bytes(), Size: writer.size}, nil
	}
	if err := writer.file.Sync(); err != nil {
		_ = writer.file.Close()
		_ = os.Remove(writer.path)
		return Archive{}, err
	}
	if err := writer.file.Close(); err != nil {
		_ = os.Remove(writer.path)
		return Archive{}, err
	}
	writer.file = nil
	return Archive{Path: writer.path, Size: writer.size}, nil
}

// RunRuntime handles the hidden bridge invocation inside the disposable copy.
func RunRuntime(arguments []string, output io.Writer) error {
	if len(arguments) != 6 {
		return errors.New("invalid internal runtime arguments")
	}
	runtimeDylib, bookPath, preferences, resources, descriptor, secretDescriptor :=
		arguments[0], arguments[1], arguments[2], arguments[3], arguments[4], arguments[5]
	fileDescriptor, err := strconv.ParseUint(descriptor, 10, 32)
	if err != nil || fileDescriptor < 3 {
		return errors.New("invalid internal archive descriptor")
	}
	archive := os.NewFile(uintptr(fileDescriptor), "leafport-decrypted-archive")
	if archive == nil {
		return errors.New("internal archive descriptor is unavailable")
	}
	defer archive.Close()
	accountSecret, err := readAccountSecret(secretDescriptor)
	if err != nil {
		return err
	}
	capture, err := runtimebridge.OpenBookWithAccountSecret(
		runtimeDylib, bookPath, preferences, resources, accountSecret)
	if err != nil {
		return fmt.Errorf("reader runtime bridge failed: %w", err)
	}
	defer clear(capture.Key)
	fmt.Fprintf(output, "Captured one content key across %d decrypt operations\n", capture.Uses)
	if !capture.KnownProfile {
		fmt.Fprintf(output, "Warning: unknown reader build passed validated fallback (%s)\n", capture.Fingerprint)
	}
	validation, err := kfxdrm.ValidateBundleKey(bookPath, capture.Key)
	if err != nil {
		return fmt.Errorf("captured content-key validation failed: %w", err)
	}
	fmt.Fprintf(output, "Validated content key against %d encrypted record(s), %d page(s)\n",
		validation.Records, validation.Pages)
	counter := &countingWriter{writer: archive}
	stats, err := kfxdrm.DecryptBundleTo(bookPath, counter, capture.Key)
	if err != nil {
		return fmt.Errorf("standalone KFX decryption failed: %w", err)
	}
	fmt.Fprintf(output, "Streamed decrypted archive (%d bytes; %d DRMION records, %d pages)\n",
		counter.written, stats.EncryptedRecords, stats.Pages)
	return nil
}

func readAccountSecret(descriptor string) (string, error) {
	if descriptor == "0" {
		return "", nil
	}
	fileDescriptor, err := strconv.ParseUint(descriptor, 10, 32)
	if err != nil || fileDescriptor < 3 {
		return "", errors.New("invalid internal account-secret descriptor")
	}
	file := os.NewFile(uintptr(fileDescriptor), "leafport-account-secret")
	if file == nil {
		return "", errors.New("internal account-secret descriptor is unavailable")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 41))
	if err != nil {
		return "", fmt.Errorf("read supplied account secret: %w", err)
	}
	secret := strings.TrimSpace(string(data))
	clear(data)
	if len(secret) != 40 {
		return "", fmt.Errorf("supplied account-secret length is %d; expected 40", len(secret))
	}
	return secret, nil
}

type countingWriter struct {
	writer  io.Writer
	written int64
}

func (writer *countingWriter) Write(data []byte) (int, error) {
	written, err := writer.writer.Write(data)
	writer.written += int64(written)
	return written, err
}

// Doctor performs the dynamic compatibility checks in the same disposable Mac
// Catalyst bridge used for exports. Its private artifacts are always removed.
func Doctor(ctx context.Context, config Config) (report runtimebridge.DoctorReport, err error) {
	temporary, err := os.MkdirTemp("", ".leafport-doctor-")
	if err != nil {
		return report, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(temporary)) }()
	if err := os.Chmod(temporary, 0o700); err != nil {
		return report, err
	}
	runtimeDylib, err := prepareRuntime(config.AppPath, temporary)
	if err != nil {
		return report, err
	}
	bridge, err := prepareCatalystSelf(config.AppPath, temporary, false)
	if err != nil {
		return report, err
	}
	if err := os.MkdirAll(filepath.Join(temporary, "Library/Caches/logs"), 0o700); err != nil {
		return report, err
	}
	if err := prepareReaderHome(temporary, config.Preferences); err != nil {
		return report, err
	}
	report, err = runDoctorBridge(ctx, config, bridge, runtimeDylib, temporary)
	if err != nil {
		return report, err
	}
	// A copied restricted keychain access group cannot be trusted merely
	// because it is present in an ad-hoc signature. Probe it in a second,
	// disposable process and preserve only success/failure—not the credential.
	// macOS commonly terminates this process during signature validation.
	if !report.AccountSecretAvailable && config.Preferences != "" {
		report.EntitlementProbeAttempted = true
		entitledRoot := filepath.Join(temporary, "entitlement-probe")
		entitledBridge, prepareErr := prepareCatalystSelf(config.AppPath, entitledRoot, true)
		if prepareErr != nil {
			report.EntitlementProbeError = prepareErr.Error()
		} else {
			entitledReport, probeErr := runDoctorBridge(ctx, config, entitledBridge, runtimeDylib, temporary)
			if probeErr != nil {
				report.EntitlementProbeError = probeErr.Error()
			} else if entitledReport.AccountSecretAvailable {
				report.AccountSecretAvailable = true
				report.AccountSecretProbeError = ""
			} else {
				report.EntitlementProbeError = entitledReport.AccountSecretProbeError
				if report.EntitlementProbeError == "" {
					report.EntitlementProbeError = "reader-entitled bridge returned no usable raw account secret"
				}
			}
		}
	}
	return report, nil
}

func runDoctorBridge(ctx context.Context, config Config, bridge, runtimeDylib, home string) (runtimebridge.DoctorReport, error) {
	var report runtimebridge.DoctorReport
	command := exec.CommandContext(ctx, bridge, DoctorRuntimeCommand, runtimeDylib, config.Preferences)
	command.Dir = home
	command.Env = replaceEnvironment(removeEnvironment(os.Environ(), "LEAFPORT_ACCOUNT_SECRET"), "CFFIXED_USER_HOME", home)
	command.Stdin = config.Stdin
	var output bytes.Buffer
	var runtimeStderr bytes.Buffer
	command.Stdout = &output
	command.Stderr = &runtimeStderr
	if err := command.Run(); err != nil {
		if config.Stderr != nil && runtimeStderr.Len() != 0 {
			_, _ = io.Copy(config.Stderr, &runtimeStderr)
		}
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		return report, fmt.Errorf("disposable Go Mac Catalyst doctor bridge: %w", err)
	}
	// Some private frameworks write informational lines to stdout during
	// process teardown. Decode the first protocol value and ignore that trailing
	// noise instead of requiring the entire stream to be JSON.
	if err := json.NewDecoder(&output).Decode(&report); err != nil {
		return report, fmt.Errorf("decode doctor bridge report: %w", err)
	}
	return report, nil
}

// RunDoctorRuntime handles the hidden doctor invocation inside the disposable
// bridge copy.
func RunDoctorRuntime(arguments []string, output io.Writer) error {
	if len(arguments) != 2 {
		return errors.New("invalid internal doctor runtime arguments")
	}
	report, err := runtimebridge.Doctor(arguments[0], arguments[1])
	if err != nil {
		return fmt.Errorf("reader runtime doctor failed: %w", err)
	}
	if err := json.NewEncoder(output).Encode(report); err != nil {
		return fmt.Errorf("encode doctor bridge report: %w", err)
	}
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

func removeEnvironment(environment []string, name string) []string {
	prefix := name + "="
	result := make([]string, 0, len(environment))
	for _, item := range environment {
		if !strings.HasPrefix(item, prefix) {
			result = append(result, item)
		}
	}
	return result
}

func prepareReaderHome(temporary, preferencesPath string) error {
	preferencesDirectory := filepath.Join(temporary, "Library", "Preferences")
	if err := os.MkdirAll(preferencesDirectory, 0o700); err != nil {
		return err
	}
	preferences, err := os.ReadFile(preferencesPath)
	if err != nil {
		return fmt.Errorf("read reader preferences: %w", err)
	}
	if err := os.WriteFile(filepath.Join(preferencesDirectory, "com.amazon.Lassen.plist"), preferences, 0o600); err != nil {
		return fmt.Errorf("prepare isolated reader preferences: %w", err)
	}

	return nil
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

func prepareCatalystSelf(appPath, temporary string, readerEntitlements bool) (string, error) {
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
	identifier := "LeafportBridge"
	var entitlements []byte
	if readerEntitlements {
		reader, readErr := os.ReadFile(filepath.Join(appPath, "Contents", "MacOS", "Kindle"))
		if readErr != nil {
			return "", fmt.Errorf("read reader entitlements: %w", readErr)
		}
		reader, readErr = machoutil.ThinARM64(reader)
		if readErr != nil {
			return "", fmt.Errorf("select reader entitlements: %w", readErr)
		}
		entitlements, readErr = machoutil.EmbeddedEntitlements(reader)
		if readErr != nil {
			return "", fmt.Errorf("extract reader entitlements: %w", readErr)
		}
		identifier = "com.amazon.Lassen"
	}
	data, err = machoutil.AdHocSignWithEntitlements(data, identifier, entitlements)
	if err != nil {
		return "", fmt.Errorf("sign Go runtime bridge: %w", err)
	}
	bundle := filepath.Join(temporary, "LeafportBridge.app", "Contents")
	macOS := filepath.Join(bundle, "MacOS")
	if err := os.MkdirAll(macOS, 0o700); err != nil {
		return "", err
	}
	info, err := os.ReadFile(filepath.Join(appPath, "Contents", "Info.plist"))
	if err != nil {
		return "", fmt.Errorf("read reader bundle metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "Info.plist"), info, 0o600); err != nil {
		return "", fmt.Errorf("prepare bridge bundle metadata: %w", err)
	}
	if err := os.Symlink(filepath.Join(appPath, "Contents", "Resources"), filepath.Join(bundle, "Resources")); err != nil {
		return "", fmt.Errorf("prepare bridge bundle resources: %w", err)
	}
	// Preserve the executable name declared by the reader's Info.plist so that
	// NSBundle treats this disposable copy as an application bundle. Private
	// runtime components then resolve their resource manifests without copying
	// the full application into the target work directory.
	destination := filepath.Join(macOS, "Kindle")
	if err := os.WriteFile(destination, data, 0o700); err != nil {
		return "", err
	}
	return destination, nil
}
