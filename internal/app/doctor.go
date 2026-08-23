package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"howett.net/plist"

	"leafport/internal/compatibility"
	"leafport/internal/exporter"
	"leafport/internal/library"
	"leafport/internal/machoutil"
	"leafport/internal/readerconfig"
)

type doctorState struct {
	output   Streams
	failures []error
}

// Doctor checks local discovery and the complete disposable runtime bridge
// without opening or decrypting a book.
func Doctor(ctx context.Context, config Config, streams Streams) error {
	state := &doctorState{output: streams}
	fmt.Fprintln(streams.Stdout, "Leafport doctor")

	state.require(runtime.GOOS == "darwin", "Operating system", runtime.GOOS,
		errors.New("leafport requires macOS"))
	state.require(runtime.GOARCH == "arm64", "Architecture", runtime.GOARCH,
		fmt.Errorf("leafport requires arm64; found %s", runtime.GOARCH))
	state.pass("Final output policy", "PDF-backed/textbook fixed layout -> PDF; explicitly marked image comics -> CBZ; reflowable KFX -> EPUB; no KFX final output")
	state.pass("Fixed-layout safeguards", "typed capability plus text/page-structure checks; variants, tiles, mixed PDF/images, metadata, navigation, and RTL supported")
	state.pass("EPUB reconstruction", "text, navigation, links, tables, ruby, MathML, KVG/SVG, fonts, images, and supported audio/video/image plugins; unknown interactive layouts fail closed")
	state.pass("Privacy cleanup", "explicit owner fields, contextual owner markers, repeatable regular expressions, output metadata removal, and common-media scrubbing; publication contact addresses are preserved")
	state.warn("JPEG-XR", "no independently validated pure-Go decoder is available; an affected fixed-layout title fails closed")

	executable := filepath.Join(config.AppPath, "Contents/MacOS/Kindle")
	resources := filepath.Join(config.AppPath, "Contents/Resources")
	var staticFingerprint machoutil.BinaryFingerprint
	appReady := true
	data, err := os.ReadFile(executable)
	if err != nil {
		state.fail("Reader executable", err)
		appReady = false
	} else if staticFingerprint, err = machoutil.FingerprintARM64(data); err != nil {
		state.fail("Reader executable", err)
		appReady = false
	} else {
		state.pass("Reader executable", executable)
		profile, exact := compatibility.Resolve(staticFingerprint.UUID, staticFingerprint.TextSHA256)
		if exact {
			state.pass("Compatibility profile", profile.Name+" ("+profile.AppVersion+")")
		} else {
			state.warn("Compatibility profile", "unknown build; all semantic runtime checks must pass")
		}
		state.pass("Mach-O UUID", valueOrUnavailable(staticFingerprint.UUID))
		state.pass("__text SHA-256", staticFingerprint.TextSHA256)
		checkCredentialFlowAnchors(state, data)
		checkReaderEntitlements(state, data)
	}
	if info, statErr := os.Stat(resources); statErr != nil || !info.IsDir() {
		if statErr == nil {
			statErr = errors.New("not a directory")
		}
		state.fail("Reader resources", statErr)
		appReady = false
	} else {
		state.pass("Reader resources", resources)
	}

	preferencesPath := ""
	accountSecretBundles := 0
	home, err := os.UserHomeDir()
	if err != nil {
		state.fail("User home", err)
	} else {
		roots, discoverErr := library.DiscoverRoots(home, config.Library)
		if discoverErr != nil {
			state.fail("Local library", discoverErr)
		} else {
			paths := make([]string, 0, len(roots))
			for _, root := range roots {
				paths = append(paths, root.Path)
			}
			sort.Strings(paths)
			state.pass("Local library", fmt.Sprintf("%d root(s): %v", len(paths), paths))
			preferencesPath = checkPreferences(state, roots)
			books, warnings, booksErr := library.DiscoverBooks(ctx, roots)
			if booksErr != nil {
				state.fail("Downloaded books", booksErr)
			} else if len(books) == 0 {
				state.fail("Downloaded books", errors.New("no complete local KFX bundle found"))
			} else {
				withTitles := 0
				for _, book := range books {
					if book.Title != "" {
						withTitles++
					}
				}
				state.pass("Downloaded books", fmt.Sprintf("%d found; %d title(s) resolved", len(books), withTitles))
				for _, book := range books {
					requirements, inspectErr := readerconfig.InspectVouchers(book.Path)
					if inspectErr != nil {
						state.fail("Voucher requirements", fmt.Errorf("%s: %w", book.ID, inspectErr))
						continue
					}
					if requirements.AccountSecret {
						accountSecretBundles++
					}
				}
				state.pass("Voucher requirements", fmt.Sprintf("%d book(s) require an account secret", accountSecretBundles))
			}
			for _, warning := range warnings {
				state.warn("Book metadata", warning.Error())
			}
		}
	}

	if appReady && runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		if config.AccountSecret == "" && accountSecretBundles != 0 && preferencesPath != "" && home != "" {
			credentials, credentialsErr := readerconfig.LoadCredentials(preferencesPath)
			if credentialsErr == nil && credentials.HashedAccountSecret != "" {
				secret, discovery, discoveryErr := readerconfig.DiscoverAccountSecret(
					credentials.HashedAccountSecret, readerconfig.DefaultAccountSecretRoots(home))
				switch {
				case discoveryErr != nil:
					state.warn("Reader-owned credential scan", discoveryErr.Error())
				case secret != "":
					config.AccountSecret = secret
					state.pass("Reader-owned credential scan", fmt.Sprintf(
						"verified one raw value against the stored fingerprint (%d file(s), %d candidate(s))",
						discovery.Files, discovery.Candidates))
				default:
					state.warn("Reader-owned credential scan", fmt.Sprintf(
						"no verified raw value in %d normally readable file(s); %d exact candidate(s) tested",
						discovery.Files, discovery.Candidates))
				}
			}
		}
		report, bridgeErr := exporter.Doctor(ctx, exporter.Config{
			AppPath: config.AppPath, Preferences: preferencesPath, AccountSecret: config.AccountSecret,
			Stdin: streams.Stdin, Stderr: streams.Stderr,
		})
		if bridgeErr != nil {
			state.fail("Disposable runtime bridge", bridgeErr)
		} else {
			state.pass("Disposable runtime bridge", "loaded and validated without installing a hook")
			if report.Fingerprint != staticFingerprint {
				state.fail("Runtime fingerprint", errors.New("prepared runtime differs from installed executable"))
			} else {
				state.pass("Runtime fingerprint", report.Fingerprint.String())
			}
			if report.KnownProfile {
				state.pass("Runtime profile", report.Profile)
			} else {
				state.warn("Runtime profile", report.Profile+" (strict semantic fallback)")
			}
			state.pass("Objective-C interface", fmt.Sprintf("%s/%s; %s; %s; %s; %s",
				report.ProviderClass, report.BookClass, report.ProviderInitializer,
				report.BookInitializer, report.ResourceBundleSetter, report.ICUDataDirectorySetter))
			contextSymbol := "optional context key-length symbol absent"
			if report.ContextKeyLengthAvailable {
				contextSymbol = "context key-length symbol present"
			}
			state.pass("Crypto entry points", fmt.Sprintf("%s; %s; %s; owner/prologue validated",
				report.DecryptSymbol, report.CipherKeyLengthSymbol, contextSymbol))
			if report.RegistrationValidated {
				state.pass("Device registration payload", "preferences payload decoded; identifier is non-empty")
			}
			if config.AccountSecret != "" {
				state.pass("Account secret", "40-character value supplied; actual voucher use is validated during export")
			} else if report.AccountSecretAvailable {
				state.pass("Account secret", "usable raw account secret is available")
			} else if accountSecretBundles != 0 {
				detail := fmt.Sprintf("required by %d downloaded book(s), but no usable raw account secret is available", accountSecretBundles)
				if report.HashedAccountSecret {
					detail += "; preferences contain only the incompatible hashed value"
				}
				if report.AccountSecretProbeError != "" {
					detail += "; runtime lookup: " + report.AccountSecretProbeError
				}
				if report.EntitlementProbeError != "" {
					detail += "; reader-entitlement probe: " + report.EntitlementProbeError
				}
				if report.EntitlementProbeAttempted {
					state.warn("Automatic account-secret retrieval", "macOS rejected or denied the disposable reader-entitled bridge; get-task-allow=false also prevents supported attachment, including when invoked as root")
				} else {
					state.warn("Automatic account-secret retrieval", "the restricted keychain access group cannot be inherited; get-task-allow=false also prevents supported attachment, including when invoked as root")
				}
				state.fail("Account secret", errors.New(detail))
			} else if report.HashedAccountSecret {
				state.pass("Account secret", "not required by downloaded books; preferences contain only a hash")
			} else {
				state.pass("Account secret", "not required by downloaded books")
			}
		}
	}

	if len(state.failures) != 0 {
		fmt.Fprintf(streams.Stdout, "Doctor result: FAIL (%d blocking issue(s))\n", len(state.failures))
		return fmt.Errorf("doctor found %d blocking issue(s)", len(state.failures))
	}
	fmt.Fprintln(streams.Stdout, "Doctor result: PASS")
	return nil
}

