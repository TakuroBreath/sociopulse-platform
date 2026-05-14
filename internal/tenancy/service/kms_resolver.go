package service

import (
	"context"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/tenancy/api"
	"github.com/sociopulse/platform/pkg/encryption"
)

// KMSResolverConfig controls the DEK cache shape. Defaults are filled in
// by NewKMSResolver — the zero value is valid and yields a 5-minute TTL,
// 1024-tenant cap.
//
// Task 4 swaps the bag-of-maps cache for an LRU with proactive TTL eviction
// and a KEK-version-aware key; the resolver surface stays the same.
type KMSResolverConfig struct {
	// DEKCacheTTL is how long an unwrapped DEK plaintext lives in
	// process memory. Default 5 minutes (spec §6.2). The cache stores
	// the ciphertext too, so a stale entry only forces a re-Decrypt
	// against KMS — never a re-Generate.
	DEKCacheTTL time.Duration

	// DEKCacheSize is the maximum number of distinct (tenantID,
	// keyVersion) entries cached at once. Default 1024.
	DEKCacheSize int

	// DEKCacheTickRate controls how often the eviction goroutine sweeps
	// expired entries. Defaults to TTL/4 clamped to [1s, 1m].
	DEKCacheTickRate time.Duration
}

func (c *KMSResolverConfig) defaults() {
	if c.DEKCacheTTL <= 0 {
		c.DEKCacheTTL = 5 * time.Minute
	}
	if c.DEKCacheSize <= 0 {
		c.DEKCacheSize = 1024
	}
}

// KMSResolverImpl is the concrete api.KMSResolver. It wraps a provider-
// specific KMSClient (Yandex KMS in production; the local in-process
// fallback in dev) with a per-(tenant, KEK-version) DEK cache so the hot
// path of Encrypt/Decrypt avoids round-tripping every payload to the KMS.
//
// Thread-safety: the underlying DEKCache is internally synchronised; the
// resolver itself is stateless beyond its dependencies, so all methods are
// safe for concurrent use.
//
// Cache key is (tenantID, KEK-version). KEK rotation produces a fresh
// version label from KMS GenerateDataKey, which addresses a different
// cache slot — the old entry stays resident long enough to fast-path
// concurrent Decrypts of in-flight ciphertexts wrapped by the previous
// version, then ages out via TTL. Operators who want immediate eviction
// call InvalidateCache(tenantID), which drops every version for the
// tenant atomically.
//
// Compile-time check: implements api.KMSResolver.
type KMSResolverImpl struct {
	logger *zap.Logger
	store  api.Store
	kms    api.KMSClient
	cfg    KMSResolverConfig

	cache *DEKCache
}

// Compile-time interface check.
var _ api.KMSResolver = (*KMSResolverImpl)(nil)

// NewKMSResolver constructs a KMSResolver from already-built dependencies.
//
// The returned implementation is goroutine-safe: callers can share a
// single resolver across all request handlers. The resolver owns a
// background eviction goroutine — call Close at process shutdown to
// terminate it cleanly (production wires this through cmd/api lifecycle;
// tests use t.Cleanup).
//
// The eviction goroutine's lifetime is bound to context.Background by
// default; production wiring uses newKMSResolverWithContext to pass the
// cmd/api root context so cancellation also tears the goroutine down.
func NewKMSResolver(logger *zap.Logger, store api.Store, kms api.KMSClient, cfg KMSResolverConfig) *KMSResolverImpl {
	return newKMSResolverWithContext(context.Background(), logger, store, kms, cfg)
}

