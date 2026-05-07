package service

import (
	"context"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/tenancy/api"
	"github.com/sociopulse/platform/pkg/encryption"
)

// KMSResolverConfig controls the DEK cache shape. Defaults are filled in
// by NewKMSResolver — the zero value is valid and yields a 5-minute TTL,
// 1024-tenant cap. Task 4 swaps the bag-of-maps cache for an LRU with
// proactive eviction; the surface stays the same.
type KMSResolverConfig struct {
	// DEKCacheTTL is how long an unwrapped DEK plaintext lives in
	// process memory. Default 5 minutes (spec §6.2). The cache stores
	// the ciphertext too, so a stale entry only forces a re-Decrypt
	// against KMS — never a re-Generate.
	DEKCacheTTL time.Duration

	// DEKCacheSize is the maximum number of distinct (tenantID,
	// keyVersion) entries cached at once. Default 1024.
	//
	// In Task 3 the cache is unbounded — the field is accepted by the
	// constructor for API compatibility with Task 4 but does not yet
	// trigger eviction.
	DEKCacheSize int
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
// fallback in dev) with a per-tenant DEK cache so the hot path of
// Encrypt/Decrypt avoids round-tripping every payload to the KMS.
//
// Thread-safety: cache is protected by RWMutex. Reads (cache hits)
// take RLock; misses take Lock to insert. Sensitive material (DEK
// plaintext, wrapped DEK ciphertext) lives only in process memory and
// is never logged or surfaced beyond the api.KMSResolver surface.
//
// Cache key is the tenant ID alone. This is a TASK 3 simplification:
// after KEK rotation, the operator must call InvalidateCache(tenantID)
// to evict the stale entry — the cached DEK was wrapped by the old KEK
// version. Task 4 hardens the cache to ((tenantID, kekVersion)) and
// invalidates entries automatically on receipt of the rotation NATS
// message.
//
// Compile-time check: implements api.KMSResolver.
type KMSResolverImpl struct {
	logger *zap.Logger
	store  api.Store
	kms    api.KMSClient
	cfg    KMSResolverConfig

	mu    sync.RWMutex
	cache map[uuid.UUID]*cachedDEK
}

// cachedDEK holds an unwrapped DEK plus its KMS-wrapped form. Storing the
// ciphertext alongside the plaintext lets Decrypt fast-path when the
// incoming wrapped-DEK matches what's already cached — common after
// Encrypt because the same DEK is reused for every payload until the
// cache evicts.
type cachedDEK struct {
	plaintext  []byte
	ciphertext []byte
	keyVersion string
	insertedAt time.Time
}

// Compile-time interface check.
var _ api.KMSResolver = (*KMSResolverImpl)(nil)

// NewKMSResolver constructs a KMSResolver from already-built dependencies.
//
// The returned implementation is goroutine-safe: callers can share a
// single resolver across all request handlers.
func NewKMSResolver(logger *zap.Logger, store api.Store, kms api.KMSClient, cfg KMSResolverConfig) *KMSResolverImpl {
	cfg.defaults()
	return &KMSResolverImpl{
		logger: logger,
		store:  store,
		kms:    kms,
		cfg:    cfg,
		cache:  make(map[uuid.UUID]*cachedDEK),
	}
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
//	[4-byte big-endian wrapped-DEK length][wrapped-DEK][AES-GCM blob]
//
// Bundling the wrapped DEK with the payload makes ciphertext self-contained:
// any process with KMS access can decrypt it without consulting an
// out-of-band DEK column. The trade-off is ~50 bytes of overhead per
// message, which is acceptable for the short PII (phones, emails) that
// flows through this path.
const wrappedDEKLenBytes = 4

// maxWrappedDEKLen guards the int→uint32 conversion in the envelope
// header. Yandex KMS-wrapped DEKs are well under 1 KiB; we cap at 1 MiB
// as a sanity bound so a misuse never produces a non-decodable header.
const maxWrappedDEKLen = 1 << 20

// Encrypt performs envelope AES-256-GCM with a cached DEK. Cache miss
// triggers a fresh GenerateDataKey, after which subsequent Encrypts on
// the same tenant reuse the same DEK until InvalidateCache is called or
// the cache evicts.
func (r *KMSResolverImpl) Encrypt(ctx context.Context, tenantID uuid.UUID, plaintext []byte) ([]byte, error) {
	dek, err := r.resolveDEKForEncrypt(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if len(dek.ciphertext) > maxWrappedDEKLen {
		return nil, fmt.Errorf("kms-resolver: wrapped dek length %d exceeds %d byte cap",
			len(dek.ciphertext), maxWrappedDEKLen)
	}
	body, err := encryption.Encrypt(dek.plaintext, plaintext, nil)
	if err != nil {
		return nil, fmt.Errorf("kms-resolver: aes-gcm encrypt: %w", err)
	}
	out := make([]byte, 0, wrappedDEKLenBytes+len(dek.ciphertext)+len(body))
	out = binary.BigEndian.AppendUint32(out, uint32(len(dek.ciphertext))) //nolint:gosec // bounded above
	out = append(out, dek.ciphertext...)
	out = append(out, body...)
	return out, nil
}

// Decrypt unpacks the envelope produced by Encrypt. Cache hit fast-paths
// when the embedded wrapped-DEK matches the cached one; otherwise the
// resolver calls KMS.Decrypt to unwrap the DEK and updates the cache.
func (r *KMSResolverImpl) Decrypt(ctx context.Context, tenantID uuid.UUID, ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < wrappedDEKLenBytes {
		return nil, fmt.Errorf("%w: ciphertext shorter than length prefix", api.ErrInvalidArgument)
	}
	ctLen := binary.BigEndian.Uint32(ciphertext[:wrappedDEKLenBytes])
	if uint64(wrappedDEKLenBytes)+uint64(ctLen) > uint64(len(ciphertext)) {
		return nil, fmt.Errorf("%w: wrapped-DEK length %d overshoots ciphertext", api.ErrInvalidArgument, ctLen)
	}
	wrappedDEK := ciphertext[wrappedDEKLenBytes : wrappedDEKLenBytes+ctLen]
	body := ciphertext[wrappedDEKLenBytes+ctLen:]

	dek, err := r.resolveDEKForDecrypt(ctx, tenantID, wrappedDEK)
	if err != nil {
		return nil, err
	}
	pt, err := encryption.Decrypt(dek.plaintext, body, nil)
	if err != nil {
		// Surface as ErrInvalidArgument: the wrapped-DEK was authentic
		// (KMS.Decrypt succeeded) but the body is corrupt. We don't map
		// to ErrKMSUnavailable here because the failure is on caller-
		// supplied bytes, not the KMS service.
		return nil, fmt.Errorf("%w: aes-gcm open: %w", api.ErrInvalidArgument, err)
	}
	return pt, nil
}

// InvalidateCache drops the cached DEK for the tenant. Called after
// KEK rotation, tenant suspension, or on receipt of a peer-cache-
// invalidation NATS message (Task 4 wires the subscriber).
func (r *KMSResolverImpl) InvalidateCache(tenantID uuid.UUID) {
	r.mu.Lock()
	delete(r.cache, tenantID)
	r.mu.Unlock()
}

// resolveDEKForEncrypt returns a cached DEK or mints a fresh one via
// GenerateDataKey. The result is committed to the cache so the next
// Encrypt is a hit.
func (r *KMSResolverImpl) resolveDEKForEncrypt(ctx context.Context, tenantID uuid.UUID) (*cachedDEK, error) {
	if hit, ok := r.lookup(tenantID); ok {
		return hit, nil
	}
	dk, err := r.GenerateDataKey(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	entry := &cachedDEK{
		plaintext:  dk.Plaintext,
		ciphertext: dk.Ciphertext,
		keyVersion: dk.KeyVersion,
		insertedAt: time.Now(),
	}
	r.put(tenantID, entry)
	return entry, nil
}

// resolveDEKForDecrypt returns the cached DEK if its wrapped form
// matches the embedded one. Otherwise it asks KMS to unwrap the
// embedded DEK and commits the result.
func (r *KMSResolverImpl) resolveDEKForDecrypt(ctx context.Context, tenantID uuid.UUID, wrappedDEK []byte) (*cachedDEK, error) {
	if hit, ok := r.lookup(tenantID); ok {
		// Constant-time compare so we don't leak timing about the
		// wrapped-DEK contents to an adversary feeding crafted bodies.
		if subtle.ConstantTimeCompare(hit.ciphertext, wrappedDEK) == 1 {
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
	entry := &cachedDEK{
		plaintext:  pt,
		ciphertext: append([]byte(nil), wrappedDEK...),
		keyVersion: version,
		insertedAt: time.Now(),
	}
	r.put(tenantID, entry)
	return entry, nil
}

func (r *KMSResolverImpl) lookup(tenantID uuid.UUID) (*cachedDEK, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	hit, ok := r.cache[tenantID]
	if !ok {
		return nil, false
	}
	if r.cfg.DEKCacheTTL > 0 && time.Since(hit.insertedAt) > r.cfg.DEKCacheTTL {
		return nil, false
	}
	return hit, true
}

func (r *KMSResolverImpl) put(tenantID uuid.UUID, entry *cachedDEK) {
	r.mu.Lock()
	r.cache[tenantID] = entry
	r.mu.Unlock()
}
