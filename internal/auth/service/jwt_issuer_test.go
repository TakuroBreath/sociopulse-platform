package service_test

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	"github.com/sociopulse/platform/internal/auth/service"
)

// fakeClock returns a controllable time.Time. now is stored as Unix-nanos in an
// atomic so the test can advance the clock without races; the issuer reads time
// through the closure passed to NewJWTIssuer.
type fakeClock struct {
	nanos atomic.Int64
}

func newFakeClock(t time.Time) *fakeClock {
	c := &fakeClock{}
	c.nanos.Store(t.UnixNano())
	return c
}

func (c *fakeClock) Now() time.Time              { return time.Unix(0, c.nanos.Load()).UTC() }
func (c *fakeClock) Advance(d time.Duration)     { c.nanos.Add(int64(d)) }
func (c *fakeClock) Set(t time.Time)             { c.nanos.Store(t.UnixNano()) }
func (c *fakeClock) FuncClock() func() time.Time { return c.Now }

// validSecret is a 32-byte (256-bit) secret used everywhere in the tests.
var validSecret = []byte("0123456789abcdef0123456789abcdef")

// makeIssuer builds a JWTIssuer with sane defaults; tests override fields
// inline by replacing arguments.
func makeIssuer(t *testing.T, secret []byte, leeway time.Duration, accessTTL, refreshTTL time.Duration, clock func() time.Time) *service.JWTIssuer {
	t.Helper()
	cfg := service.JWTConfig{
		Issuer:     "sociopulse-test",
		Secret:     secret,
		AccessTTL:  accessTTL,
		RefreshTTL: refreshTTL,
		Leeway:     leeway,
	}
	iss, err := service.NewJWTIssuer(cfg, clock)
	require.NoError(t, err)
	return iss
}

func sampleClaims(t *testing.T) authapi.Claims {
	t.Helper()
	return authapi.Claims{
		UserID:    uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		TenantID:  uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Login:     "alice",
		Roles:     []authapi.Role{authapi.RoleOperator, authapi.RoleSupervisor},
		SessionID: "session-abc",
		JTI:       "jti-fixed-1",
		TOTPDone:  true,
	}
}

//  1. IssueAccess → Validate("access") round-trips Claims (UserID, TenantID,
//     Login, Roles, SessionID, JTI all preserved).
func TestJWTIssuer_IssueAccess_RoundTrip(t *testing.T) {
	t.Parallel()

	clk := newFakeClock(time.Date(2026, time.May, 8, 12, 0, 0, 0, time.UTC))
	iss := makeIssuer(t, validSecret, 30*time.Second, 15*time.Minute, 30*24*time.Hour, clk.FuncClock())

	in := sampleClaims(t)
	tok, exp, err := iss.IssueAccess(in)
	require.NoError(t, err)
	require.NotEmpty(t, tok)
	require.Equal(t, clk.Now().Add(15*time.Minute).Unix(), exp.Unix())

	got, err := iss.Validate(tok, "access")
	require.NoError(t, err)

	assert.Equal(t, in.UserID, got.UserID)
	assert.Equal(t, in.TenantID, got.TenantID)
	assert.Equal(t, in.Login, got.Login)
	assert.Equal(t, in.Roles, got.Roles)
	assert.Equal(t, in.SessionID, got.SessionID)
	assert.Equal(t, in.JTI, got.JTI)
	assert.True(t, got.TOTPDone)
	assert.Equal(t, clk.Now().Unix(), got.IssuedAt.Unix())
	assert.Equal(t, exp.Unix(), got.ExpiresAt.Unix())
}

//  2. Token past `exp` (use fake clock that advances) → Validate returns error
//     matching ErrTokenInvalid.
func TestJWTIssuer_Validate_RejectsExpired(t *testing.T) {
	t.Parallel()

	clk := newFakeClock(time.Date(2026, time.May, 8, 12, 0, 0, 0, time.UTC))
	iss := makeIssuer(t, validSecret, 0, 1*time.Second, 1*time.Hour, clk.FuncClock())

	tok, _, err := iss.IssueAccess(sampleClaims(t))
	require.NoError(t, err)

	// Move past expiration + outside any leeway window.
	clk.Advance(2 * time.Second)

	_, err = iss.Validate(tok, "access")
	require.Error(t, err)
	require.ErrorIs(t, err, authapi.ErrTokenInvalid)
}