// newKMSResolverWithContext is the internal constructor that the package's
// register seam uses to bind the cache's eviction goroutine to cmd/api's
// root context. Tests stick with NewKMSResolver + t.Cleanup; this overload
// is unexported because callers outside the module shouldn't reach for it.
func newKMSResolverWithContext(ctx context.Context, logger *zap.Logger, store api.Store, kms api.KMSClient, cfg KMSResolverConfig) *KMSResolverImpl {
	cfg.defaults()
	cache := NewDEKCacheWithContext(ctx, DEKCacheConfig{
		Size:     cfg.DEKCacheSize,
		TTL:      cfg.DEKCacheTTL,
		TickRate: cfg.DEKCacheTickRate,
	})
	return &KMSResolverImpl{
		logger: logger,
		store:  store,
		kms:    kms,
		cfg:    cfg,
		cache:  cache,
	}
}

// Close terminates the resolver's background eviction goroutine. Idempotent:
// calling twice is safe.
func (r *KMSResolverImpl) Close() {
	r.cache.Stop()
}

// EnsureKEK returns the tenant's KEK ID after fetching the row through
// the BYPASSRLS Store. If the tenant has no KEK provisioned (a state
// that should not occur after TenantService.Create), the call surfaces
// api.ErrInvalidArgument so the operator can investigate.
func (r *KMSResolverImpl) EnsureKEK(ctx context.Context, tenantID uuid.UUID) (string, error) {
	t, err := r.store.Get(ctx, tenantID)
	if err != nil {
		return "", fmt.Errorf("kms-resolver: get tenant: %w", err)
	}
	if t.KMSKEKID == "" {
		return "", fmt.Errorf("%w: tenant has no KEK provisioned", api.ErrInvalidArgument)
	}
	return t.KMSKEKID, nil
}

// GenerateDataKey mints a fresh DEK via the configured KMSClient. The
// plaintext is returned to the caller for one-shot use; the wrapped
// ciphertext should be persisted alongside the encrypted payload.
//
// The resolver does NOT cache the result here — Encrypt is the cache
// owner. Callers who use GenerateDataKey directly (e.g. recording
// uploaders generating per-recording DEKs) own the lifecycle of the
// returned plaintext and SHOULD zero it after use.
func (r *KMSResolverImpl) GenerateDataKey(ctx context.Context, tenantID uuid.UUID) (api.DataKey, error) {
	kekID, err := r.EnsureKEK(ctx, tenantID)
	if err != nil {
		return api.DataKey{}, err
	}
	pt, ct, version, err := r.kms.GenerateDataKey(ctx, kekID)
	if err != nil {
		return api.DataKey{}, fmt.Errorf("%w: generate data key: %w", api.ErrKMSUnavailable, err)
	}
	if len(pt) != encryption.KeyLen {
		return api.DataKey{}, fmt.Errorf("kms-resolver: dek plaintext must be %d bytes, got %d",
			encryption.KeyLen, len(pt))
	}
	return api.DataKey{Plaintext: pt, Ciphertext: ct, KeyVersion: version}, nil
}

// Envelope ciphertext layout:
//
// Plan 13.2.5 Task 6 introduced AAD-bound ciphertext writes. Every new
// write is v2 (0x02 prefix); legacy production data without a version
// byte continues to decrypt under the v1 (no AAD) path. Layouts:
//
//	v2  : [0x02][4-byte BE wrapped-DEK length][wrapped-DEK][AES-GCM blob, AAD = BuildAAD(tenant, scope, rowID)]
//	v1' : [0x01][4-byte BE wrapped-DEK length][wrapped-DEK][AES-GCM blob, AAD = nil]   (explicit-legacy; produced by a future migration)
//	v1  :       [4-byte BE wrapped-DEK length][wrapped-DEK][AES-GCM blob, AAD = nil]   (pre-13.2.5 production data; no version byte)
//
// Dispatch on the FIRST byte:
//   - 0x02 → v2 path; strip version, AAD = BuildAAD
//   - 0x01 → versioned legacy; strip version, AAD = nil
//   - 0x00 → unprefixed legacy (existing prod data — the high byte of the
//     length-prefix is always 0x00 because wrapped-DEK ≤ 1 MiB); decode
//     bytes verbatim, AAD = nil
//   - other → ErrInvalidArgument (unknown version)
//
// Bundling the wrapped DEK with the payload makes ciphertext self-contained:
// any process with KMS access can decrypt it without consulting an
// out-of-band DEK column. The trade-off is ~50 bytes of overhead per
// message, which is acceptable for the short PII (phones, emails) that
// flows through this path.
const wrappedDEKLenBytes = 4

