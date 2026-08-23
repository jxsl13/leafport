//go:build darwin

package runtimebridge

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"

	"leafport/internal/compatibility"
)

type objectiveCClassRequirement struct {
	role                 string
	preferredNames       []string
	directInstanceMethod objectiveCMethodRequirement
	instanceMethods      []objectiveCMethodRequirement
	classMethods         []objectiveCMethodRequirement
}

type objectiveCMethodRequirement struct {
	role           string
	preferredNames []string
	prefix         string
	argumentCount  uint32
	semanticTerms  [][]string
	classMethod    bool
}

type objectiveCMethod struct {
	selector      objc.SEL
	name          string
	argumentCount uint32
}

type readerInterface struct {
	providerClass           objc.Class
	bookClass               objc.Class
	providerInitializer     objc.SEL
	bookInitializer         objc.SEL
	setResourceBundlePath   objc.SEL
	setICUDataDirectory     objc.SEL
	providerClassName       string
	bookClassName           string
	providerInitializerName string
	bookInitializerName     string
	resourceBundleName      string
	icuDataDirectoryName    string
}

type objectiveCRuntime struct {
	getNamedClass          func(string) objc.Class
	getClassList           func(unsafe.Pointer, int32) int32
	classGetName           func(objc.Class) string
	classGetSuperclass     func(objc.Class) objc.Class
	classGetInstanceMethod func(objc.Class, objc.SEL) uintptr
	classGetClassMethod    func(objc.Class, objc.SEL) uintptr
	objectGetClass         func(objc.Class) objc.Class
	classCopyMethodList    func(objc.Class, *uint32) unsafe.Pointer
	methodGetName          func(uintptr) objc.SEL
	methodGetArgumentCount func(uintptr) uint32
	selectorGetName        func(objc.SEL) string
	free                   func(unsafe.Pointer)
	listMethods            func(objc.Class, bool) ([]objectiveCMethod, error)
}

func resolveReaderInterface(anchors compatibility.ReaderAnchors) (readerInterface, error) {
	runtime, err := loadObjectiveCRuntime()
	if err != nil {
		return readerInterface{}, err
	}
	providerRequirement, bookRequirement := classRequirements(anchors)
	providerInitializerRequirement := methodRequirement(anchors.ProviderInitializer, false)
	bookInitializerRequirement := methodRequirement(anchors.BookInitializer, false)
	resourceBundleRequirement := methodRequirement(anchors.ResourceBundle, true)
	icuDirectoryRequirement := methodRequirement(anchors.ICUDataDirectory, true)
	provider, err := resolveObjectiveCClass(runtime, providerRequirement)
	if err != nil {
		return readerInterface{}, err
	}
	book, err := resolveObjectiveCClass(runtime, bookRequirement)
	if err != nil {
		return readerInterface{}, err
	}
	providerInitializer, err := resolveObjectiveCMethod(runtime, provider, providerInitializerRequirement)
	if err != nil {
		return readerInterface{}, err
	}
	bookInitializer, err := resolveObjectiveCMethod(runtime, book, bookInitializerRequirement)
	if err != nil {
		return readerInterface{}, err
	}
	resourceBundle, err := resolveObjectiveCMethod(runtime, book, resourceBundleRequirement)
	if err != nil {
		return readerInterface{}, err
	}
	icuDirectory, err := resolveObjectiveCMethod(runtime, book, icuDirectoryRequirement)
	if err != nil {
		return readerInterface{}, err
	}
	return readerInterface{
		providerClass: provider, bookClass: book,
		providerInitializer: providerInitializer, bookInitializer: bookInitializer,
		setResourceBundlePath: resourceBundle, setICUDataDirectory: icuDirectory,
		providerClassName: runtime.classGetName(provider), bookClassName: runtime.classGetName(book),
		providerInitializerName: runtime.selectorGetName(providerInitializer),
		bookInitializerName:     runtime.selectorGetName(bookInitializer),
		resourceBundleName:      runtime.selectorGetName(resourceBundle),
		icuDataDirectoryName:    runtime.selectorGetName(icuDirectory),
	}, nil
}

