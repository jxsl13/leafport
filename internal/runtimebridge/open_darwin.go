//go:build darwin

package runtimebridge

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
	"golang.org/x/arch/arm64/arm64asm"

	"github.com/jxsl13/leafport/internal/compatibility"
	"github.com/jxsl13/leafport/internal/machoutil"
	"github.com/jxsl13/leafport/internal/readerconfig"
)

const foundationPath = "/System/Library/Frameworks/Foundation.framework/Foundation"

const inlineHookSize = 16

type Capture struct {
	Key          []byte
	Uses         int
	Profile      string
	KnownProfile bool
	Fingerprint  machoutil.BinaryFingerprint
}

// DoctorReport describes the independent compatibility checks completed by
// the disposable runtime bridge.
type DoctorReport struct {
	Fingerprint               machoutil.BinaryFingerprint `json:"fingerprint"`
	Profile                   string                      `json:"profile"`
	KnownProfile              bool                        `json:"knownProfile"`
	ProviderClass             string                      `json:"providerClass"`
	BookClass                 string                      `json:"bookClass"`
	ProviderInitializer       string                      `json:"providerInitializer"`
	BookInitializer           string                      `json:"bookInitializer"`
	ResourceBundleSetter      string                      `json:"resourceBundleSetter"`
	ICUDataDirectorySetter    string                      `json:"icuDataDirectorySetter"`
	DecryptSymbol             string                      `json:"decryptSymbol"`
	CipherKeyLengthSymbol     string                      `json:"cipherKeyLengthSymbol"`
	ContextKeyLengthAvailable bool                        `json:"contextKeyLengthAvailable"`
	RegistrationValidated     bool                        `json:"registrationValidated"`
	AccountSecretAvailable    bool                        `json:"accountSecretAvailable"`
	HashedAccountSecret       bool                        `json:"hashedAccountSecret"`
	AccountSecretProbeError   string                      `json:"accountSecretProbeError,omitempty"`
	EntitlementProbeAttempted bool                        `json:"entitlementProbeAttempted,omitempty"`
	EntitlementProbeError     string                      `json:"entitlementProbeError,omitempty"`
}

type captureState struct {
	mu         sync.Mutex
	key        []byte
	uses       int
	different  bool
	active     bool
	trampoline []byte
}

// OpenBook loads a disposable Kindle runtime and opens one downloaded bundle.
// All foreign calls originate from Go through a cgo-free dynamic-call bridge.
func OpenBook(runtimePath, bundlePath, preferencesPath, resourcesPath string) (Capture, error) {
	return OpenBookWithAccountSecret(runtimePath, bundlePath, preferencesPath, resourcesPath, "")
}