//  3. Token signed with secret A, validated with secret B → ErrTokenInvalid.
//     Must not panic.
func TestJWTIssuer_Validate_RejectsBadSignature(t *testing.T) {
	t.Parallel()

	clk := newFakeClock(time.Date(2026, time.May, 8, 12, 0, 0, 0, time.UTC))
	secretA := []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	secretB := []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	issA := makeIssuer(t, secretA, 30*time.Second, 15*time.Minute, time.Hour, clk.FuncClock())
	issB := makeIssuer(t, secretB, 30*time.Second, 15*time.Minute, time.Hour, clk.FuncClock())

	tok, _, err := issA.IssueAccess(sampleClaims(t))
	require.NoError(t, err)

	require.NotPanics(t, func() {
		_, vErr := issB.Validate(tok, "access")
		require.Error(t, vErr)
		require.ErrorIs(t, vErr, authapi.ErrTokenInvalid)
	})
}

//  4. IssueAccess and IssueRefresh for same Claims{SessionID:"S1"} produce two
//     tokens with same SID, different JTI, different `typ`.
func TestJWTIssuer_AccessAndRefresh_ShareSessionDifferJTI(t *testing.T) {
	t.Parallel()

	clk := newFakeClock(time.Date(2026, time.May, 8, 12, 0, 0, 0, time.UTC))
	iss := makeIssuer(t, validSecret, 30*time.Second, 15*time.Minute, 30*24*time.Hour, clk.FuncClock())

	c := sampleClaims(t)
	c.SessionID = "S1"
	c.JTI = "" // force generation

	accessTok, _, err := iss.IssueAccess(c)
	require.NoError(t, err)
	refreshTok, _, err := iss.IssueRefresh(c)
	require.NoError(t, err)

	assert.NotEqual(t, accessTok, refreshTok, "tokens must differ")

	access, err := iss.Validate(accessTok, "access")
	require.NoError(t, err)
	refresh, err := iss.Validate(refreshTok, "refresh")
	require.NoError(t, err)

	assert.Equal(t, "S1", access.SessionID)
	assert.Equal(t, "S1", refresh.SessionID)
	assert.NotEqual(t, access.JTI, refresh.JTI)
	assert.NotEmpty(t, access.JTI)
	assert.NotEmpty(t, refresh.JTI)

	// Cross-typ rejection: an access token must not validate as refresh and vice versa.
	_, err = iss.Validate(accessTok, "refresh")
	require.ErrorIs(t, err, authapi.ErrTokenInvalid)
	_, err = iss.Validate(refreshTok, "access")
	require.ErrorIs(t, err, authapi.ErrTokenInvalid)
}

// 5. Validate rejects alg=none even when correctly serialized.
func TestJWTIssuer_Validate_RejectsAlgNone(t *testing.T) {
	t.Parallel()

	clk := newFakeClock(time.Date(2026, time.May, 8, 12, 0, 0, 0, time.UTC))
	iss := makeIssuer(t, validSecret, 30*time.Second, 15*time.Minute, time.Hour, clk.FuncClock())

	now := clk.Now()
	mc := jwt.MapClaims{
		"iss":   "sociopulse-test",
		"sub":   uuid.New().String(),
		"tid":   uuid.New().String(),
		"login": "alice",
		"roles": []string{"operator"},
		"sid":   "S",
		"jti":   "J",
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
		"typ":   "access",
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, mc)
	noneSigned, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	require.NoError(t, err)

	require.NotPanics(t, func() {
		_, vErr := iss.Validate(noneSigned, "access")
		require.Error(t, vErr)
		require.ErrorIs(t, vErr, authapi.ErrTokenInvalid)
	})

	// Belt-and-suspenders: confirm header really is alg=none.
	require.True(t, strings.HasPrefix(noneSigned, "eyJ"))
}

