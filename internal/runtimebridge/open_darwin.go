//go:build darwin

package runtimebridge

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
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
	"howett.net/plist"
)

const foundationPath = "/System/Library/Frameworks/Foundation.framework/Foundation"

type Capture struct {
	Key  []byte
	Uses int
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

	dsn, err := deviceSerial(preferencesPath)
	if err != nil {
		return Capture{}, err
	}
	mainPath, vouchers, containers, err := bundleObjects(bundlePath)
	if err != nil {
		return Capture{}, err
	}

	providerClass := objc.GetClass("KRFDRMDataProvider")
	bookClass := objc.GetClass("KRFBook")
	if providerClass == 0 || bookClass == 0 {
		return Capture{}, errors.New("kindle reading classes are unavailable")
	}
	nsResources := nsString(filepath.Join(resourcesPath, "KRFResources"))
	objc.ID(bookClass).Send(objc.RegisterName("setResourceBundlePath:"), nsResources)
	objc.ID(bookClass).Send(objc.RegisterName("setICUDataDirectory:"), nsString(resourcesPath))
	state := &captureState{active: true}
	if err := installKeyCapture(handle, state); err != nil {
		return Capture{}, err
	}
	defer func() {
		state.mu.Lock()
		defer state.mu.Unlock()
		clear(state.key)
		state.active = false
	}()

	emptyArray := objc.ID(objc.GetClass("NSArray")).Send(objc.RegisterName("array"))
	provider := objc.ID(providerClass).Send(objc.RegisterName("alloc")).Send(
		objc.RegisterName("initWithAccountSecrets:kindleSerialNumber:voucherList:"),
		emptyArray, nsString(dsn), nsURLArray(vouchers))
	if provider == 0 {
		return Capture{}, errors.New("kindle DRM provider initialization failed")
	}
	var nsError objc.ID
	book := objc.ID(bookClass).Send(objc.RegisterName("alloc")).Send(
		objc.RegisterName("initWithURL:DRMDataProvider:containers:error:"),
		nsFileURL(mainPath), provider, nsURLArray(containers), unsafe.Pointer(&nsError))
	if book == 0 {
		code := int64(0)
		if nsError != 0 {
			code = objc.Send[int64](nsError, objc.RegisterName("code"))
		}
		return Capture{}, fmt.Errorf("kindle book open failed (code %d)", code)
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.uses == 0 || len(state.key) == 0 {
		return Capture{}, errors.New("book opened but no content key was observed")
	}
	if state.different {
		return Capture{}, errors.New("kindle runtime used more than one distinct content key")
	}
	result := Capture{Key: append([]byte(nil), state.key...), Uses: state.uses}
	return result, nil
}

func deviceSerial(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read Kindle preferences: %w", err)
	}
	var preferences map[string]any
	if _, err := plist.Unmarshal(data, &preferences); err != nil {
		return "", fmt.Errorf("decode Kindle preferences: %w", err)
	}
	raw, ok := preferences["kindle_notifications_persist_NotificationsCustomData"].(string)
	if !ok || raw == "" {
		return "", errors.New("kindle device registration is unavailable")
	}
	var payload struct {
		DSN string `json:"dsn"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return "", fmt.Errorf("decode Kindle device registration: %w", err)
	}
	if payload.DSN == "" {
		return "", errors.New("kindle device serial is empty")
	}
	return payload.DSN, nil
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

func installKeyCapture(handle uintptr, state *captureState) error {
	initAddress, err := purego.Dlsym(handle, "EVP_DecryptInit_ex")
	if err != nil {
		return fmt.Errorf("find Kindle decrypt entry point: %w", err)
	}
	keyLengthAddress, err := purego.Dlsym(handle, "EVP_CIPHER_key_length")
	if err != nil {
		return fmt.Errorf("find Kindle cipher key-length function: %w", err)
	}
	contextLengthAddress, _ := purego.Dlsym(handle, "EVP_CIPHER_CTX_key_length")
	var cipherKeyLength func(unsafe.Pointer) int32
	purego.RegisterFunc(&cipherKeyLength, keyLengthAddress)
	var contextKeyLength func(unsafe.Pointer) int32
	if contextLengthAddress != 0 {
		purego.RegisterFunc(&contextKeyLength, contextLengthAddress)
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
	trampoline, mapped, err := installInlineHook(initAddress, callback)
	if err != nil {
		return err
	}
	state.trampoline = mapped
	purego.RegisterFunc(&original, trampoline)
	return nil
}

func installInlineHook(target, replacement uintptr) (uintptr, []byte, error) {
	expected := []byte{0xff, 0x03, 0x01, 0xd1, 0xfd, 0x7b, 0x03, 0xa9, 0xfd, 0xc3, 0x00, 0x91}
	actual := make([]byte, len(expected))
	if target == 0 || replacement == 0 || copyFromAddress(actual, target) != nil ||
		!bytes.Equal(actual, expected) {
		return 0, nil, errors.New("this Kindle build has an unsupported crypto entry point")
	}
	pageSize := syscall.Getpagesize()
	trampoline, err := syscall.Mmap(-1, 0, pageSize,
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		return 0, nil, fmt.Errorf("allocate hook trampoline: %w", err)
	}
	trampolineAddress := uintptr(unsafe.Pointer(&trampoline[0]))
	if err := copyFromAddress(trampoline[:16], target); err != nil {
		_ = syscall.Munmap(trampoline)
		return 0, nil, err
	}
	writeAbsoluteJump(trampoline[16:32], target+16)
	if err := syscall.Mprotect(trampoline, syscall.PROT_READ|syscall.PROT_EXEC); err != nil {
		_ = syscall.Munmap(trampoline)
		return 0, nil, fmt.Errorf("make hook trampoline executable: %w", err)
	}
	invalidateInstructionCache(trampolineAddress, 32)

	page := target & ^(uintptr(pageSize) - 1)
	if err := protectMachPage(page, uintptr(pageSize), 1|2|0x10); err != nil {
		_ = syscall.Munmap(trampoline)
		return 0, nil, fmt.Errorf("make Kindle decrypt entry point writable: %w", err)
	}
	jump := make([]byte, 16)
	writeAbsoluteJump(jump, replacement)
	if err := copyToAddress(target, jump); err != nil {
		return 0, nil, err
	}
	invalidateInstructionCache(target, 16)
	if err := protectMachPage(page, uintptr(pageSize), 1|4); err != nil {
		return 0, nil, fmt.Errorf("restore Kindle code protection: %w", err)
	}
	return trampolineAddress, trampoline, nil
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