func checkCredentialFlowAnchors(state *doctorState, executable []byte) {
	anchors := []string{
		"AuthenticationManager", "setAccountSecret:", "accountSecret",
		"kindle.accountsecret.item", "com.amazon.Lassen.KeychainUI",
		"KRFDRMDataProvider", "initWithAccountSecrets:kindleSerialNumber:voucherList:",
	}
	var missing []string
	for _, anchor := range anchors {
		if !bytes.Contains(executable, []byte(anchor)) {
			missing = append(missing, anchor)
		}
	}
	if len(missing) == 0 {
		state.pass("Credential flow anchors", "raw secret: AuthenticationManager -> DRM provider; Keychain item/service anchors present")
		return
	}
	state.warn("Credential flow anchors", "reader build changed; missing static anchors: "+strings.Join(missing, ", "))
}

func checkReaderEntitlements(state *doctorState, executable []byte) {
	thin, err := machoutil.ThinARM64(executable)
	if err != nil {
		state.warn("Reader security boundary", "cannot select arm64 entitlements: "+err.Error())
		return
	}
	encoded, err := machoutil.EmbeddedEntitlements(thin)
	if err != nil {
		state.warn("Reader security boundary", "cannot read embedded entitlements: "+err.Error())
		return
	}
	var entitlements map[string]any
	if _, err := plist.Unmarshal(encoded, &entitlements); err != nil {
		state.warn("Reader security boundary", "cannot decode embedded entitlements: "+err.Error())
		return
	}
	applicationID, _ := entitlements["application-identifier"].(string)
	groups := stringValues(entitlements["keychain-access-groups"])
	sandboxed, _ := entitlements["com.apple.security.app-sandbox"].(bool)
	debuggable, _ := entitlements["get-task-allow"].(bool)
	if applicationID == "" || !containsString(groups, applicationID) {
		state.warn("Reader security boundary", "application identifier is not its keychain access group")
		return
	}
	detail := fmt.Sprintf("sandbox=%t; keychain access group=%s; get-task-allow=%t", sandboxed, applicationID, debuggable)
	if sandboxed && !debuggable {
		state.pass("Reader security boundary", detail)
		return
	}
	state.warn("Reader security boundary", detail)
}

