package storage

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// SyncStatus holds sync state for a file
type SyncStatus struct {
	LastSyncedAt time.Time
	RemoteKey    string
	Hash         string
}

// MetadataStore is an in-memory store for file hashes and sync status
type MetadataStore struct {
	mu     sync.RWMutex
	hashes map[string]string
	status map[string]*SyncStatus
}

// NewMetadataStore creates a new MetadataStore
func NewMetadataStore() *MetadataStore {
	return &MetadataStore{
		hashes: make(map[string]string),
		status: make(map[string]*SyncStatus),
	}
}

// GetFileHash returns the last known hash for a local path
func (m *MetadataStore) GetFileHash(path string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	h, ok := m.hashes[path]
	return h, ok
}

// SetFileHash stores the hash for a local path
func (m *MetadataStore) SetFileHash(path, hash string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hashes[path] = hash
}

// GetSyncStatus returns the sync status for a path
func (m *MetadataStore) GetSyncStatus(path string) (*SyncStatus, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.status[path]
	return s, ok
}

// SetSyncStatus stores the sync status for a path
func (m *MetadataStore) SetSyncStatus(path string, status *SyncStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status[path] = status
}

// HashFile computes the SHA-256 hash of a file using streaming I/O
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
