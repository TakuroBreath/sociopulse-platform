// Package api defines public contracts for the tenancy module.
// Other modules import only from this package — never from tenancy/service or tenancy/store.
//
// tenancy is the trunk module: every other module depends on it for tenant
// context, envelope encryption (DEK per payload, KEK per tenant), HMAC-SHA256
// phone hashing with per-tenant pepper, settings cache with NATS-driven
// invalidation, and per-tenant S3 bucket provisioning.
package api

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// TenantStatus is the active/suspended/archived lifecycle state of a tenant.
type TenantStatus string

const (
	TenantStatusActive    TenantStatus = "active"
	TenantStatusSuspended TenantStatus = "suspended"
	TenantStatusArchived  TenantStatus = "archived"
)

// Tenant is the public projection of a tenant row.
type Tenant struct {
	ID        uuid.UUID
	OrgCode   string // public code, e.g. "CC-MOSKVA-01"
	Name      string
	Status    TenantStatus
	KMSKEKID  string // Yandex KMS symmetric key ID
	CreatedAt time.Time
}

// CreateTenantRequest is the input to TenantService.Create.
type CreateTenantRequest struct {
	OrgCode string
	Name    string
}

// ListTenantsFilter narrows TenantService.List.
type ListTenantsFilter struct {
	Status  *TenantStatus
	OrgCode string // exact match if non-empty
	Limit   int
	Offset  int
}

// DataKey is one envelope-encryption data key (DEK). Plaintext is the
// raw 32 bytes for AES-256; Ciphertext is the KMS-wrapped blob persisted
// alongside the payload; KeyVersion identifies the KEK version that wrapped it.
type DataKey struct {
	Plaintext  []byte // 32 bytes for AES-256
	Ciphertext []byte // KMS-encrypted blob
	KeyVersion string // KEK version that wrapped this DEK
}

// SettingValue is a typed wrapper around a json.RawMessage with typed accessors.
// Settings are stored as jsonb in tenant_settings and accessors centralise
// type coercion so that callers do not duplicate parsing logic.
type SettingValue struct {
	raw json.RawMessage
}

// SettingValueFromAny encodes v into a SettingValue.
func SettingValueFromAny(v any) (SettingValue, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return SettingValue{}, err
	}
	return SettingValue{raw: b}, nil
}

// SettingValueFromRaw wraps an existing JSON byte slice without re-encoding.
// Caller is responsible for ensuring b is valid JSON.
func SettingValueFromRaw(b []byte) SettingValue {
	dup := make(json.RawMessage, len(b))
	copy(dup, b)
	return SettingValue{raw: dup}
}

// Raw returns the underlying JSON bytes. Returned slice is a copy; callers
// may mutate it freely without affecting the SettingValue.
func (v SettingValue) Raw() json.RawMessage {
	if len(v.raw) == 0 {
		return nil
	}
	dup := make(json.RawMessage, len(v.raw))
	copy(dup, v.raw)
	return dup
}

// AsString returns the value as a string. Returns ErrInvalidArgument-wrapped
// error if the encoded value is not a JSON string.
func (v SettingValue) AsString() (string, error) {
	var s string
	if err := json.Unmarshal(v.raw, &s); err != nil {
		return "", err
	}
	return s, nil
}

// AsInt returns the value as an int64.
func (v SettingValue) AsInt() (int64, error) {
	var n int64
	if err := json.Unmarshal(v.raw, &n); err != nil {
		return 0, err
	}
	return n, nil
}

// AsBool returns the value as a bool.
func (v SettingValue) AsBool() (bool, error) {
	var b bool
	if err := json.Unmarshal(v.raw, &b); err != nil {
		return false, err
	}
	return b, nil
}

// AsDuration returns the value as a time.Duration. Accepts both Go-style
// duration strings ("30s", "5m") and an integer number of nanoseconds.
func (v SettingValue) AsDuration() (time.Duration, error) {
	// Try string form first.
	var s string
	if err := json.Unmarshal(v.raw, &s); err == nil {
		return time.ParseDuration(s)
	}
	// Fall back to integer nanoseconds.
	var n int64
	if err := json.Unmarshal(v.raw, &n); err != nil {
		return 0, err
	}
	return time.Duration(n), nil
}

// AsJSON decodes the value into dst. dst must be a non-nil pointer.
func (v SettingValue) AsJSON(dst any) error {
	return json.Unmarshal(v.raw, dst)
}
