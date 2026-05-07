package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// SettingValue is the dynamically-typed value of a tenant_settings row.
//
// Construct via SettingValueFromAny; access via typed accessors. The wire
// format is JSON (jsonb in Postgres).
type SettingValue struct {
	raw json.RawMessage
}

// SettingValueFromAny converts a Go value into a SettingValue.
func SettingValueFromAny(v any) (SettingValue, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return SettingValue{}, fmt.Errorf("%w: marshal: %w", ErrInvalidArgument, err)
	}
	return SettingValue{raw: b}, nil
}

// SettingValueFromRaw constructs from already-marshalled JSON. The bytes are
// copied so the caller is free to mutate the source after the call returns.
func SettingValueFromRaw(b []byte) SettingValue {
	out := make(json.RawMessage, len(b))
	copy(out, b)
	return SettingValue{raw: out}
}

// Raw returns a copy of the underlying jsonb bytes. Returned slice is owned
// by the caller and may be mutated freely.
func (v SettingValue) Raw() json.RawMessage {
	if len(v.raw) == 0 {
		return nil
	}
	out := make(json.RawMessage, len(v.raw))
	copy(out, v.raw)
	return out
}

// AsString reads the value as a JSON string.
func (v SettingValue) AsString() (string, error) {
	var s string
	if err := json.Unmarshal(v.raw, &s); err != nil {
		return "", fmt.Errorf("%w: not a string: %w", ErrInvalidArgument, err)
	}
	return s, nil
}

// AsInt reads as a JSON number → int64.
func (v SettingValue) AsInt() (int64, error) {
	var n json.Number
	if err := json.Unmarshal(v.raw, &n); err != nil {
		return 0, fmt.Errorf("%w: not a number: %w", ErrInvalidArgument, err)
	}
	i, err := strconv.ParseInt(n.String(), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: not int64: %w", ErrInvalidArgument, err)
	}
	return i, nil
}

// AsBool reads as a JSON bool.
func (v SettingValue) AsBool() (bool, error) {
	var b bool
	if err := json.Unmarshal(v.raw, &b); err != nil {
		return false, fmt.Errorf("%w: not a bool: %w", ErrInvalidArgument, err)
	}
	return b, nil
}

// AsDuration reads as a duration-shaped string ("4h", "30m", "2h30m").
func (v SettingValue) AsDuration() (time.Duration, error) {
	s, err := v.AsString()
	if err != nil {
		return 0, err
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%w: not a duration: %w", ErrInvalidArgument, err)
	}
	return d, nil
}

// AsJSON unmarshals into the destination.
func (v SettingValue) AsJSON(dst any) error {
	if err := json.Unmarshal(v.raw, dst); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidArgument, err)
	}
	return nil
}

// SettingsCache is the per-tenant key/value cache.
//
// Reads: lazy-load from Postgres on miss, cache for TTL=30s, NATS subscriber
// on `tenant.<id>.settings.updated` invalidates the entry.
// Writes: write-through (UPDATE Postgres, then publish NATS event, then update cache).
//
// Method names use the Lookup* prefix (rather than Get*) to keep the Tenancy
// aggregate composable with TenantService.Get without a Go method-name
// collision. See doc.go for context.
type SettingsCache interface {
	// Lookup returns the value for key, or ErrNotFound.
	Lookup(ctx context.Context, tenantID uuid.UUID, key string) (SettingValue, error)
	// LookupWithDefault returns the value for key, falling back to def if missing.
	LookupWithDefault(ctx context.Context, tenantID uuid.UUID, key string, def SettingValue) (SettingValue, error)
	// LookupAll returns every setting for the tenant (snapshot).
	LookupAll(ctx context.Context, tenantID uuid.UUID) (map[string]SettingValue, error)
	// Set upserts a setting and publishes settings.updated for peer invalidation.
	Set(ctx context.Context, tenantID uuid.UUID, key string, value SettingValue) error
	// Delete removes a setting and publishes settings.updated.
	Delete(ctx context.Context, tenantID uuid.UUID, key string) error

	// InvalidateLocal drops the in-memory entry for (tenantID, key) without
	// publishing a NATS event. Used by the NATS subscriber on incoming
	// invalidation messages from peer pods.
	InvalidateLocal(tenantID uuid.UUID, key string)

	// InvalidateAllLocal drops all entries for a tenant. Used on tenant.archived.
	InvalidateAllLocal(tenantID uuid.UUID)
}