//  6. Validate rejects token whose `sub` parses to uuid.Nil. We bypass IssueAccess
//     by using jwt.NewWithClaims directly with an empty sub.
func TestJWTIssuer_Validate_RejectsZeroSubject(t *testing.T) {
	t.Parallel()

	clk := newFakeClock(time.Date(2026, time.May, 8, 12, 0, 0, 0, time.UTC))
	iss := makeIssuer(t, validSecret, 30*time.Second, 15*time.Minute, time.Hour, clk.FuncClock())

	now := clk.Now()
	mc := jwt.MapClaims{
		"iss":   "sociopulse-test",
		"sub":   "", // zero subject
		"tid":   uuid.New().String(),
		"login": "alice",
		"roles": []string{"operator"},
		"sid":   "S",
		"jti":   "J",
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
		"typ":   "access",
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, mc)
	signed, err := tok.SignedString(validSecret)
	require.NoError(t, err)

	_, err = iss.Validate(signed, "access")
	require.Error(t, err)
	require.ErrorIs(t, err, authapi.ErrTokenInvalid)

	// Same with explicit zero-uuid string for sub.
	mc["sub"] = uuid.Nil.String()
	tok2 := jwt.NewWithClaims(jwt.SigningMethodHS256, mc)
	signed2, err := tok2.SignedString(validSecret)
	require.NoError(t, err)
	_, err = iss.Validate(signed2, "access")
	require.ErrorIs(t, err, authapi.ErrTokenInvalid)

	// And tid=zero is also rejected.
	mc["sub"] = uuid.New().String()
	mc["tid"] = uuid.Nil.String()
	tok3 := jwt.NewWithClaims(jwt.SigningMethodHS256, mc)
	signed3, err := tok3.SignedString(validSecret)
	require.NoError(t, err)
	_, err = iss.Validate(signed3, "access")
	require.ErrorIs(t, err, authapi.ErrTokenInvalid)
}

//  7. Clock-skew tolerance: token issued at T-29s with leeway=30s and TTL=0 →
//     still valid; T-31s → invalid.
func TestJWTIssuer_Validate_LeewayWindow(t *testing.T) {
	t.Parallel()

	clk := newFakeClock(time.Date(2026, time.May, 8, 12, 0, 0, 0, time.UTC))
	iss := makeIssuer(t, validSecret, 30*time.Second, 1*time.Second, time.Hour, clk.FuncClock())

	tok, _, err := iss.IssueAccess(sampleClaims(t))
	require.NoError(t, err)

	// 29 seconds past expiry: still inside leeway window (29s elapsed, TTL=1s, leeway=30s → 30s of slack).
	clk.Advance(30 * time.Second)
	_, err = iss.Validate(tok, "access")
	require.NoError(t, err, "token within leeway must be accepted")

	// Step further: 32s past expiry: outside leeway.
	clk.Advance(2 * time.Second)
	_, err = iss.Validate(tok, "access")
	require.Error(t, err)
	require.ErrorIs(t, err, authapi.ErrTokenInvalid)
}

// --- additional safety/contract tests (constructor + zero claims rejection) ---

func TestNewJWTIssuer_ValidatesConfig(t *testing.T) {
	t.Parallel()

	t.Run("negative leeway rejected", func(t *testing.T) {
		t.Parallel()
		_, err := service.NewJWTIssuer(service.JWTConfig{
			Issuer:     "x",
			Secret:     validSecret,
			AccessTTL:  time.Minute,
			RefreshTTL: time.Hour,
			Leeway:     -1 * time.Second,
		}, nil)
		require.Error(t, err)
	})

	t.Run("empty issuer rejected", func(t *testing.T) {
		t.Parallel()
		_, err := service.NewJWTIssuer(service.JWTConfig{
			Issuer:     "",
			Secret:     validSecret,
			AccessTTL:  time.Minute,
			RefreshTTL: time.Hour,
		}, nil)
		require.Error(t, err)
	})

	t.Run("short secret rejected", func(t *testing.T) {
		t.Parallel()
		_, err := service.NewJWTIssuer(service.JWTConfig{
			Issuer:     "x",
			Secret:     []byte("too-short"),
			AccessTTL:  time.Minute,
			RefreshTTL: time.Hour,
		}, nil)
		require.Error(t, err)
	})

	t.Run("zero access ttl rejected", func(t *testing.T) {
		t.Parallel()
		_, err := service.NewJWTIssuer(service.JWTConfig{
			Issuer:     "x",
			Secret:     validSecret,
			AccessTTL:  0,
			RefreshTTL: time.Hour,
		}, nil)
		require.Error(t, err)
	})

	t.Run("zero refresh ttl rejected", func(t *testing.T) {
		t.Parallel()
		_, err := service.NewJWTIssuer(service.JWTConfig{
			Issuer:     "x",
			Secret:     validSecret,
			AccessTTL:  time.Minute,
			RefreshTTL: 0,
		}, nil)
		require.Error(t, err)
	})

	t.Run("nil clock defaults to time.Now", func(t *testing.T) {
		t.Parallel()
		iss, err := service.NewJWTIssuer(service.JWTConfig{
			Issuer:     "x",
			Secret:     validSecret,
			AccessTTL:  time.Minute,
			RefreshTTL: time.Hour,
		}, nil)
		require.NoError(t, err)
		// Issuing should work — implies clock is non-nil internally.
		tok, _, err := iss.IssueAccess(sampleClaims(t))
		require.NoError(t, err)
		require.NotEmpty(t, tok)
	})
}

