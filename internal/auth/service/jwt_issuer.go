package service

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	authapi "github.com/sociopulse/platform/internal/auth/api"
)

// Token type discriminators carried in the `typ` claim. They are the
// only values Validate accepts via expectedType. Cross-typ misuse
// (passing an access token where refresh was expected, etc.) maps to
// api.ErrTokenInvalid.
const (
	tokenTypeAccess  = "access"
	tokenTypeRefresh = "refresh"
)

// minSecretBytes is the lower bound on the HS256 signing-secret length.
// HS256 requires a 256-bit key; we round down to 16 bytes (128 bits) as
// an absolute floor to flag obviously-misconfigured secrets, but
// production secrets must be 32+ bytes (sourced from Lockbox/KMS).
const minSecretBytes = 16

// jtiByteLen is the entropy of a generated JTI / SessionID before hex
// encoding. 16 bytes -> 32 hex characters, well above birthday-collision
// concerns for the populations we care about.
const jtiByteLen = 16

// JWTConfig captures every knob NewJWTIssuer needs. The Issuer string is
// stamped into the `iss` claim and verified on the way back. Secret is
// the HS256 signing secret (sourced from Lockbox via env in prod;
// rotated by re-instantiating the issuer behind an atomic pointer).
// AccessTTL / RefreshTTL are the lifetimes of access vs refresh tokens
// (15 min / 30 d in prod). Leeway is the clock-skew tolerance applied
// during validation; 30s matches industry practice (jwt.io,
// Auth0, AWS Cognito).
type JWTConfig struct {
	Issuer     string
	Secret     []byte
	AccessTTL  time.Duration
	RefreshTTL time.Duration
	Leeway     time.Duration
}

// JWTIssuer mints HS256 JWTs and validates them back. It is the single
// implementation of api.JWTIssuer; the type is intentionally concrete
// in service/ so other packages depend on the interface in api/.
//
// The struct is safe for concurrent use; the underlying jwt.Parser is
// stateless and the secret is read-only after construction.
type JWTIssuer struct {
	cfg JWTConfig
	now func() time.Time
}

// Compile-time guarantee the implementation satisfies the public contract.
var _ authapi.JWTIssuer = (*JWTIssuer)(nil)

// NewJWTIssuer validates the config and returns a ready-to-use issuer.
// A nil clock defaults to time.Now, which is what production wants;
// tests inject a fake clock to exercise expiry and skew paths.
func NewJWTIssuer(cfg JWTConfig, clock func() time.Time) (*JWTIssuer, error) {
	if cfg.Issuer == "" {
		return nil, errors.New("jwt: Issuer must be non-empty")
	}
	if len(cfg.Secret) < minSecretBytes {
		return nil, fmt.Errorf("jwt: Secret must be at least %d bytes, got %d", minSecretBytes, len(cfg.Secret))
	}
	if cfg.AccessTTL <= 0 {
		return nil, errors.New("jwt: AccessTTL must be positive")
	}
	if cfg.RefreshTTL <= 0 {
		return nil, errors.New("jwt: RefreshTTL must be positive")
	}
	if cfg.Leeway < 0 {
		return nil, errors.New("jwt: Leeway must be >= 0")
	}
	if clock == nil {
		clock = time.Now
	}
	return &JWTIssuer{cfg: cfg, now: clock}, nil
}

// IssueAccess produces a signed access token whose lifetime is
// cfg.AccessTTL. Empty Claims.JTI / Claims.SessionID are filled with
// crypto/rand-derived hex strings; uuid.Nil UserID/TenantID are rejected
// as invalid input (wraps api.ErrTokenInvalid).
func (j *JWTIssuer) IssueAccess(c authapi.Claims) (string, time.Time, error) {
	return j.sign(c, tokenTypeAccess, j.cfg.AccessTTL)
}

// IssueRefresh produces a signed refresh token whose lifetime is
// cfg.RefreshTTL. Caller is expected to plumb the same Claims.SessionID
// across access+refresh issuance for the same login; the issuer just
// stamps it.
func (j *JWTIssuer) IssueRefresh(c authapi.Claims) (string, time.Time, error) {
	return j.sign(c, tokenTypeRefresh, j.cfg.RefreshTTL)
}

func (j *JWTIssuer) sign(c authapi.Claims, typ string, ttl time.Duration) (string, time.Time, error) {
	if c.UserID == uuid.Nil {
		return "", time.Time{}, fmt.Errorf("%w: UserID is uuid.Nil", authapi.ErrTokenInvalid)
	}
	if c.TenantID == uuid.Nil {
		return "", time.Time{}, fmt.Errorf("%w: TenantID is uuid.Nil", authapi.ErrTokenInvalid)
	}

	if c.JTI == "" {
		jti, err := randomHex(jtiByteLen)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("jwt: generate jti: %w", err)
		}
		c.JTI = jti
	}
	if c.SessionID == "" {
		sid, err := randomHex(jtiByteLen)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("jwt: generate sid: %w", err)
		}
		c.SessionID = sid
	}

	now := j.now()
	exp := now.Add(ttl)

	rolesStr := make([]string, len(c.Roles))
	for i, r := range c.Roles {
		rolesStr[i] = string(r)
	}

	mc := jwt.MapClaims{
		"iss":   j.cfg.Issuer,
		"sub":   c.UserID.String(),
		"tid":   c.TenantID.String(),
		"login": c.Login,
		"roles": rolesStr,
		"sid":   c.SessionID,
		"jti":   c.JTI,
		"iat":   now.Unix(),
		"exp":   exp.Unix(),
		"typ":   typ,
	}
	if c.TOTPDone {
		mc["totp_done"] = true
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, mc)
	signed, err := tok.SignedString(j.cfg.Secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("jwt: sign: %w", err)
	}
	return signed, exp, nil
}

