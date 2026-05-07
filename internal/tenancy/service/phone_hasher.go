package service

import (
	"container/list"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/tenancy/api"
)

// PhoneHasherConfig configures the per-tenant pepper LRU+TTL cache. The zero
// value is valid: defaults() fills it with a 5-minute TTL and 1024 slots.
//
// The cache lives at the service layer rather than inside the store because
// the store is intentionally thin — every per-tenant lookup that returns a
// secret should pay through one indirection that owns the secret's
// in-memory lifetime and zero-on-eviction policy.
type PhoneHasherConfig struct {
	// PepperCacheTTL bounds how long a per-tenant pepper lives in process
	// memory. Default 5m. Pepper rotation is exceedingly rare; the TTL is a
	// defence-in-depth bound, not a correctness requirement.
	PepperCacheTTL time.Duration

	// PepperCacheSize caps the number of resident peppers. Default 1024.
	// On overflow the cache evicts the least-recently-used entry — which
	// matches the DEK cache behaviour established in Plan 04 Task 3 / 4.
	PepperCacheSize int
}

// defaults populates zero-valued fields with the documented defaults so a
// caller may pass PhoneHasherConfig{} and still get a sane configuration.
func (c *PhoneHasherConfig) defaults() {
	if c.PepperCacheTTL <= 0 {
		c.PepperCacheTTL = 5 * time.Minute
	}
	if c.PepperCacheSize <= 0 {
		c.PepperCacheSize = 1024
	}
}

// pepperFetcher is the narrow read surface PhoneHasher requires from the
// tenancy store. We accept the full api.Store at the constructor for
// ergonomics (callers already hold one) but the implementation only relies
// on this fragment, which keeps the unit tests focused.
type pepperFetcher interface {
	GetPhoneHashPepper(ctx context.Context, tenantID uuid.UUID) ([]byte, error)
}

// PhoneHasher computes deterministic HMAC-SHA256 hashes of phone numbers
// using a per-tenant pepper sourced from the tenancy store.
//
// The hasher canonicalises every phone to E.164 before hashing, so callers
// can pass the same number in any common formatting variant and get the
// same digest back — a hard requirement for the unique index on
// respondents.phone_hash and users.login_phone_hash (spec §6.4).
//
// Concurrency: the cache is guarded by a single mutex; reads (Hash) move
// the entry to MRU which is a write-side mutation, so RWMutex would not
// help. The cache is sized to a small constant; mutex contention is
// dominated by HMAC computation, not lock acquisition.
//
// Lifetime: no background goroutines. Stale entries are dropped lazily on
// the next Hash call after TTL expiry. This avoids leaking eviction
// goroutines under goleak.VerifyTestMain.
type phoneHasher struct {
	logger *zap.Logger
	store  pepperFetcher
	cfg    PhoneHasherConfig
	now    func() time.Time

	mu    sync.Mutex
	items map[uuid.UUID]*list.Element
	lru   *list.List
}

// pepperCacheItem is the LRU node payload. We keep a copy of the key inside
// the item so eviction can reach back to the items map without a second
// pass over the linked list.
type pepperCacheItem struct {
	tenantID  uuid.UUID
	pepper    []byte
	expiresAt time.Time
}

// Compile-time assertion: phoneHasher must satisfy api.PhoneHasher.
var _ api.PhoneHasher = (*phoneHasher)(nil)

// NewPhoneHasher constructs a PhoneHasher backed by the tenancy store. The
// returned value satisfies api.PhoneHasher; the concrete type is unexported
// so callers cannot reach into cache internals.
//
// store may be nil iff every caller only invokes Normalise — useful for
// pure-validation tests that have no tenant ID. The Hash path nil-checks
// store and returns an error.
func NewPhoneHasher(logger *zap.Logger, store api.Store, cfg PhoneHasherConfig) api.PhoneHasher {
	return newPhoneHasher(logger, store, cfg, time.Now)
}

// NewPhoneHasherWithClock is the test-only constructor that injects a clock
// indirection so TTL behaviour can be exercised deterministically. The
// signature matches the production constructor with one extra argument; in
// production code, callers should always use NewPhoneHasher.
func NewPhoneHasherWithClock(logger *zap.Logger, store api.Store, cfg PhoneHasherConfig, now func() time.Time) api.PhoneHasher {
	if now == nil {
		now = time.Now
	}
	return newPhoneHasher(logger, store, cfg, now)
}