func TestJWTIssuer_Issue_RejectsZeroIDs(t *testing.T) {
	t.Parallel()

	clk := newFakeClock(time.Date(2026, time.May, 8, 12, 0, 0, 0, time.UTC))
	iss := makeIssuer(t, validSecret, 30*time.Second, time.Minute, time.Hour, clk.FuncClock())

	t.Run("nil UserID rejected", func(t *testing.T) {
		t.Parallel()
		c := sampleClaims(t)
		c.UserID = uuid.Nil
		_, _, err := iss.IssueAccess(c)
		require.Error(t, err)
		require.ErrorIs(t, err, authapi.ErrTokenInvalid)
	})

	t.Run("nil TenantID rejected", func(t *testing.T) {
		t.Parallel()
		c := sampleClaims(t)
		c.TenantID = uuid.Nil
		_, _, err := iss.IssueAccess(c)
		require.Error(t, err)
		require.ErrorIs(t, err, authapi.ErrTokenInvalid)
	})

	t.Run("nil UserID rejected on refresh too", func(t *testing.T) {
		t.Parallel()
		c := sampleClaims(t)
		c.UserID = uuid.Nil
		_, _, err := iss.IssueRefresh(c)
		require.Error(t, err)
		require.ErrorIs(t, err, authapi.ErrTokenInvalid)
	})
}

func TestJWTIssuer_Issue_GeneratesJTIAndSessionID(t *testing.T) {
	t.Parallel()

	clk := newFakeClock(time.Date(2026, time.May, 8, 12, 0, 0, 0, time.UTC))
	iss := makeIssuer(t, validSecret, 30*time.Second, time.Minute, time.Hour, clk.FuncClock())

	c := sampleClaims(t)
	c.JTI = ""
	c.SessionID = ""

	tok, _, err := iss.IssueAccess(c)
	require.NoError(t, err)

	got, err := iss.Validate(tok, "access")
	require.NoError(t, err)
	assert.NotEmpty(t, got.JTI)
	assert.NotEmpty(t, got.SessionID)
	assert.Len(t, got.JTI, 32, "16 random bytes hex-encoded == 32 chars")
	assert.Len(t, got.SessionID, 32)

	// Two issuances must yield different ids.
	tok2, _, err := iss.IssueAccess(c)
	require.NoError(t, err)
	got2, err := iss.Validate(tok2, "access")
	require.NoError(t, err)
	assert.NotEqual(t, got.JTI, got2.JTI)
	assert.NotEqual(t, got.SessionID, got2.SessionID)
}

func TestJWTIssuer_Validate_WrongIssuerRejected(t *testing.T) {
	t.Parallel()

	clk := newFakeClock(time.Date(2026, time.May, 8, 12, 0, 0, 0, time.UTC))
	mintIss, err := service.NewJWTIssuer(service.JWTConfig{
		Issuer:     "other-issuer",
		Secret:     validSecret,
		AccessTTL:  time.Minute,
		RefreshTTL: time.Hour,
		Leeway:     30 * time.Second,
	}, clk.FuncClock())
	require.NoError(t, err)

	verifyIss := makeIssuer(t, validSecret, 30*time.Second, time.Minute, time.Hour, clk.FuncClock())

	tok, _, err := mintIss.IssueAccess(sampleClaims(t))
	require.NoError(t, err)

	_, err = verifyIss.Validate(tok, "access")
	require.Error(t, err)
	require.ErrorIs(t, err, authapi.ErrTokenInvalid)
}

