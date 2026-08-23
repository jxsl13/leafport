// Package readerconfig reads the local reader registration and voucher
// requirements without exposing credential values in logs.
package readerconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"howett.net/plist"
)

// Credentials contains the minimum local registration material accepted by
// the reader's DRM provider.
type Credentials struct {
	DSN                 string
	HashedAccountSecret string
	AccountSecrets      []string
}

// VoucherRequirements summarizes credential markers embedded in a bundle's
// protected-data voucher. Values themselves are never returned.
type VoucherRequirements struct {
	Vouchers      int
	AccountSecret bool
	ClientID      bool
}

// LoadCredentials decodes the device serial and optional hashed account
// secret from the reader preferences. The hashed value is deliberately kept
// separate from AccountSecrets: KFX vouchers lock against the original
// account secret, so substituting its hash produces an invalid lock parameter.
func LoadCredentials(path string) (Credentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Credentials{}, fmt.Errorf("read reader preferences: %w", err)
	}
	var preferences map[string]any
	if _, err := plist.Unmarshal(data, &preferences); err != nil {
		return Credentials{}, fmt.Errorf("decode reader preferences: %w", err)
	}
	raw, ok := preferences["kindle_notifications_persist_NotificationsCustomData"].(string)
	if !ok || raw == "" {
		return Credentials{}, errors.New("reader device registration is unavailable")
	}
	var payload struct {
		DSN string `json:"dsn"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return Credentials{}, fmt.Errorf("decode reader device registration: %w", err)
	}
	payload.DSN = strings.TrimSpace(payload.DSN)
	if payload.DSN == "" {
		return Credentials{}, errors.New("reader device serial is empty")
	}
	credentials := Credentials{DSN: payload.DSN}
	if secret, ok := preferences["HashedAccountSecret"].(string); ok {
		if secret = strings.TrimSpace(secret); secret != "" {
			credentials.HashedAccountSecret = secret
		}
	}
	return credentials, nil
}

// InspectVouchers reports the credential types required by all voucher files
// in one downloaded bundle.
func InspectVouchers(bundle string) (VoucherRequirements, error) {
	var requirements VoucherRequirements
	err := filepath.WalkDir(bundle, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".voucher") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		requirements.Vouchers++
		requirements.AccountSecret = requirements.AccountSecret || bytes.Contains(data, []byte("ACCOUNT_SECRET"))
		requirements.ClientID = requirements.ClientID || bytes.Contains(data, []byte("CLIENT_ID"))
		return nil
	})
	if err != nil {
		return requirements, fmt.Errorf("inspect reader vouchers: %w", err)
	}
	if requirements.Vouchers == 0 {
		return requirements, errors.New("bundle contains no DRM voucher")
	}
	return requirements, nil
}