// newPhoneHasher is the shared constructor body. Both public constructors
// funnel through it so the cache initialisation lives in one place.
func newPhoneHasher(logger *zap.Logger, store api.Store, cfg PhoneHasherConfig, now func() time.Time) *phoneHasher {
	cfg.defaults()
	if logger == nil {
		logger = zap.NewNop()
	}
	var fetcher pepperFetcher
	if store != nil {
		fetcher = store
	}
	return &phoneHasher{
		logger: logger.Named("phone-hasher"),
		store:  fetcher,
		cfg:    cfg,
		now:    now,
		items:  make(map[uuid.UUID]*list.Element, cfg.PepperCacheSize),
		lru:    list.New(),
	}
}

// Hash implements api.PhoneHasher.Hash.
//
// Order: canonicalise → resolve pepper (with cache) → HMAC-SHA256 over the
// canonical bytes. The pepper is never logged.
func (h *phoneHasher) Hash(ctx context.Context, tenantID uuid.UUID, phone string) ([]byte, error) {
	canon, err := h.Normalise(phone)
	if err != nil {
		return nil, err
	}
	pepper, err := h.pepperFor(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, pepper)
	// crypto/hmac's Write never errors; the documented contract says "always
	// returns nil error". We still pass the value through to keep the call
	// shape symmetric with future hash.Hash implementations that might.
	mac.Write([]byte(canon))
	return mac.Sum(nil), nil
}

// Normalise implements api.PhoneHasher.Normalise.
//
// Rules:
//   - Trim whitespace.
//   - A leading "8" with exactly 11 digits-only characters is treated as the
//     Russian-call-center shorthand and rewritten to "+7…".
//   - Otherwise the result must start with "+" followed by 8..15 digits
//     after stripping spaces, dashes, parentheses and dots — that's the
//     E.164 envelope.
//   - Any other character or shape is rejected with api.ErrInvalidArgument.
func (h *phoneHasher) Normalise(phone string) (string, error) {
	trimmed := strings.TrimSpace(phone)
	if trimmed == "" {
		return "", fmt.Errorf("%w: blank phone", api.ErrInvalidArgument)
	}
	// Russian-call-center heuristic: leading "8" → "+7" when the digit
	// envelope is exactly 11. We extract digits-only first because the input
	// may contain formatting characters such as parens/dashes.
	if rewritten, ok := russianEightShorthand(trimmed); ok {
		return rewritten, nil
	}
	out, err := stripFormattingToE164(trimmed)
	if err != nil {
		return "", err
	}
	digits := strings.TrimPrefix(out, "+")
	if n := len(digits); n < 8 || n > 15 {
		return "", fmt.Errorf("%w: bad e164 length %d", api.ErrInvalidArgument, n)
	}
	return out, nil
}

// russianEightShorthand rewrites "8…" call-centre numbers to "+7…" when the
// digits-only envelope is exactly 11 characters. Returns (rewritten, true)
// on a match, or ("", false) when the heuristic does not apply — the caller
// then falls through to the strict E.164 path.
func russianEightShorthand(s string) (string, bool) {
	if !strings.HasPrefix(s, "8") {
		return "", false
	}
	digits := digitsOnly(s)
	if len(digits) == 11 && digits[0] == '8' {
		return "+7" + digits[1:], true
	}
	return "", false
}

// stripFormattingToE164 removes spaces/dashes/parens/dots and emits the
// "+DDDD…" form. Returns api.ErrInvalidArgument when an unknown character
// appears or the leading "+" is missing.
func stripFormattingToE164(s string) (string, error) {
	var b strings.Builder
	b.Grow(len(s) + 1)
	hasPlus := false
	for i, r := range s {
		switch {
		case r == '+' && i == 0:
			b.WriteRune('+')
			hasPlus = true
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '(' || r == ')' || r == '.':
			// Strip canonical separators silently.
		default:
			return "", fmt.Errorf("%w: invalid char %q", api.ErrInvalidArgument, r)
		}
	}
	if !hasPlus {
		return "", fmt.Errorf("%w: missing leading +", api.ErrInvalidArgument)
	}
	return b.String(), nil
}

