// Package storage is the project-wide view of object storage. Production
// runs against Yandex Object Storage (S3-compatible, Plan 01); the
// LocalObjectStore in this package serves tests + dev environments without
// cloud credentials.
//
// Plan 13.3 extends the port from "Get + Delete" (the recording read-side)
// to also include Put + PresignedURL so reports can upload XLSX/CSV/PDF
// artifacts and emit 24h presigned download URLs. If a third module
// consumer appears, extract this port to pkg/objectstore (rule of three).
package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sync"
	"time"
)

// ObjectStore is the project-wide view of object storage. Reports +
// recording share this port; if a 3rd module shows up, extract to
// pkg/objectstore.
type ObjectStore interface {
	// Get streams the object as ciphertext. The returned ReadCloser MUST
	// be closed by the caller.
	//
	// Errors:
	//   ErrObjectNotFound — object does not exist or has been deleted.
	//   wrapped network/IO errors — propagated as-is for retry classification.
	Get(ctx context.Context, bucket, key string) (io.ReadCloser, error)

	// Delete removes the object. Idempotent — calling Delete on a missing
	// object returns nil (matches S3 semantics).
	Delete(ctx context.Context, bucket, key string) error

	// Put uploads payload under (bucket, key) with the given Content-Type.
	// Overwrites existing objects; matches S3 PUT semantics.
	Put(ctx context.Context, bucket, key string, payload []byte, contentType string) error

	// PresignedURL returns a time-limited URL granting GET access to
	// (bucket, key). The implementation MUST encode the TTL into the URL
	// (Yandex S3 uses the X-Amz-Expires query param). Production callers
	// pass 24*time.Hour (reports artifacts).
	PresignedURL(ctx context.Context, bucket, key string, ttl time.Duration) (string, error)
}

// ErrObjectNotFound is returned by Get when the object is missing.
// Plan 12.4 service.OpenAudioStream MUST hide this behind api.ErrNotFound
// so the storage shape doesn't leak to API consumers.
var ErrObjectNotFound = errors.New("recording.storage: object not found")

// LocalObjectStore is an in-memory ObjectStore for tests and local dev.
// Concurrent-safe via sync.RWMutex. Bucket isolation is preserved — each
// (bucket, key) tuple is a separate slot.
//
// Production uses the Yandex S3 implementation (Plan 01); LocalObjectStore
// is the dev/test seam.
type LocalObjectStore struct {
	mu      sync.RWMutex
	objects map[string]map[string]localBlob
}

type localBlob struct {
	payload     []byte
	contentType string
}

// NewLocalObjectStore returns an empty in-memory ObjectStore.
func NewLocalObjectStore() *LocalObjectStore {
	return &LocalObjectStore{objects: make(map[string]map[string]localBlob)}
}

// Compile-time interface check.
var _ ObjectStore = (*LocalObjectStore)(nil)

// Get satisfies ObjectStore. The returned ReadCloser owns a CLONE of the
// stored bytes — caller mutations to the returned buffer cannot corrupt
// the in-memory store.
func (s *LocalObjectStore) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.objects[bucket]
	if !ok {
		return nil, fmt.Errorf("%w: bucket=%s key=%s", ErrObjectNotFound, bucket, key)
	}
	blob, ok := b[key]
	if !ok {
		return nil, fmt.Errorf("%w: bucket=%s key=%s", ErrObjectNotFound, bucket, key)
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(blob.payload))), nil
}

// Delete satisfies ObjectStore. Idempotent on missing objects.
func (s *LocalObjectStore) Delete(ctx context.Context, bucket, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.objects[bucket]; ok {
		delete(b, key)
	}
	return nil
}

// Put stores the payload under (bucket, key) and overwrites any existing
// entry. The contentType field is held for symmetry with the production
// Yandex S3 implementation; the local store does not actually serve a
// Content-Type header.
func (s *LocalObjectStore) Put(ctx context.Context, bucket, key string, payload []byte, contentType string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objects[bucket]; !ok {
		s.objects[bucket] = make(map[string]localBlob)
	}
	s.objects[bucket][key] = localBlob{payload: bytes.Clone(payload), contentType: contentType}
	return nil
}

// PresignedURL returns a stub URL of the shape
//
//	local://<bucket>/<key>?expires=<unix>
//
// Production overrides this with the Yandex SDK presigner.
func (s *LocalObjectStore) PresignedURL(ctx context.Context, bucket, key string, ttl time.Duration) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if ttl <= 0 {
		return "", fmt.Errorf("recording.storage: ttl must be positive, got %v", ttl)
	}
	u := url.URL{
		Scheme: "local",
		Host:   bucket,
		Path:   "/" + key,
	}
	q := u.Query()
	q.Set("expires", fmt.Sprintf("%d", time.Now().UTC().Add(ttl).Unix()))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// PutBytes is the legacy test seam that pre-dates Put. Forwards to Put
// with an empty content type. Kept for backwards compatibility with
// existing recording tests; do NOT use in new code.
//
// Deprecated: use Put.
func (s *LocalObjectStore) PutBytes(bucket, key string, payload []byte) {
	_ = s.Put(context.Background(), bucket, key, payload, "")
}
