//go:build darwin

package runtimebridge

import (
	"strings"
	"testing"
	"unsafe"

	"github.com/ebitengine/purego/objc"

	"github.com/jxsl13/leafport/internal/compatibility"
)

func TestResolveObjectiveCClassPrefersKnownName(t *testing.T) {
	runtime := fakeObjectiveCRuntime([]objc.Class{1, 2}, map[objc.Class]bool{1: true, 2: true})
	runtime.getNamedClass = func(name string) objc.Class {
		if name == "Known" {
			return 2
		}
		return 0
	}
	class, err := resolveObjectiveCClass(runtime, objectiveCClassRequirement{
		role: "test", preferredNames: []string{"Known"},
		directInstanceMethod: testMethodRequirement("initializer", "initTest:", false),
		instanceMethods:      []objectiveCMethodRequirement{testMethodRequirement("initializer", "initTest:", false)},
		classMethods:         []objectiveCMethodRequirement{testMethodRequirement("configuration", "configure:", true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if class != 2 {
		t.Fatalf("class = %d, want 2", class)
	}
}

func TestResolveObjectiveCClassFindsRenamedClass(t *testing.T) {
	runtime := fakeObjectiveCRuntime([]objc.Class{1, 2}, map[objc.Class]bool{2: true})
	class, err := resolveObjectiveCClass(runtime, objectiveCClassRequirement{
		role: "test", preferredNames: []string{"OldName"},
		directInstanceMethod: testMethodRequirement("initializer", "initTest:", false),
		instanceMethods:      []objectiveCMethodRequirement{testMethodRequirement("initializer", "initTest:", false)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if class != 2 {
		t.Fatalf("class = %d, want renamed class 2", class)
	}
}

func TestResolveObjectiveCClassRejectsAmbiguity(t *testing.T) {
	runtime := fakeObjectiveCRuntime([]objc.Class{1, 2}, map[objc.Class]bool{1: true, 2: true})
	_, err := resolveObjectiveCClass(runtime, objectiveCClassRequirement{
		role: "test", preferredNames: []string{"Missing"},
		directInstanceMethod: testMethodRequirement("initializer", "initTest:", false),
		instanceMethods:      []objectiveCMethodRequirement{testMethodRequirement("initializer", "initTest:", false)},
	})
	if err == nil || !strings.Contains(err.Error(), "multiple Objective-C classes") {
		t.Fatalf("error = %v, want ambiguity", err)
	}
}

func TestResolveObjectiveCClassAndMethodRenameTogether(t *testing.T) {
	runtime := fakeObjectiveCRuntime([]objc.Class{1, 2}, nil)
	runtime.classGetInstanceMethod = func(class objc.Class, selector objc.SEL) uintptr {
		if class == 2 && selector == 42 {
			return 42
		}
		return 0
	}
	runtime.listMethods = func(class objc.Class, classMethod bool) ([]objectiveCMethod, error) {
		if class == 2 && !classMethod {
			return []objectiveCMethod{{
				selector: 42, name: "initWithAccountData:deviceSerial:vouchers:", argumentCount: 5,
			}}, nil
		}
		return nil, nil
	}
	providerInitializer := defaultProviderInitializerRequirement()
	requirement := objectiveCClassRequirement{
		role: "test", preferredNames: []string{"OldProvider"},
		directInstanceMethod: providerInitializer,
		instanceMethods:      []objectiveCMethodRequirement{providerInitializer},
	}
	class, err := resolveObjectiveCClass(runtime, requirement)
	if err != nil {
		t.Fatal(err)
	}
	if class != 2 {
		t.Fatalf("class = %d, want renamed class 2", class)
	}
}

func TestResolveObjectiveCMethodFindsSemanticRename(t *testing.T) {
	runtime := fakeObjectiveCRuntime([]objc.Class{1}, map[objc.Class]bool{1: true})
	runtime.classGetInstanceMethod = func(objc.Class, objc.SEL) uintptr { return 0 }
	runtime.methodGetArgumentCount = func(method uintptr) uint32 { return uint32(method) }
	runtime.listMethods = func(class objc.Class, classMethod bool) ([]objectiveCMethod, error) {
		return []objectiveCMethod{{
			selector: 42, name: "initWithAccountData:deviceSerial:vouchers:", argumentCount: 5,
		}}, nil
	}
	selector, err := resolveObjectiveCMethod(runtime, 1, defaultProviderInitializerRequirement())
	if err != nil {
		t.Fatal(err)
	}
	if selector != 42 {
		t.Fatalf("selector = %d, want renamed selector 42", selector)
	}
}

func TestResolveObjectiveCMethodRejectsSemanticAmbiguity(t *testing.T) {
	runtime := fakeObjectiveCRuntime([]objc.Class{1}, map[objc.Class]bool{1: true})
	runtime.classGetInstanceMethod = func(objc.Class, objc.SEL) uintptr { return 0 }
	runtime.methodGetArgumentCount = func(method uintptr) uint32 { return uint32(method) }
	runtime.listMethods = func(class objc.Class, classMethod bool) ([]objectiveCMethod, error) {
		return []objectiveCMethod{
			{selector: 42, name: "initWithAccountData:deviceSerial:vouchers:", argumentCount: 5},
			{selector: 43, name: "initAccount:serial:voucher:", argumentCount: 5},
		}, nil
	}
	_, err := resolveObjectiveCMethod(runtime, 1, defaultProviderInitializerRequirement())
	if err == nil || !strings.Contains(err.Error(), "multiple compatible") {
		t.Fatalf("error = %v, want semantic ambiguity", err)
	}
}

func fakeObjectiveCRuntime(classes []objc.Class, matching map[objc.Class]bool) objectiveCRuntime {
	return objectiveCRuntime{
		getNamedClass: func(string) objc.Class { return 0 },
		getClassList: func(buffer unsafe.Pointer, count int32) int32 {
			if buffer != nil {
				copy(unsafe.Slice((*objc.Class)(buffer), int(count)), classes)
			}
			return int32(len(classes))
		},
		classGetName:       func(class objc.Class) string { return string(rune('A' + class - 1)) },
		classGetSuperclass: func(objc.Class) objc.Class { return 0 },
		classGetInstanceMethod: func(class objc.Class, selector objc.SEL) uintptr {
			if matching[class] {
				return uintptr(selector)
			}
			return 0
		},
		classGetClassMethod: func(class objc.Class, selector objc.SEL) uintptr {
			if matching[class] {
				return uintptr(selector)
			}
			return 0
		},
		methodGetArgumentCount: func(uintptr) uint32 { return 3 },
		listMethods:            func(objc.Class, bool) ([]objectiveCMethod, error) { return nil, nil },
	}
}

func testMethodRequirement(role, name string, classMethod bool) objectiveCMethodRequirement {
	return objectiveCMethodRequirement{
		role: role, preferredNames: []string{name}, prefix: strings.TrimSuffix(name, ":"),
		argumentCount: 3, semanticTerms: [][]string{{strings.TrimSuffix(name, ":")}},
		classMethod: classMethod,
	}
}

func defaultProviderInitializerRequirement() objectiveCMethodRequirement {
	profile, _ := compatibility.Resolve("", "")
	return methodRequirement(profile.Reader.ProviderInitializer, false)
}