func classRequirements(anchors compatibility.ReaderAnchors) (objectiveCClassRequirement, objectiveCClassRequirement) {
	providerInitializer := methodRequirement(anchors.ProviderInitializer, false)
	bookInitializer := methodRequirement(anchors.BookInitializer, false)
	resourceBundle := methodRequirement(anchors.ResourceBundle, true)
	icuDirectory := methodRequirement(anchors.ICUDataDirectory, true)
	return objectiveCClassRequirement{
		role: "DRM data provider", preferredNames: append([]string(nil), anchors.ProviderClasses...),
		directInstanceMethod: providerInitializer,
		instanceMethods:      []objectiveCMethodRequirement{providerInitializer},
	}, objectiveCClassRequirement{
		role: "book reader", preferredNames: append([]string(nil), anchors.BookClasses...),
		directInstanceMethod: bookInitializer,
		instanceMethods:      []objectiveCMethodRequirement{bookInitializer},
		classMethods:         []objectiveCMethodRequirement{resourceBundle, icuDirectory},
	}
}

func methodRequirement(anchor compatibility.MethodAnchor, classMethod bool) objectiveCMethodRequirement {
	return objectiveCMethodRequirement{
		role: anchor.Role, preferredNames: append([]string(nil), anchor.Names...),
		prefix: anchor.Prefix, argumentCount: anchor.ArgumentCount,
		semanticTerms: anchor.SemanticTerms, classMethod: classMethod,
	}
}

func loadObjectiveCRuntime() (objectiveCRuntime, error) {
	runtime := objectiveCRuntime{getNamedClass: objc.GetClass}
	registrations := []struct {
		name        string
		destination any
	}{
		{"objc_getClassList", &runtime.getClassList},
		{"class_getName", &runtime.classGetName},
		{"class_getSuperclass", &runtime.classGetSuperclass},
		{"class_getInstanceMethod", &runtime.classGetInstanceMethod},
		{"class_getClassMethod", &runtime.classGetClassMethod},
		{"object_getClass", &runtime.objectGetClass},
		{"class_copyMethodList", &runtime.classCopyMethodList},
		{"method_getName", &runtime.methodGetName},
		{"method_getNumberOfArguments", &runtime.methodGetArgumentCount},
		{"sel_getName", &runtime.selectorGetName},
		{"free", &runtime.free},
	}
	for _, registration := range registrations {
		address, err := purego.Dlsym(purego.RTLD_DEFAULT, registration.name)
		if err != nil {
			return objectiveCRuntime{}, fmt.Errorf("load Objective-C runtime function %s: %w", registration.name, err)
		}
		purego.RegisterFunc(registration.destination, address)
	}
	runtime.listMethods = runtime.copyMethods
	return runtime, nil
}

func resolveObjectiveCClass(runtime objectiveCRuntime, requirement objectiveCClassRequirement) (objc.Class, error) {
	if err := validateObjectiveCRuntime(runtime); err != nil {
		return 0, err
	}
	for _, name := range requirement.preferredNames {
		if preferred := runtime.getNamedClass(name); preferred != 0 && classMatches(runtime, preferred, requirement) {
			return preferred, nil
		}
	}

	classes, err := runtime.classes()
	if err != nil {
		return 0, err
	}
	var matches []objc.Class
	for _, class := range classes {
		if classMatches(runtime, class, requirement) {
			matches = append(matches, class)
		}
	}
	switch len(matches) {
	case 0:
		return 0, fmt.Errorf("no Objective-C class satisfies the %s interface", requirement.role)
	case 1:
		return matches[0], nil
	default:
		names := make([]string, 0, len(matches))
		for _, class := range matches {
			name := runtime.classGetName(class)
			if name == "" {
				name = fmt.Sprintf("%#x", uintptr(class))
			}
			names = append(names, name)
		}
		sort.Strings(names)
		return 0, fmt.Errorf("multiple Objective-C classes satisfy the %s interface: %s",
			requirement.role, strings.Join(names, ", "))
	}
}