// Validate parses a token string, verifies its HS256 signature against
// cfg.Secret, checks `iss`, `exp`, `iat`, and that `typ` matches
// expectedType ("access" or "refresh"). Any failure (malformed, bad
// signature, expired, wrong issuer, wrong type, zero subject/tenant,
// alg=none) is wrapped with api.ErrTokenInvalid so callers can use a
// single errors.Is check.
//
// The parser is configured with WithValidMethods to bind the algorithm
// (defeats alg=none and HS/RS confusion) and WithExpirationRequired to
// force every token to declare its lifetime explicitly.
func (j *JWTIssuer) Validate(token, expectedType string) (authapi.Claims, error) {
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithLeeway(j.cfg.Leeway),
		jwt.WithIssuer(j.cfg.Issuer),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithTimeFunc(j.now),
	)

	parsed, err := parser.Parse(token, func(_ *jwt.Token) (any, error) {
		return j.cfg.Secret, nil
	})
	if err != nil {
		return authapi.Claims{}, errors.Join(authapi.ErrTokenInvalid, err)
	}

	mc, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return authapi.Claims{}, fmt.Errorf("%w: unexpected claims type %T", authapi.ErrTokenInvalid, parsed.Claims)
	}

	typ, _ := mc["typ"].(string)
	if typ != expectedType {
		return authapi.Claims{}, fmt.Errorf("%w: typ=%q expected %q", authapi.ErrTokenInvalid, typ, expectedType)
	}

	out, err := mapClaimsTo(mc)
	if err != nil {
		return authapi.Claims{}, errors.Join(authapi.ErrTokenInvalid, err)
	}
	return out, nil
}

// mapClaimsTo decodes the validated MapClaims into the typed
// api.Claims, including UUID parsing and a non-zero check on subject
// and tenant. Errors here are returned bare; the caller wraps with
// ErrTokenInvalid.
func mapClaimsTo(mc jwt.MapClaims) (authapi.Claims, error) {
	subStr, _ := mc["sub"].(string)
	if subStr == "" {
		return authapi.Claims{}, errors.New("missing sub")
	}
	uid, err := uuid.Parse(subStr)
	if err != nil {
		return authapi.Claims{}, fmt.Errorf("parse sub: %w", err)
	}
	if uid == uuid.Nil {
		return authapi.Claims{}, errors.New("sub is uuid.Nil")
	}

	tidStr, _ := mc["tid"].(string)
	if tidStr == "" {
		return authapi.Claims{}, errors.New("missing tid")
	}
	tid, err := uuid.Parse(tidStr)
	if err != nil {
		return authapi.Claims{}, fmt.Errorf("parse tid: %w", err)
	}
	if tid == uuid.Nil {
		return authapi.Claims{}, errors.New("tid is uuid.Nil")
	}

	login, _ := mc["login"].(string)

	var roles []authapi.Role
	if rawRoles, ok := mc["roles"].([]any); ok {
		roles = make([]authapi.Role, 0, len(rawRoles))
		for _, raw := range rawRoles {
			s, ok := raw.(string)
			if !ok {
				return authapi.Claims{}, fmt.Errorf("role entry has type %T", raw)
			}
			roles = append(roles, authapi.Role(s))
		}
	}

	sid, _ := mc["sid"].(string)
	jti, _ := mc["jti"].(string)
	totpDone, _ := mc["totp_done"].(bool)

	iat, err := numericTime(mc, "iat")
	if err != nil {
		return authapi.Claims{}, fmt.Errorf("parse iat: %w", err)
	}
	exp, err := numericTime(mc, "exp")
	if err != nil {
		return authapi.Claims{}, fmt.Errorf("parse exp: %w", err)
	}

	return authapi.Claims{
		UserID:    uid,
		TenantID:  tid,
		Login:     login,
		Roles:     roles,
		SessionID: sid,
		JTI:       jti,
		IssuedAt:  iat,
		ExpiresAt: exp,
		TOTPDone:  totpDone,
	}, nil
}

// numericTime decodes a JWT NumericDate claim (RFC 7519 §2). The
// json.Unmarshal path inside jwt-go always hands us a float64 for JSON
// numbers; non-numeric values fall through to the default error branch.
func numericTime(mc jwt.MapClaims, key string) (time.Time, error) {
	v, ok := mc[key]
	if !ok || v == nil {
		return time.Time{}, fmt.Errorf("%s missing", key)
	}
	f, ok := v.(float64)
	if !ok {
		return time.Time{}, fmt.Errorf("%s has type %T", key, v)
	}
	return time.Unix(int64(f), 0).UTC(), nil
}

// randomHex returns 2*n hex characters of crypto/rand entropy.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return hex.EncodeToString(b), nil
}
