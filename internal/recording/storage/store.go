// Package storage is the recording module's view of object storage.
// Production runs against Yandex Object Storage (S3-compatible, Plan 01);
// the LocalObjectStore in this package serves tests + dev environments
// without cloud credentials.
package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ObjectStore is the recording module's view of object storage. Plan 12.2
// uses Get for stream playback and Delete for the retention worker
// (deferred — Plan 12.4). All methods are tenant-agnostic — the bucket
// name embeds the tenant segregation by design (see migration 000010 +
// tenancy.api.BucketProvisioner).
type ObjectStore interface {
	// Get streams the object as ciphertext. The returned ReadCloser
	// MUST be closed by the caller.
	//
	// Errors:
	//   ErrObjectNotFound — object does not exist or has been deleted.
	//   wrapped network/IO errors — propagated as-is for retry classification.
	Get(ctx context.Context, bucket, key string) (io.ReadCloser, error)

	// Delete removes the object. Idempotent — calling Delete on a
	// missing object returns nil (matches S3 semantics).
	Delete(ctx context.Context, bucket, key string) error
}

// ErrObjectNotFound is returned by Get when the object is missing.
// Plan 12.4 service.OpenAudioStream MUST hide this behind api.ErrNotFound
// so the storage shape doesn't leak to API consumers.
var ErrObjectNotFound = errors.New("recording.storage: object not found")

// LocalObjectStore is an in-memory ObjectStore for tests and local dev.
// Concurrent-safe via sync.RWMutex. Bucket isolation is preserved — each
// (bucket, key) tuple is a separate slot.
//
// Production uses the Yandex S3 implementation (Plan 01); the recording
// flow's "Put" path is owned by the ingest-uploader (Plan 08) which uses
// the Yandex SDK directly. PutBytes here is a TEST SEAM only.
type LocalObjectStore struct {
	mu      sync.RWMutex
	objects map[string]map[string][]byte // bucket → key → bytes
}

func NewLocalObjectStore() *LocalObjectStore {
	return &LocalObjectStore{objects: make(map[string]map[string][]byte)}
}

// Compile-time interface check.
var _ ObjectStore = (*LocalObjectStore)(nil)

// Get satisfies ObjectStore. The returned ReadCloser owns a CLONE of
// the stored bytes — caller mutations to the returned buffer cannot
// corrupt the in-memory store.
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
	payload, ok := b[key]
	if !ok {
		return nil, fmt.Errorf("%w: bucket=%s key=%s", ErrObjectNotFound, bucket, key)
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(payload))), nil
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

// PutBytes seeds the store with a single object — TEST USE ONLY.
// Production deployments use Plan 01's Yandex S3 adapter, which puts
// objects via the Yandex SDK directly (the ingest-uploader is the only
// Put-path producer in production).
func (s *LocalObjectStore) PutBytes(bucket, key string, payload []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objects[bucket]; !ok {
		s.objects[bucket] = make(map[string][]byte)
	}
	s.objects[bucket][key] = bytes.Clone(payload)
}
