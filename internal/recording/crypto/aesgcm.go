package crypto

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/sociopulse/platform/pkg/encryption"
)

// MaxAudioPlaintextBytes caps the plaintext size of a single decrypt call.
// 200 MiB matches the Plan 12 design brief's §Outputs trade-off — at higher
// sizes we'd OOM under concurrent audits. v2 chunked-envelope format
// (deferred) lifts this cap.
const MaxAudioPlaintextBytes = 200 * 1024 * 1024

var (
	// ErrAudioTooLargeForV1 is returned by Decrypt when ciphertext.Size
	// exceeds MaxAudioPlaintextBytes. Plan 12.3 maps this to HTTP 413.
	ErrAudioTooLargeForV1 = errors.New("recording.crypto: audio exceeds v1 in-memory cap")

	// ErrAuth wraps any AEAD authentication failure. The HTTP layer
	// maps this to 502 Bad Gateway — never expose tampering details.
	ErrAuth = errors.New("recording.crypto: authentication failed")
)

// AudioDecryptor decrypts a complete AES-256-GCM ciphertext into plaintext
// bytes. Reads the entire ciphertext from r before validating — a partial
// validation would risk delivering tampered plaintext to a client. Size is
// the expected ciphertext length (bytes_size from call_recordings); the
// implementation enforces MaxAudioPlaintextBytes against this BEFORE
// reading.
type AudioDecryptor interface {
	Decrypt(ctx context.Context, key []byte, ciphertext io.Reader, size int64, aad []byte) ([]byte, error)
}

// AESGCMDecryptor is the default AudioDecryptor backed by pkg/encryption.
// Stateless; safe to share across goroutines.
type AESGCMDecryptor struct{}

// NewAESGCMDecryptor returns a new AESGCMDecryptor.
func NewAESGCMDecryptor() *AESGCMDecryptor { return &AESGCMDecryptor{} }

// compile-time interface check
var _ AudioDecryptor = (*AESGCMDecryptor)(nil)

// Decrypt reads size bytes from ciphertext, enforces the 200 MiB cap, then
// delegates to pkg/encryption.Decrypt. Returns ErrAudioTooLargeForV1 when
// size exceeds MaxAudioPlaintextBytes, ErrAuth on AEAD tag failure, and
// context.Canceled / context.DeadlineExceeded when ctx is done.
func (d *AESGCMDecryptor) Decrypt(ctx context.Context, key []byte, ciphertext io.Reader, size int64, aad []byte) ([]byte, error) {
	if size <= 0 {
		return nil, fmt.Errorf("recording.crypto: invalid size %d", size)
	}
	if size > MaxAudioPlaintextBytes {
		return nil, fmt.Errorf("%w: size=%d cap=%d", ErrAudioTooLargeForV1, size, MaxAudioPlaintextBytes)
	}
	buf := bytes.NewBuffer(make([]byte, 0, size))
	if _, err := io.CopyN(buf, &ctxReader{ctx: ctx, r: ciphertext}, size); err != nil {
		return nil, fmt.Errorf("recording.crypto: read ciphertext: %w", err)
	}
	plain, err := encryption.Decrypt(key, buf.Bytes(), aad)
	if err != nil {
		if errors.Is(err, encryption.ErrAuth) {
			return nil, fmt.Errorf("%w: %s", ErrAuth, err.Error())
		}
		return nil, fmt.Errorf("recording.crypto: decrypt: %w", err)
	}
	return plain, nil
}

// ctxReader wraps r so Read returns ctx.Err() on cancellation. Cheap;
// only checks ctx between reads, not mid-syscall.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (cr *ctxReader) Read(p []byte) (int, error) {
	if err := cr.ctx.Err(); err != nil {
		return 0, err
	}
	return cr.r.Read(p)
}