func resolveObjectiveCMethod(runtime objectiveCRuntime, class objc.Class, requirement objectiveCMethodRequirement) (objc.SEL, error) {
	for _, name := range requirement.preferredNames {
		preferred := objc.RegisterName(name)
		var method uintptr
		if requirement.classMethod {
			method = runtime.classGetClassMethod(class, preferred)
		} else {
			method = runtime.classGetInstanceMethod(class, preferred)
		}
		if method != 0 && runtime.methodGetArgumentCount(method) == requirement.argumentCount {
			return preferred, nil
		}
	}
	if runtime.listMethods == nil {
		return 0, errors.New("Objective-C method enumeration is unavailable")
	}
	methods, err := runtime.listMethods(class, requirement.classMethod)
	if err != nil {
		return 0, err
	}
	var matches []objectiveCMethod
	for _, candidate := range methods {
		if candidate.argumentCount == requirement.argumentCount &&
			strings.HasPrefix(strings.ToLower(candidate.name), strings.ToLower(requirement.prefix)) &&
			matchesSemanticTerms(candidate.name, requirement.semanticTerms) {
			matches = append(matches, candidate)
		}
	}
	className := runtime.classGetName(class)
	switch len(matches) {
	case 0:
		return 0, fmt.Errorf("Objective-C class %s has no compatible %s", className, requirement.role)
	case 1:
		return matches[0].selector, nil
	default:
		names := make([]string, 0, len(matches))
		for _, match := range matches {
			names = append(names, match.name)
		}
		sort.Strings(names)
		return 0, fmt.Errorf("Objective-C class %s has multiple compatible %s methods: %s",
			className, requirement.role, strings.Join(names, ", "))
	}
}

func matchesSemanticTerms(name string, groups [][]string) bool {
	lower := strings.ToLower(name)
	for _, alternatives := range groups {
		matched := false
		for _, alternative := range alternatives {
			if strings.Contains(lower, strings.ToLower(alternative)) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func validateObjectiveCRuntime(runtime objectiveCRuntime) error {
	if runtime.getNamedClass == nil || runtime.getClassList == nil || runtime.classGetName == nil ||
		runtime.classGetSuperclass == nil || runtime.classGetInstanceMethod == nil ||
		runtime.classGetClassMethod == nil {
		return errors.New("Objective-C runtime resolver is incomplete")
	}
	return nil
}

func (runtime objectiveCRuntime) classes() ([]objc.Class, error) {
	for range 3 {
		count := runtime.getClassList(nil, 0)
		if count <= 0 {
			return nil, errors.New("Objective-C runtime contains no classes")
		}
		classes := make([]objc.Class, count)
		actual := runtime.getClassList(unsafe.Pointer(&classes[0]), count)
		if actual < 0 {
			return nil, errors.New("Objective-C class enumeration failed")
		}
		if actual <= count {
			return classes[:actual], nil
		}
	}
	return nil, errors.New("Objective-C class list changed repeatedly during enumeration")
}

func (runtime objectiveCRuntime) copyMethods(class objc.Class, classMethod bool) ([]objectiveCMethod, error) {
	target := class
	if classMethod {
		target = runtime.objectGetClass(class)
		if target == 0 {
			return nil, fmt.Errorf("Objective-C class %s has no metaclass", runtime.classGetName(class))
		}
	}
	var count uint32
	pointer := runtime.classCopyMethodList(target, &count)
	if pointer == nil {
		if count == 0 {
			return nil, nil
		}
		return nil, errors.New("Objective-C method enumeration failed")
	}
	defer runtime.free(pointer)
	raw := unsafe.Slice((*uintptr)(pointer), int(count))
	methods := make([]objectiveCMethod, 0, len(raw))
	for _, method := range raw {
		selector := runtime.methodGetName(method)
		if selector == 0 {
			continue
		}
		methods = append(methods, objectiveCMethod{
			selector: selector, name: runtime.selectorGetName(selector),
			argumentCount: runtime.methodGetArgumentCount(method),
		})
	}
	return methods, nil
}

func classMatches(runtime objectiveCRuntime, class objc.Class, requirement objectiveCClassRequirement) bool {
	if class == 0 {
		return false
	}
	for _, method := range requirement.instanceMethods {
		method.classMethod = false
		if _, err := resolveObjectiveCMethod(runtime, class, method); err != nil {
			return false
		}
	}
	for _, method := range requirement.classMethods {
		method.classMethod = true
		if _, err := resolveObjectiveCMethod(runtime, class, method); err != nil {
			return false
		}
	}
	direct := requirement.directInstanceMethod
	direct.classMethod = false
	selector, err := resolveObjectiveCMethod(runtime, class, direct)
	if err != nil {
		return false
	}
	method := runtime.classGetInstanceMethod(class, selector)
	if method == 0 {
		return false
	}
	if superclass := runtime.classGetSuperclass(class); superclass != 0 &&
		runtime.classGetInstanceMethod(superclass, selector) == method {
		return false
	}
	return true
}