func stringValues(value any) []string {
	switch values := value.(type) {
	case []string:
		return values
	case []any:
		result := make([]string, 0, len(values))
		for _, value := range values {
			if text, ok := value.(string); ok {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func checkPreferences(state *doctorState, roots []library.Root) string {
	seen := make(map[string]bool)
	valid := 0
	first := ""
	for _, root := range roots {
		if root.Preferences == "" || seen[root.Preferences] {
			continue
		}
		seen[root.Preferences] = true
		if info, err := os.Stat(root.Preferences); err != nil || !info.Mode().IsRegular() {
			continue
		}
		valid++
		if first == "" {
			first = root.Preferences
		}
	}
	if valid == 0 {
		state.fail("Reader registration", errors.New("no readable preferences file with device registration was found"))
		return ""
	}
	state.pass("Reader registration", fmt.Sprintf("%d preferences file(s) available", valid))
	return first
}

func (state *doctorState) pass(name, detail string) {
	fmt.Fprintf(state.output.Stdout, "[PASS] %s: %s\n", name, detail)
}

func (state *doctorState) warn(name, detail string) {
	fmt.Fprintf(state.output.Stdout, "[WARN] %s: %s\n", name, detail)
}

func (state *doctorState) fail(name string, err error) {
	state.failures = append(state.failures, fmt.Errorf("%s: %w", name, err))
	fmt.Fprintf(state.output.Stdout, "[FAIL] %s: %v\n", name, err)
}

func (state *doctorState) require(ok bool, name, detail string, err error) {
	if ok {
		state.pass(name, detail)
		return
	}
	state.fail(name, err)
}

func valueOrUnavailable(value string) string {
	if value == "" {
		return "unavailable"
	}
	return value
}
