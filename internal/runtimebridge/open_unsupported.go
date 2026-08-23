//go:build !darwin

package runtimebridge

import (
	"errors"

	"github.com/jxsl13/leafport/internal/machoutil"
)

type Capture struct {
	Key          []byte
	Uses         int
	Profile      string
	KnownProfile bool
	Fingerprint  machoutil.BinaryFingerprint
}

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

func OpenBook(_, _, _, _ string) (Capture, error) {
	return Capture{}, errors.New("reader runtime bridge requires macOS")
}

func OpenBookWithAccountSecret(_, _, _, _, _ string) (Capture, error) {
	return Capture{}, errors.New("reader runtime bridge requires macOS")
}

func Doctor(_, _ string) (DoctorReport, error) {
	return DoctorReport{}, errors.New("reader runtime bridge requires macOS")
}
