// Package compatibility defines the versioned runtime anchors used by Leafport.
package compatibility

import "strings"

// MethodAnchor describes one Objective-C method without depending only on its
// current symbol name. ArgumentCount includes the implicit self and _cmd
// arguments used by the Objective-C runtime.
type MethodAnchor struct {
	Role          string
	Names         []string
	Prefix        string
	ArgumentCount uint32
	SemanticTerms [][]string
}

// ReaderAnchors describes the minimum Objective-C interface needed to open a
// local book. Alternative names are tried in order and every resolved method
// is independently validated.
type ReaderAnchors struct {
	ProviderClasses     []string
	BookClasses         []string
	ProviderInitializer MethodAnchor
	BookInitializer     MethodAnchor
	ResourceBundle      MethodAnchor
	ICUDataDirectory    MethodAnchor
}

// CryptoAnchors contains the stable OpenSSL entry points used by the bridge.
type CryptoAnchors struct {
	DecryptInit      string
	CipherKeyLength  string
	ContextKeyLength string
}

// Profile records the independent identifiers and runtime anchors for one
// verified reader build.
type Profile struct {
	Name       string
	AppVersion string
	UUID       string
	TextSHA256 string
	Reader     ReaderAnchors
	Crypto     CryptoAnchors
}

var defaultReaderAnchors = ReaderAnchors{
	ProviderClasses: []string{"KRFDRMDataProvider"},
	BookClasses:     []string{"KRFBook"},
	ProviderInitializer: MethodAnchor{
		Role: "DRM provider initializer", Names: []string{"initWithAccountSecrets:kindleSerialNumber:voucherList:"},
		Prefix: "init", ArgumentCount: 5,
		SemanticTerms: [][]string{{"account"}, {"serial", "device"}, {"voucher"}},
	},
	BookInitializer: MethodAnchor{
		Role: "book initializer", Names: []string{"initWithURL:DRMDataProvider:containers:error:"},
		Prefix: "init", ArgumentCount: 6,
		SemanticTerms: [][]string{{"url"}, {"drm"}, {"container"}, {"error"}},
	},
	ResourceBundle: MethodAnchor{
		Role: "resource bundle setter", Names: []string{"setResourceBundlePath:"},
		Prefix: "set", ArgumentCount: 3,
		SemanticTerms: [][]string{{"resource"}, {"bundle"}},
	},
	ICUDataDirectory: MethodAnchor{
		Role: "ICU data-directory setter", Names: []string{"setICUDataDirectory:"},
		Prefix: "set", ArgumentCount: 3,
		SemanticTerms: [][]string{{"icu"}, {"data"}},
	},
}

var defaultCryptoAnchors = CryptoAnchors{
	DecryptInit:      "EVP_DecryptInit_ex",
	CipherKeyLength:  "EVP_CIPHER_key_length",
	ContextKeyLength: "EVP_CIPHER_CTX_key_length",
}

var knownProfiles = []Profile{
	{
		Name:       "reader-7.65-arm64",
		AppVersion: "7.65 build 1.465815.10",
		UUID:       "F93581D6-2609-3696-9120-14F10F2C1A08",
		TextSHA256: "24c62a02ddbd78a9d9afa5f2209dc6fbbf58a2d8ec0ed7ce850f43fe11f3d029",
		Reader:     defaultReaderAnchors,
		Crypto:     defaultCryptoAnchors,
	},
}

// Resolve returns an exact verified profile when both independent build
// identifiers match. Unknown builds receive conservative semantic anchors and
// must pass all runtime validation before they are used.
func Resolve(uuid, textSHA256 string) (Profile, bool) {
	for _, profile := range knownProfiles {
		if strings.EqualFold(profile.UUID, uuid) && strings.EqualFold(profile.TextSHA256, textSHA256) {
			return cloneProfile(profile), true
		}
	}
	return Profile{
		Name:   "validated-fallback",
		Reader: cloneReaderAnchors(defaultReaderAnchors),
		Crypto: defaultCryptoAnchors,
	}, false
}

// KnownProfiles returns defensive copies of all verified build profiles.
func KnownProfiles() []Profile {
	profiles := make([]Profile, len(knownProfiles))
	for index, profile := range knownProfiles {
		profiles[index] = cloneProfile(profile)
	}
	return profiles
}

func cloneProfile(profile Profile) Profile {
	profile.Reader = cloneReaderAnchors(profile.Reader)
	return profile
}

func cloneReaderAnchors(anchors ReaderAnchors) ReaderAnchors {
	anchors.ProviderClasses = append([]string(nil), anchors.ProviderClasses...)
	anchors.BookClasses = append([]string(nil), anchors.BookClasses...)
	anchors.ProviderInitializer = cloneMethodAnchor(anchors.ProviderInitializer)
	anchors.BookInitializer = cloneMethodAnchor(anchors.BookInitializer)
	anchors.ResourceBundle = cloneMethodAnchor(anchors.ResourceBundle)
	anchors.ICUDataDirectory = cloneMethodAnchor(anchors.ICUDataDirectory)
	return anchors
}

func cloneMethodAnchor(anchor MethodAnchor) MethodAnchor {
	anchor.Names = append([]string(nil), anchor.Names...)
	terms := make([][]string, len(anchor.SemanticTerms))
	for index, alternatives := range anchor.SemanticTerms {
		terms[index] = append([]string(nil), alternatives...)
	}
	anchor.SemanticTerms = terms
	return anchor
}