// digitsOnly returns the input with every non-digit byte removed. Used by
// Normalise's leading-"8" branch.
func digitsOnly(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// pepperFor resolves the per-tenant pepper, consulting the cache before
// falling through to the store. Cache hits move the entry to MRU; cache
// misses fetch + insert + (when over capacity) evict the LRU tail.
//
// The returned slice is the cached value — callers must NOT mutate it.
// HMAC's New copies the key into its internal block buffer, so the
// downstream Hash path never persists a reference.
func (h *phoneHasher) pepperFor(ctx context.Context, tenantID uuid.UUID) ([]byte, error) {
	if pepper, ok := h.cacheGet(tenantID); ok {
		return pepper, nil
	}
	if h.store == nil {
		return nil, fmt.Errorf("tenancy/phone-hasher: store is nil; cannot resolve pepper")
	}
	pepper, err := h.store.GetPhoneHashPepper(ctx, tenantID)
	if err != nil {
		// Preserve the sentinel chain (api.ErrNotFound, etc.) so callers
		// can errors.Is without unwrapping a custom layer.
		if errors.Is(err, api.ErrNotFound) {
			return nil, fmt.Errorf("tenancy/phone-hasher: pepper for tenant %s: %w", tenantID, err)
		}
		return nil, fmt.Errorf("tenancy/phone-hasher: get pepper: %w", err)
	}
	if len(pepper) < 32 {
		return nil, fmt.Errorf("%w: pepper length %d (want >= 32)", api.ErrInvalidArgument, len(pepper))
	}
	// Defensive copy: the store may share its internal buffer; we must own
	// the bytes we cache so a future store mutation cannot corrupt us.
	cp := make([]byte, len(pepper))
	copy(cp, pepper)
	h.cachePut(tenantID, cp)
	return cp, nil
}

// cacheGet returns the cached pepper for tenantID, or (nil, false) if
// absent or expired. A hit promotes the entry to MRU; a stale entry is
// removed (lazy expiry) so subsequent Gets see a clean miss.
func (h *phoneHasher) cacheGet(tenantID uuid.UUID) ([]byte, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	el, ok := h.items[tenantID]
	if !ok {
		return nil, false
	}
	it, _ := el.Value.(*pepperCacheItem)
	if h.expired(it) {
		h.removeElementLocked(el)
		return nil, false
	}
	h.lru.MoveToFront(el)
	return it.pepper, true
}

// cachePut inserts (or refreshes) the pepper for tenantID. Evicts the LRU
// tail when capacity is reached.
func (h *phoneHasher) cachePut(tenantID uuid.UUID, pepper []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if el, ok := h.items[tenantID]; ok {
		it, _ := el.Value.(*pepperCacheItem)
		it.pepper = pepper
		it.expiresAt = h.now().Add(h.cfg.PepperCacheTTL)
		h.lru.MoveToFront(el)
		return
	}
	for h.lru.Len() >= h.cfg.PepperCacheSize {
		if !h.evictOldestLocked() {
			break
		}
	}
	it := &pepperCacheItem{
		tenantID:  tenantID,
		pepper:    pepper,
		expiresAt: h.now().Add(h.cfg.PepperCacheTTL),
	}
	el := h.lru.PushFront(it)
	h.items[tenantID] = el
}

// expired reports whether it is past its TTL deadline. Caller must hold mu.
func (h *phoneHasher) expired(it *pepperCacheItem) bool {
	return h.now().After(it.expiresAt)
}

// evictOldestLocked drops the LRU tail. Returns false when the list is
// empty (defence against an inconsistent cfg.PepperCacheSize). Caller must
// hold mu.
func (h *phoneHasher) evictOldestLocked() bool {
	tail := h.lru.Back()
	if tail == nil {
		return false
	}
	h.removeElementLocked(tail)
	return true
}

// removeElementLocked deletes one entry and zeroises the pepper slice the
// hasher held. Best-effort: the runtime may have copied the bytes earlier.
// Caller must hold mu.
func (h *phoneHasher) removeElementLocked(el *list.Element) {
	it, _ := el.Value.(*pepperCacheItem)
	if it != nil {
		for i := range it.pepper {
			it.pepper[i] = 0
		}
		delete(h.items, it.tenantID)
	}
	h.lru.Remove(el)
}