// OpenBookWithAccountSecret opens a book with an optional caller-supplied raw
// device account secret. The value is used only in this short-lived process.
func OpenBookWithAccountSecret(runtimePath, bundlePath, preferencesPath, resourcesPath, accountSecret string) (Capture, error) {
	fingerprint, profile, knownProfile, err := runtimeProfile(runtimePath)
	if err != nil {
		return Capture{}, err
	}
	if _, err := purego.Dlopen(foundationPath, purego.RTLD_GLOBAL|purego.RTLD_NOW); err != nil {
		return Capture{}, fmt.Errorf("load Foundation: %w", err)
	}
	handle, err := purego.Dlopen(runtimePath, purego.RTLD_LOCAL|purego.RTLD_NOW)
	if err != nil {
		return Capture{}, fmt.Errorf("load disposable Kindle runtime: %w", err)
	}

	poolClass := objc.GetClass("NSAutoreleasePool")
	if poolClass == 0 {
		return Capture{}, errors.New("foundation did not register NSAutoreleasePool")
	}
	pool := objc.ID(poolClass).Send(objc.RegisterName("alloc")).Send(objc.RegisterName("init"))
	// Keep the pool alive until this short-lived bridge process exits. Kindle
	// starts background work while opening a book; draining here can release
	// objects that those threads still reference.
	_ = pool

	credentials, err := readerconfig.LoadCredentials(preferencesPath)
	if err != nil {
		return Capture{}, err
	}
	if accountSecret != "" {
		if len(accountSecret) != 40 {
			return Capture{}, fmt.Errorf("supplied account-secret length is %d; expected 40", len(accountSecret))
		}
		credentials.AccountSecrets = []string{accountSecret}
	}
	requirements, err := readerconfig.InspectVouchers(bundlePath)
	if err != nil {
		return Capture{}, err
	}
	if requirements.AccountSecret && len(credentials.AccountSecrets) == 0 {
		secret, secretErr := runtimeAccountSecret()
		if secretErr == nil {
			credentials.AccountSecrets = []string{secret}
		} else {
			detail := "reader preferences contain no usable raw account secret"
			if credentials.HashedAccountSecret != "" {
				detail += "; the stored hashed account secret is not a valid substitute"
			}
			return Capture{}, fmt.Errorf("book voucher requires ACCOUNT_SECRET: %s; runtime lookup failed: %w",
				detail, secretErr)
		}
	}
	mainPath, vouchers, containers, err := bundleObjects(bundlePath)
	if err != nil {
		return Capture{}, err
	}

	reader, err := resolveReaderInterface(profile.Reader)
	if err != nil {
		return Capture{}, runtimeCompatibilityError(runtimePath, err)
	}
	nsResources := nsString(filepath.Join(resourcesPath, "KRFResources"))
	objc.ID(reader.bookClass).Send(reader.setResourceBundlePath, nsResources)
	objc.ID(reader.bookClass).Send(reader.setICUDataDirectory, nsString(resourcesPath))
	state := &captureState{active: true}
	if err := installKeyCapture(handle, runtimePath, state, profile.Crypto); err != nil {
		return Capture{}, runtimeCompatibilityError(runtimePath, err)
	}
	defer func() {
		state.mu.Lock()
		defer state.mu.Unlock()
		clear(state.key)
		state.active = false
	}()

	provider := objc.ID(reader.providerClass).Send(objc.RegisterName("alloc")).Send(
		reader.providerInitializer,
		nsStringArray(credentials.AccountSecrets), nsString(credentials.DSN), nsURLArray(vouchers))
	if provider == 0 {
		return Capture{}, errors.New("kindle DRM provider initialization failed")
	}
	var nsError objc.ID
	book := objc.ID(reader.bookClass).Send(objc.RegisterName("alloc")).Send(
		reader.bookInitializer,
		nsFileURL(mainPath), provider, nsURLArray(containers), unsafe.Pointer(&nsError))
	if book == 0 {
		code := int64(0)
		domain := ""
		description := ""
		if nsError != 0 {
			code = objc.Send[int64](nsError, objc.RegisterName("code"))
			domain = nsGoString(objc.Send[objc.ID](nsError, objc.RegisterName("domain")))
			description = nsGoString(objc.Send[objc.ID](nsError, objc.RegisterName("localizedDescription")))
		}
		return Capture{}, fmt.Errorf("reader book open failed (domain %q, code %d): %s",
			domain, code, valueOrFallback(description, "no description"))
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.uses == 0 || len(state.key) == 0 {
		return Capture{}, errors.New("book opened but no content key was observed")
	}
	if state.different {
		return Capture{}, errors.New("kindle runtime used more than one distinct content key")
	}
	result := Capture{
		Key: append([]byte(nil), state.key...), Uses: state.uses,
		Profile: profile.Name, KnownProfile: knownProfile, Fingerprint: fingerprint,
	}
	return result, nil
}

// Doctor loads the disposable runtime without opening a book or installing a
// hook. It verifies the build profile, Objective-C interface, dynamic symbol
// ownership, and relocatability of the prospective hook prologue.
func Doctor(runtimePath, preferencesPath string) (DoctorReport, error) {
	fingerprint, profile, knownProfile, err := runtimeProfile(runtimePath)
	if err != nil {
		return DoctorReport{}, err
	}
	if _, err := purego.Dlopen(foundationPath, purego.RTLD_GLOBAL|purego.RTLD_NOW); err != nil {
		return DoctorReport{}, fmt.Errorf("load Foundation: %w", err)
	}
	handle, err := purego.Dlopen(runtimePath, purego.RTLD_LOCAL|purego.RTLD_NOW)
	if err != nil {
		return DoctorReport{}, fmt.Errorf("load disposable reader runtime: %w", err)
	}
	reader, err := resolveReaderInterface(profile.Reader)
	if err != nil {
		return DoctorReport{}, runtimeCompatibilityError(runtimePath, err)
	}
	crypto, err := resolveCryptoSymbols(handle, runtimePath, profile.Crypto)
	if err != nil {
		return DoctorReport{}, runtimeCompatibilityError(runtimePath, err)
	}
	registrationValidated := false
	accountSecretAvailable := false
	hashedAccountSecret := false
	accountSecretProbeError := ""
	if preferencesPath != "" {
		credentials, err := readerconfig.LoadCredentials(preferencesPath)
		if err != nil {
			return DoctorReport{}, fmt.Errorf("validate reader registration: %w", err)
		}
		registrationValidated = true
		accountSecretAvailable = len(credentials.AccountSecrets) != 0
		hashedAccountSecret = credentials.HashedAccountSecret != ""
		if !accountSecretAvailable {
			if _, secretErr := runtimeAccountSecret(); secretErr != nil {
				accountSecretProbeError = secretErr.Error()
			} else {
				accountSecretAvailable = true
			}
		}
	}
	return DoctorReport{
		Fingerprint: fingerprint, Profile: profile.Name, KnownProfile: knownProfile,
		ProviderClass: reader.providerClassName, BookClass: reader.bookClassName,
		ProviderInitializer:       reader.providerInitializerName,
		BookInitializer:           reader.bookInitializerName,
		ResourceBundleSetter:      reader.resourceBundleName,
		ICUDataDirectorySetter:    reader.icuDataDirectoryName,
		DecryptSymbol:             profile.Crypto.DecryptInit,
		CipherKeyLengthSymbol:     profile.Crypto.CipherKeyLength,
		ContextKeyLengthAvailable: crypto.contextKeyLength != 0,
		RegistrationValidated:     registrationValidated,
		AccountSecretAvailable:    accountSecretAvailable,
		HashedAccountSecret:       hashedAccountSecret,
		AccountSecretProbeError:   accountSecretProbeError,
	}, nil
}

// runtimeAccountSecret asks the reader's already-loaded authentication
// manager for its in-memory credential. It never logs or persists the value.
// Class and selector names are preferred anchors only; interface matching is
// used as a fail-closed fallback when a future build renames them.
func runtimeAccountSecret() (string, error) {
	runtime, err := loadObjectiveCRuntime()
	if err != nil {
		return "", err
	}
	getter := objectiveCMethodRequirement{
		role: "account-secret getter", preferredNames: []string{"accountSecret"},
		prefix: "account", argumentCount: 2, semanticTerms: [][]string{{"secret"}},
	}
	shared := objectiveCMethodRequirement{
		role: "authentication-manager singleton", preferredNames: []string{"sharedInstance"},
		prefix: "shared", argumentCount: 2, semanticTerms: [][]string{{"shared"}}, classMethod: true,
	}
	cache := objectiveCMethodRequirement{
		role: "authentication cache loader", preferredNames: []string{"cacheInformation"},
		prefix: "cache", argumentCount: 2, semanticTerms: [][]string{{"information"}},
	}
	class, err := resolveObjectiveCClass(runtime, objectiveCClassRequirement{
		role: "authentication manager", preferredNames: []string{"AuthenticationManager"},
		directInstanceMethod: getter, instanceMethods: []objectiveCMethodRequirement{getter, cache},
		classMethods: []objectiveCMethodRequirement{shared},
	})
	if err != nil {
		return "", err
	}
	sharedSelector, err := resolveObjectiveCMethod(runtime, class, shared)
	if err != nil {
		return "", err
	}
	getterSelector, err := resolveObjectiveCMethod(runtime, class, getter)
	if err != nil {
		return "", err
	}
	cacheSelector, err := resolveObjectiveCMethod(runtime, class, cache)
	if err != nil {
		return "", err
	}
	manager := objc.ID(class).Send(sharedSelector)
	if manager == 0 {
		return "", errors.New("reader authentication manager is unavailable")
	}
	manager.Send(cacheSelector)
	secret := strings.TrimSpace(nsGoString(manager.Send(getterSelector)))
	if len(secret) != 40 {
		return "", fmt.Errorf("reader authentication manager returned account-secret length %d; expected 40", len(secret))
	}
	return secret, nil
}

func runtimeProfile(runtimePath string) (machoutil.BinaryFingerprint, compatibility.Profile, bool, error) {
	data, err := os.ReadFile(runtimePath)
	if err != nil {
		return machoutil.BinaryFingerprint{}, compatibility.Profile{}, false, fmt.Errorf("read reader runtime: %w", err)
	}
	fingerprint, err := machoutil.FingerprintARM64(data)
	if err != nil {
		return machoutil.BinaryFingerprint{}, compatibility.Profile{}, false, err
	}
	profile, known := compatibility.Resolve(fingerprint.UUID, fingerprint.TextSHA256)
	return fingerprint, profile, known, nil
}

func runtimeCompatibilityError(runtimePath string, compatibilityErr error) error {
	data, err := os.ReadFile(runtimePath)
	if err != nil {
		return compatibilityErr
	}
	fingerprint, err := machoutil.FingerprintARM64(data)
	if err != nil {
		return compatibilityErr
	}
	return fmt.Errorf("%w (%s)", compatibilityErr, fingerprint)
}

func bundleObjects(root string) (mainPath string, vouchers, containers []string, err error) {
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(entry.Name())) {
		case ".voucher":
			vouchers = append(vouchers, path)
		case ".azw8":
			if mainPath == "" {
				mainPath = path
			}
		case ".res":
			containers = append(containers, path)
		}
		return nil
	})
	if err != nil {
		return "", nil, nil, fmt.Errorf("scan Kindle bundle: %w", err)
	}
	if mainPath == "" || len(vouchers) == 0 {
		return "", nil, nil, errors.New("selected bundle is missing its AZW8 container or voucher")
	}
	return mainPath, vouchers, containers, nil
}

