package compatibility

import "testing"

func TestResolveKnownProfileRequiresBothIdentifiers(t *testing.T) {
	known := KnownProfiles()[0]
	profile, exact := Resolve(known.UUID, known.TextSHA256)
	if !exact || profile.Name != known.Name {
		t.Fatalf("Resolve() = (%q, %t), want (%q, true)", profile.Name, exact, known.Name)
	}
	if _, exact := Resolve(known.UUID, "different"); exact {
		t.Fatal("UUID-only match was accepted")
	}
	if _, exact := Resolve("different", known.TextSHA256); exact {
		t.Fatal("text-hash-only match was accepted")
	}
}

func TestProfilesAreDefensiveCopies(t *testing.T) {
	first := KnownProfiles()
	first[0].Reader.ProviderClasses[0] = "modified"
	second := KnownProfiles()
	if second[0].Reader.ProviderClasses[0] == "modified" {
		t.Fatal("KnownProfiles exposed mutable package state")
	}
}

func TestUnknownProfileKeepsSemanticFallback(t *testing.T) {
	profile, exact := Resolve("unknown", "unknown")
	if exact || profile.Name != "validated-fallback" {
		t.Fatalf("Resolve() = (%q, %t)", profile.Name, exact)
	}
	if profile.Reader.ProviderInitializer.ArgumentCount == 0 || profile.Crypto.DecryptInit == "" {
		t.Fatalf("fallback anchors are incomplete: %+v", profile)
	}
}