// Ciphertext version bytes — first byte of the resolver-emitted blob.
const (
	// ciphertextVersionLegacyUnprefixed marks the historical envelope —
	// no leading byte, AAD = nil. Existing production data has this
	// implicit "version": the wrapped-DEK length-prefix high byte is
	// always 0x00 because we cap wrapped-DEK at 1 MiB (= 0x100000).
	ciphertextVersionLegacyUnprefixed byte = 0x00

	// ciphertextVersionLegacy is an explicit "no AAD" marker — supports
	// a future migration that stamps a version byte onto in-place data
	// without re-encrypting. NOT produced by Encrypt today.
	ciphertextVersionLegacy byte = 0x01

	// ciphertextVersionAAD marks a v2 envelope: AAD = BuildAAD(tenant,
	// scope, rowID). Encrypt ALWAYS writes this version.
	ciphertextVersionAAD byte = 0x02
)

// maxWrappedDEKLen guards the int→uint32 conversion in the envelope
// header. Yandex KMS-wrapped DEKs are well under 1 KiB; we cap at 1 MiB
// as a sanity bound so a misuse never produces a non-decodable header.
const maxWrappedDEKLen = 1 << 20

// Encrypt performs envelope AES-256-GCM with a cached DEK, binding
// (tenant, scope, rowID) into the AEAD authentication tag via
// pkg/encryption.BuildAAD. Every emitted ciphertext starts with the
// ciphertextVersionAAD (0x02) byte; legacy unprefixed/0x01 envelopes
// are decrypt-only (backward compatibility) and never produced here.
//
// Cache miss triggers a fresh GenerateDataKey, after which subsequent
// Encrypts on the same (tenant, KEK-version) reuse the same DEK until
// InvalidateCache is called or the cache evicts.
func (r *KMSResolverImpl) Encrypt(ctx context.Context, tenantID uuid.UUID, scope, rowID string, plaintext []byte) ([]byte, error) {
	dek, err := r.resolveDEKForEncrypt(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if len(dek.Ciphertext) > maxWrappedDEKLen {
		return nil, fmt.Errorf("kms-resolver: wrapped dek length %d exceeds %d byte cap",
			len(dek.Ciphertext), maxWrappedDEKLen)
	}
	aad := encryption.BuildAAD(tenantID, scope, rowID)
	body, err := encryption.Encrypt(dek.Plaintext, plaintext, aad)
	if err != nil {
		return nil, fmt.Errorf("kms-resolver: aes-gcm encrypt: %w", err)
	}
	out := make([]byte, 0, 1+wrappedDEKLenBytes+len(dek.Ciphertext)+len(body))
	out = append(out, ciphertextVersionAAD)
	out = binary.BigEndian.AppendUint32(out, uint32(len(dek.Ciphertext))) //nolint:gosec // bounded above
	out = append(out, dek.Ciphertext...)
	out = append(out, body...)
	return out, nil
}

// Decrypt unpacks the envelope produced by Encrypt. Reads the leading
// version byte to choose between AAD-bound (v2) and legacy (v1 /
// v1-unprefixed) paths.
//
// Cache hit fast-paths when the embedded wrapped-DEK matches the cached
// one; otherwise the resolver calls KMS.Decrypt to unwrap the DEK and
// updates the cache.
func (r *KMSResolverImpl) Decrypt(ctx context.Context, tenantID uuid.UUID, scope, rowID string, ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < wrappedDEKLenBytes {
		return nil, fmt.Errorf("%w: ciphertext shorter than length prefix", api.ErrInvalidArgument)
	}

	body, wrappedDEK, aad, err := r.decodeCiphertext(tenantID, scope, rowID, ciphertext)
	if err != nil {
		return nil, err
	}

	dek, err := r.resolveDEKForDecrypt(ctx, tenantID, wrappedDEK)
	if err != nil {
		return nil, err
	}
	pt, err := encryption.Decrypt(dek.Plaintext, body, aad)
	if err != nil {
		// Surface as ErrInvalidArgument: the wrapped-DEK was authentic
		// (KMS.Decrypt succeeded) but the body is corrupt. We don't map
		// to ErrKMSUnavailable here because the failure is on caller-
		// supplied bytes, not the KMS service.
		return nil, fmt.Errorf("%w: aes-gcm open: %w", api.ErrInvalidArgument, err)
	}
	return pt, nil
}

// decodeCiphertext dispatches on the leading version byte and returns
// the AES-GCM body, the wrapped-DEK slice, and the AAD to use. Caller
// owns slice lifetime — returned slices alias `ciphertext`.
//
// Layouts (see resolver header):
//   - 0x02 → v2: strip byte, decode framing, AAD = BuildAAD
//   - 0x01 → versioned legacy: strip byte, decode framing, AAD = nil
//   - 0x00 → unprefixed legacy: decode framing on the whole input, AAD = nil
//   - else → unknown version (ErrInvalidArgument)
func (r *KMSResolverImpl) decodeCiphertext(tenantID uuid.UUID, scope, rowID string, ciphertext []byte) (body, wrappedDEK, aad []byte, err error) {
	switch ciphertext[0] {
	case ciphertextVersionAAD:
		body, wrappedDEK, err = r.parseEnvelope(ciphertext[1:])
		if err != nil {
			return nil, nil, nil, err
		}
		aad = encryption.BuildAAD(tenantID, scope, rowID)
		return body, wrappedDEK, aad, nil

	case ciphertextVersionLegacy:
		body, wrappedDEK, err = r.parseEnvelope(ciphertext[1:])
		if err != nil {
			return nil, nil, nil, err
		}
		// AAD = nil for legacy envelopes; scope/rowID are intentionally
		// not consulted.
		return body, wrappedDEK, nil, nil

	case ciphertextVersionLegacyUnprefixed:
		// No version byte: pre-13.2.5 production data. The "0x00" is the
		// high byte of the length-prefix.
		body, wrappedDEK, err = r.parseEnvelope(ciphertext)
		if err != nil {
			return nil, nil, nil, err
		}
		return body, wrappedDEK, nil, nil

	default:
		return nil, nil, nil, fmt.Errorf("%w: unknown ciphertext version 0x%02x", api.ErrInvalidArgument, ciphertext[0])
	}
}

// parseEnvelope splits a length-prefixed envelope into (body, wrappedDEK).
// Used by both the v2 and legacy decode paths after the optional version
// byte has been stripped.
func (r *KMSResolverImpl) parseEnvelope(envelope []byte) (body, wrappedDEK []byte, err error) {
	if len(envelope) < wrappedDEKLenBytes {
		return nil, nil, fmt.Errorf("%w: ciphertext shorter than length prefix", api.ErrInvalidArgument)
	}
	ctLen := binary.BigEndian.Uint32(envelope[:wrappedDEKLenBytes])
	if uint64(wrappedDEKLenBytes)+uint64(ctLen) > uint64(len(envelope)) {
		return nil, nil, fmt.Errorf("%w: wrapped-DEK length %d overshoots ciphertext", api.ErrInvalidArgument, ctLen)
	}
	wrappedDEK = envelope[wrappedDEKLenBytes : wrappedDEKLenBytes+ctLen]
	body = envelope[wrappedDEKLenBytes+ctLen:]
	return body, wrappedDEK, nil
}

// InvalidateCache drops every cached DEK for the tenant — across all
// KEK versions. Called after KEK rotation, tenant suspension, or on
// receipt of a peer-cache-invalidation NATS message.
func (r *KMSResolverImpl) InvalidateCache(tenantID uuid.UUID) {
	r.cache.InvalidateTenant(tenantID)
}

// resolveDEKForEncrypt returns a cached DEK or mints a fresh one via
// GenerateDataKey. The result is committed to the cache so the next
// Encrypt is a hit.
//
// On the cache-miss path we discover the KEK version only from the
// GenerateDataKey response, so the cache key is computed after the
// network call. On the cache-hit path we look up by tenant alone (we
// don't yet know which version the caller wants); when we find a hit
// we trust it — concurrent KEK rotation is rare and the operator is
// expected to call InvalidateCache to force fresh material.
//
// To preserve KEK-version-aware behaviour the resolver scans the cache
// for any entry under the tenant; if found, returns it. If absent, the
// fresh DEK is keyed by (tenant, returned-version).
func (r *KMSResolverImpl) resolveDEKForEncrypt(ctx context.Context, tenantID uuid.UUID) (*CachedDEK, error) {
	if hit, ok := r.cacheLookupAnyVersion(tenantID); ok {
		return hit, nil
	}
	dk, err := r.GenerateDataKey(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	entry := &CachedDEK{
		Plaintext:  dk.Plaintext,
		Ciphertext: dk.Ciphertext,
		KeyVersion: dk.KeyVersion,
	}
	r.cache.Put(DEKCacheKey{TenantID: tenantID, KEKVersion: dk.KeyVersion}, entry)
	return entry, nil
}

// resolveDEKForDecrypt returns the cached DEK if its wrapped form
// matches the embedded one. Otherwise it asks KMS to unwrap the
// embedded DEK and commits the result.
func (r *KMSResolverImpl) resolveDEKForDecrypt(ctx context.Context, tenantID uuid.UUID, wrappedDEK []byte) (*CachedDEK, error) {
	if hit, ok := r.cacheLookupAnyVersion(tenantID); ok {
		// Constant-time compare so we don't leak timing about the
		// wrapped-DEK contents to an adversary feeding crafted bodies.
		if subtle.ConstantTimeCompare(hit.Ciphertext, wrappedDEK) == 1 {
			return hit, nil
		}
	}
	kekID, err := r.EnsureKEK(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	pt, version, err := r.kms.Decrypt(ctx, kekID, wrappedDEK)
	if err != nil {
		// Map provider errors that look transient to ErrKMSUnavailable
		// so callers can retry. ErrKEKNotFound passes through.
		if errors.Is(err, api.ErrKEKNotFound) || errors.Is(err, api.ErrInvalidWrappedDEK) {
			return nil, fmt.Errorf("kms-resolver: decrypt: %w", err)
		}
		return nil, fmt.Errorf("%w: kms decrypt: %w", api.ErrKMSUnavailable, err)
	}
	if len(pt) != encryption.KeyLen {
		return nil, fmt.Errorf("kms-resolver: unwrapped dek must be %d bytes, got %d",
			encryption.KeyLen, len(pt))
	}
	entry := &CachedDEK{
		Plaintext:  pt,
		Ciphertext: append([]byte(nil), wrappedDEK...),
		KeyVersion: version,
	}
	r.cache.Put(DEKCacheKey{TenantID: tenantID, KEKVersion: version}, entry)
	return entry, nil
}

// cacheLookupAnyVersion returns the most-recently-used cached DEK for the
// tenant, regardless of KEK version. The LRU order ensures we prefer the
// newest version when one is resident — the bookkeeping for "which version
// is current" lives in KMS, not the cache.
//
// Implementation note: the underlying DEKCache is keyed by (tenantID,
// version) so a strict per-key Get cannot answer "any version". We expose
// a thin scan method on DEKCache for this use case; in practice the cache
// holds at most a handful of versions per tenant during a rotation
// window, so a linear scan is bounded.
func (r *KMSResolverImpl) cacheLookupAnyVersion(tenantID uuid.UUID) (*CachedDEK, bool) {
	return r.cache.GetByTenant(tenantID)
}