func nsString(value string) objc.ID {
	return objc.ID(objc.GetClass("NSString")).Send(
		objc.RegisterName("stringWithUTF8String:"), value+"\x00")
}

func nsGoString(value objc.ID) string {
	if value == 0 {
		return ""
	}
	address := objc.Send[uintptr](value, objc.RegisterName("UTF8String"))
	return cStringAt(address, 4096)
}

func valueOrFallback(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func nsFileURL(path string) objc.ID {
	return objc.ID(objc.GetClass("NSURL")).Send(
		objc.RegisterName("fileURLWithPath:"), nsString(path))
}

func nsURLArray(paths []string) objc.ID {
	array := objc.ID(objc.GetClass("NSMutableArray")).Send(objc.RegisterName("array"))
	add := objc.RegisterName("addObject:")
	for _, path := range paths {
		array.Send(add, nsFileURL(path))
	}
	return array
}

func nsStringArray(values []string) objc.ID {
	array := objc.ID(objc.GetClass("NSMutableArray")).Send(objc.RegisterName("array"))
	add := objc.RegisterName("addObject:")
	for _, value := range values {
		array.Send(add, nsString(value))
	}
	return array
}

func installKeyCapture(handle uintptr, runtimePath string, state *captureState, anchors compatibility.CryptoAnchors) error {
	crypto, err := resolveCryptoSymbols(handle, runtimePath, anchors)
	if err != nil {
		return err
	}
	var cipherKeyLength func(unsafe.Pointer) int32
	purego.RegisterFunc(&cipherKeyLength, crypto.cipherKeyLength)
	var contextKeyLength func(unsafe.Pointer) int32
	if crypto.contextKeyLength != 0 {
		purego.RegisterFunc(&contextKeyLength, crypto.contextKeyLength)
	}
	var original func(unsafe.Pointer, unsafe.Pointer, unsafe.Pointer, unsafe.Pointer, unsafe.Pointer) int32
	callback := purego.NewCallback(func(context, cipherType, implementation, key, iv unsafe.Pointer) uintptr {
		keyLength := int32(0)
		if cipherType != nil {
			keyLength = cipherKeyLength(cipherType)
		} else if context != nil && contextKeyLength != nil {
			keyLength = contextKeyLength(context)
		}
		result := original(context, cipherType, implementation, key, iv)
		if result != 1 || key == nil || (keyLength != 16 && keyLength != 24 && keyLength != 32) {
			return uintptr(uint32(result))
		}
		candidate := append([]byte(nil), unsafe.Slice((*byte)(key), int(keyLength))...)
		state.mu.Lock()
		if !state.active {
			clear(candidate)
			state.mu.Unlock()
			return uintptr(uint32(result))
		}
		state.uses++
		if state.key == nil {
			state.key = candidate
		} else {
			if !bytes.Equal(state.key, candidate) {
				state.different = true
			}
			clear(candidate)
		}
		state.mu.Unlock()
		return uintptr(uint32(result))
	})
	trampoline, mapped, err := installInlineHook(crypto.decryptInit, callback)
	if err != nil {
		return err
	}
	state.trampoline = mapped
	purego.RegisterFunc(&original, trampoline)
	return nil
}

type cryptoSymbols struct {
	decryptInit      uintptr
	cipherKeyLength  uintptr
	contextKeyLength uintptr
}

type dynamicSymbolInfo struct {
	imagePath     uintptr
	imageBase     uintptr
	symbolName    uintptr
	symbolAddress uintptr
}

func resolveCryptoSymbols(handle uintptr, runtimePath string, anchors compatibility.CryptoAnchors) (cryptoSymbols, error) {
	decryptInit, err := requiredDynamicSymbol(handle, runtimePath, anchors.DecryptInit)
	if err != nil {
		return cryptoSymbols{}, fmt.Errorf("find reader decrypt entry point: %w", err)
	}
	code := make([]byte, inlineHookSize)
	if err := copyFromAddress(code, decryptInit); err != nil {
		return cryptoSymbols{}, err
	}
	if err := validateRelocatablePrologue(code); err != nil {
		return cryptoSymbols{}, fmt.Errorf("unsupported crypto entry point: %w", err)
	}
	cipherKeyLength, err := requiredDynamicSymbol(handle, runtimePath, anchors.CipherKeyLength)
	if err != nil {
		return cryptoSymbols{}, fmt.Errorf("find reader cipher key-length function: %w", err)
	}
	contextKeyLength := uintptr(0)
	if anchors.ContextKeyLength != "" {
		if address, lookupErr := purego.Dlsym(handle, anchors.ContextKeyLength); lookupErr == nil && address != 0 {
			if err := validateDynamicSymbol(address, runtimePath, anchors.ContextKeyLength); err != nil {
				return cryptoSymbols{}, err
			}
			contextKeyLength = address
		}
	}
	return cryptoSymbols{
		decryptInit: decryptInit, cipherKeyLength: cipherKeyLength,
		contextKeyLength: contextKeyLength,
	}, nil
}

func requiredDynamicSymbol(handle uintptr, runtimePath, name string) (uintptr, error) {
	if name == "" {
		return 0, errors.New("compatibility profile contains an empty symbol name")
	}
	address, err := purego.Dlsym(handle, name)
	if err != nil {
		return 0, err
	}
	if err := validateDynamicSymbol(address, runtimePath, name); err != nil {
		return 0, err
	}
	return address, nil
}

func validateDynamicSymbol(address uintptr, runtimePath, expectedName string) error {
	if address == 0 || address%4 != 0 {
		return fmt.Errorf("symbol %s has invalid arm64 address %#x", expectedName, address)
	}
	var dladdr func(uintptr, *dynamicSymbolInfo) int32
	purego.RegisterLibFunc(&dladdr, purego.RTLD_DEFAULT, "dladdr")
	var info dynamicSymbolInfo
	if result := dladdr(address, &info); result == 0 {
		return fmt.Errorf("dladdr could not resolve %s", expectedName)
	}
	if info.imageBase == 0 || info.symbolAddress != address || address < info.imageBase {
		return fmt.Errorf("symbol %s does not resolve to its own loaded-image entry point", expectedName)
	}
	actualName := strings.TrimPrefix(cStringAt(info.symbolName, 512), "_")
	if actualName != expectedName {
		return fmt.Errorf("symbol identity mismatch: requested %s, resolved %q", expectedName, actualName)
	}
	loadedPath := cStringAt(info.imagePath, 4096)
	loadedInfo, loadedErr := os.Stat(loadedPath)
	runtimeInfo, runtimeErr := os.Stat(runtimePath)
	if loadedErr != nil || runtimeErr != nil || !os.SameFile(loadedInfo, runtimeInfo) {
		return fmt.Errorf("symbol %s belongs to unexpected image %q", expectedName, loadedPath)
	}
	return nil
}

func cStringAt(address uintptr, limit int) string {
	if address == 0 || limit <= 0 {
		return ""
	}
	var stringLength func(uintptr, uintptr) uintptr
	purego.RegisterLibFunc(&stringLength, purego.RTLD_DEFAULT, "strnlen")
	length := stringLength(address, uintptr(limit))
	if length == 0 || length >= uintptr(limit) {
		return ""
	}
	data := make([]byte, length)
	if err := copyFromAddress(data, address); err != nil {
		return ""
	}
	return string(data)
}

func installInlineHook(target, replacement uintptr) (uintptr, []byte, error) {
	if target == 0 || replacement == 0 {
		return 0, nil, errors.New("crypto hook address is nil")
	}
	originalCode := make([]byte, inlineHookSize)
	if err := copyFromAddress(originalCode, target); err != nil {
		return 0, nil, err
	}
	if err := validateRelocatablePrologue(originalCode); err != nil {
		return 0, nil, fmt.Errorf("unsupported crypto entry point: %w", err)
	}
	pageSize := syscall.Getpagesize()
	trampoline, err := syscall.Mmap(-1, 0, pageSize,
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		return 0, nil, fmt.Errorf("allocate hook trampoline: %w", err)
	}
	trampolineAddress := uintptr(unsafe.Pointer(&trampoline[0]))
	copy(trampoline[:inlineHookSize], originalCode)
	writeAbsoluteJump(trampoline[inlineHookSize:inlineHookSize*2], target+inlineHookSize)
	if err := syscall.Mprotect(trampoline, syscall.PROT_READ|syscall.PROT_EXEC); err != nil {
		_ = syscall.Munmap(trampoline)
		return 0, nil, fmt.Errorf("make hook trampoline executable: %w", err)
	}
	invalidateInstructionCache(trampolineAddress, inlineHookSize*2)

	page := target & ^(uintptr(pageSize) - 1)
	protectionSize := uintptr(pageSize)
	if target+inlineHookSize > page+uintptr(pageSize) {
		protectionSize += uintptr(pageSize)
	}
	if err := protectMachPage(page, protectionSize, 1|2|0x10); err != nil {
		_ = syscall.Munmap(trampoline)
		return 0, nil, fmt.Errorf("make Kindle decrypt entry point writable: %w", err)
	}
	jump := make([]byte, inlineHookSize)
	writeAbsoluteJump(jump, replacement)
	if err := copyToAddress(target, jump); err != nil {
		restoreErr := protectMachPage(page, protectionSize, 1|4)
		_ = syscall.Munmap(trampoline)
		return 0, nil, errors.Join(err, restoreErr)
	}
	invalidateInstructionCache(target, inlineHookSize)
	if err := protectMachPage(page, protectionSize, 1|4); err != nil {
		_ = syscall.Munmap(trampoline)
		return 0, nil, fmt.Errorf("restore Kindle code protection: %w", err)
	}
	return trampolineAddress, trampoline, nil
}

func validateRelocatablePrologue(code []byte) error {
	if len(code) != inlineHookSize {
		return fmt.Errorf("prologue is %d bytes; need %d", len(code), inlineHookSize)
	}
	for offset := 0; offset < len(code); offset += 4 {
		instruction, err := arm64asm.Decode(code[offset : offset+4])
		if err != nil {
			return fmt.Errorf("decode instruction at +%#x: %w", offset, err)
		}
		for _, argument := range instruction.Args {
			if _, ok := argument.(arm64asm.PCRel); ok {
				return fmt.Errorf("PC-relative instruction at +%#x: %s", offset, instruction)
			}
		}
		if changesControlFlow(instruction.Op.String()) {
			return fmt.Errorf("control-flow instruction at +%#x: %s", offset, instruction)
		}
	}
	return nil
}

func changesControlFlow(operation string) bool {
	switch operation {
	case "BR", "BRAA", "BRAAZ", "BRAB", "BRABZ",
		"BLR", "BLRAA", "BLRAAZ", "BLRAB", "BLRABZ",
		"RET", "RETAA", "RETAB", "ERET", "ERETAA", "ERETAB", "DRPS",
		"SVC", "HVC", "SMC", "BRK", "HLT", "DCPS1", "DCPS2", "DCPS3":
		return true
	default:
		return false
	}
}

func writeAbsoluteJump(destination []byte, address uintptr) {
	binary.LittleEndian.PutUint32(destination[0:4], 0x58000051) // ldr x17, #8
	binary.LittleEndian.PutUint32(destination[4:8], 0xd61f0220) // br x17
	binary.LittleEndian.PutUint64(destination[8:16], uint64(address))
}

func protectMachPage(address, size uintptr, protection int32) error {
	variable, err := purego.Dlsym(purego.RTLD_DEFAULT, "mach_task_self_")
	if err != nil {
		return err
	}
	var task uint32
	var copyFrom func(unsafe.Pointer, uintptr, uintptr) unsafe.Pointer
	purego.RegisterLibFunc(&copyFrom, purego.RTLD_DEFAULT, "memcpy")
	copyFrom(unsafe.Pointer(&task), variable, unsafe.Sizeof(task))
	var machVMProtect func(uint32, uint64, uint64, int32, int32) int32
	purego.RegisterLibFunc(&machVMProtect, purego.RTLD_DEFAULT, "mach_vm_protect")
	if result := machVMProtect(task, uint64(address), uint64(size), 0, protection); result != 0 {
		return fmt.Errorf("mach_vm_protect returned %d", result)
	}
	return nil
}

func copyFromAddress(destination []byte, source uintptr) error {
	if len(destination) == 0 {
		return nil
	}
	if source == 0 {
		return errors.New("memory source address is nil")
	}
	var memoryCopy func(unsafe.Pointer, uintptr, uintptr) unsafe.Pointer
	purego.RegisterLibFunc(&memoryCopy, purego.RTLD_DEFAULT, "memcpy")
	memoryCopy(unsafe.Pointer(&destination[0]), source, uintptr(len(destination)))
	return nil
}

func copyToAddress(destination uintptr, source []byte) error {
	if len(source) == 0 {
		return nil
	}
	if destination == 0 {
		return errors.New("memory destination address is nil")
	}
	var memoryCopy func(uintptr, unsafe.Pointer, uintptr) uintptr
	purego.RegisterLibFunc(&memoryCopy, purego.RTLD_DEFAULT, "memcpy")
	memoryCopy(destination, unsafe.Pointer(&source[0]), uintptr(len(source)))
	return nil
}

func invalidateInstructionCache(address, size uintptr) {
	var invalidate func(uintptr, uintptr)
	purego.RegisterLibFunc(&invalidate, purego.RTLD_DEFAULT, "sys_icache_invalidate")
	invalidate(address, size)
}