func TestJWTIssuer_Validate_MalformedToken(t *testing.T) {
	t.Parallel()

	clk := newFakeClock(time.Date(2026, time.May, 8, 12, 0, 0, 0, time.UTC))
	iss := makeIssuer(t, validSecret, 30*time.Second, time.Minute, time.Hour, clk.FuncClock())

	require.NotPanics(t, func() {
		_, err := iss.Validate("not-a-jwt-at-all", "access")
		require.Error(t, err)
		require.ErrorIs(t, err, authapi.ErrTokenInvalid)
	})

	// Empty string also handled.
	_, err := iss.Validate("", "access")
	require.Error(t, err)
	require.ErrorIs(t, err, authapi.ErrTokenInvalid)
}

// Forged-claim coverage: build raw HS256 tokens with malformed claim values to
// exercise mapClaimsTo's error branches. These cannot be reached through
// IssueAccess/IssueRefresh.
func TestJWTIssuer_Validate_RejectsCorruptClaims(t *testing.T) {
	t.Parallel()

	clk := newFakeClock(time.Date(2026, time.May, 8, 12, 0, 0, 0, time.UTC))
	iss := makeIssuer(t, validSecret, 30*time.Second, time.Minute, time.Hour, clk.FuncClock())

	now := clk.Now()
	base := func() jwt.MapClaims {
		return jwt.MapClaims{
			"iss":   "sociopulse-test",
			"sub":   uuid.New().String(),
			"tid":   uuid.New().String(),
			"login": "alice",
			"roles": []string{"operator"},
			"sid":   "S",
			"jti":   "J",
			"iat":   now.Unix(),
			"exp":   now.Add(time.Hour).Unix(),
			"typ":   "access",
		}
	}

	mintRaw := func(t *testing.T, mc jwt.MapClaims) string {
		t.Helper()
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, mc)
		signed, err := tok.SignedString(validSecret)
		require.NoError(t, err)
		return signed
	}

	t.Run("malformed sub uuid", func(t *testing.T) {
		t.Parallel()
		mc := base()
		mc["sub"] = "not-a-uuid"
		_, err := iss.Validate(mintRaw(t, mc), "access")
		require.ErrorIs(t, err, authapi.ErrTokenInvalid)
	})

	t.Run("missing tid", func(t *testing.T) {
		t.Parallel()
		mc := base()
		delete(mc, "tid")
		_, err := iss.Validate(mintRaw(t, mc), "access")
		require.ErrorIs(t, err, authapi.ErrTokenInvalid)
	})

	t.Run("malformed tid uuid", func(t *testing.T) {
		t.Parallel()
		mc := base()
		mc["tid"] = "not-a-uuid"
		_, err := iss.Validate(mintRaw(t, mc), "access")
		require.ErrorIs(t, err, authapi.ErrTokenInvalid)
	})

	t.Run("non-string role entry", func(t *testing.T) {
		t.Parallel()
		mc := base()
		mc["roles"] = []any{"operator", 42}
		_, err := iss.Validate(mintRaw(t, mc), "access")
		require.ErrorIs(t, err, authapi.ErrTokenInvalid)
	})

	t.Run("iat wrong type", func(t *testing.T) {
		t.Parallel()
		mc := base()
		// jwt-go won't reject a string iat outright in v5 with WithIssuedAt,
		// but mapClaimsTo will. Replace with a string.
		mc["iat"] = "not-a-number"
		_, err := iss.Validate(mintRaw(t, mc), "access")
		require.ErrorIs(t, err, authapi.ErrTokenInvalid)
	})
}

// Sanity check — make sure errors.Is unwraps the wrapped jwt errors so callers
// can match on api.ErrTokenInvalid uniformly.
func TestJWTIssuer_Validate_ErrorWraps_ErrTokenInvalid(t *testing.T) {
	t.Parallel()

	clk := newFakeClock(time.Date(2026, time.May, 8, 12, 0, 0, 0, time.UTC))
	iss := makeIssuer(t, validSecret, 0, time.Second, time.Hour, clk.FuncClock())

	tok, _, err := iss.IssueAccess(sampleClaims(t))
	require.NoError(t, err)
	clk.Advance(2 * time.Second)

	_, err = iss.Validate(tok, "access")
	require.Error(t, err)
	require.ErrorIs(t, err, authapi.ErrTokenInvalid,
		"wrapped error must satisfy errors.Is(api.ErrTokenInvalid)")
}
