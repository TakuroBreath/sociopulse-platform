package observability

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func newCapture(t *testing.T, patterns []string) (*zap.Logger, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	encCfg := zap.NewProductionEncoderConfig()
	encCfg.TimeKey = "" // strip timestamp for stable assertions
	enc, err := NewRedactingEncoder(zapcore.NewJSONEncoder(encCfg), patterns)
	require.NoError(t, err)
	core := zapcore.NewCore(enc, zapcore.AddSync(buf), zapcore.DebugLevel)
	return zap.New(core), buf
}

func TestRedactingEncoderMasksPhoneNumber(t *testing.T) {
	t.Parallel()
	log, buf := newCapture(t, []string{`\+?7\d{10}`})
	log.Info("call placed", zap.String("number", "+79161234567"))
	got := buf.String()
	assert.NotContains(t, got, "+79161234567")
	assert.Contains(t, got, "[REDACTED]")
}

func TestRedactingEncoderMasksTokenInString(t *testing.T) {
	t.Parallel()
	log, buf := newCapture(t, []string{`token:[A-Za-z0-9._-]+`})
	log.Info("auth", zap.String("dump", "Authorization: token:eyJhbGciOiJIUzI1NiJ9.payload.sig"))
	got := buf.String()
	assert.NotContains(t, got, "eyJhbGciOiJIUzI1NiJ9")
	assert.Contains(t, got, "[REDACTED]")
}

func TestRedactingEncoderMasksPassword(t *testing.T) {
	t.Parallel()
	log, buf := newCapture(t, []string{`password:\S+`})
	log.Info("login", zap.String("creds", "user=alice password:hunter2"))
	got := buf.String()
	assert.NotContains(t, got, "hunter2")
	assert.Contains(t, got, "[REDACTED]")
}

func TestRedactingEncoderMasksJWT(t *testing.T) {
	t.Parallel()
	// A simple JWT pattern: three base64url-encoded segments separated by dots.
	log, buf := newCapture(t, []string{`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`})
	log.Info("token recv", zap.String("authorization", "Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.signaturepart"))
	got := buf.String()
	assert.NotContains(t, got, "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.signaturepart")
	assert.Contains(t, got, "[REDACTED]")
}

func TestRedactingEncoderLeavesPlainTextAlone(t *testing.T) {
	t.Parallel()
	log, buf := newCapture(t, []string{`\+?7\d{10}`})
	log.Info("benign", zap.String("hello", "world"))
	assert.Contains(t, buf.String(), "world")
}

func TestNewRedactingEncoderRejectsBadPattern(t *testing.T) {
	t.Parallel()
	encCfg := zap.NewProductionEncoderConfig()
	_, err := NewRedactingEncoder(zapcore.NewJSONEncoder(encCfg), []string{`(unclosed`})
	require.Error(t, err)
}

func TestRedactingEncoderClonePreservesPatterns(t *testing.T) {
	t.Parallel()
	encCfg := zap.NewProductionEncoderConfig()
	encCfg.TimeKey = ""
	enc, err := NewRedactingEncoder(zapcore.NewJSONEncoder(encCfg), []string{`secret`})
	require.NoError(t, err)
	clone := enc.Clone()
	require.NotNil(t, clone)

	// Use the clone to encode an entry and ensure redaction still works.
	buf := &bytes.Buffer{}
	core := zapcore.NewCore(clone, zapcore.AddSync(buf), zapcore.DebugLevel)
	log := zap.New(core)
	log.Info("msg", zap.String("k", "secret"))
	assert.NotContains(t, buf.String(), "secret")
	assert.Contains(t, buf.String(), "[REDACTED]")
}
