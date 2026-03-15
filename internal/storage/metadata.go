package storage

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// SyncStatus holds sync state for a file
type SyncStatus struct {
	LastSyncedAt  time.Time
	RemoteKey     string
	Hash          string
	RemoteETag    string    // ETag at last sync
	RemoteModTime time.Time // COS LastModified at last sync
}

var (
	bucketHashes = []byte("hashes") // localPath → sha256hex
	bucketStatus = []byte("status") // localPath → JSON(SyncStatus)
)

// MetadataStore persists file hashes and sync status to a bbolt database.
// An in-memory fallback is used if the database path is empty or cannot be opened.
type MetadataStore struct {
	db *bolt.DB

	// In-memory fallback (used when db == nil).
	mu     sync.RWMutex
	hashes map[string]string
	status map[string]*SyncStatus
}

// NewMetadataStore creates an in-memory MetadataStore (no persistence).
// Use OpenMetadataStore for a persistent store.
func NewMetadataStore() *MetadataStore {
	return &MetadataStore{
		hashes: make(map[string]string),
		status: make(map[string]*SyncStatus),
	}
}

// OpenMetadataStore opens (or creates) a bbolt database at dbPath.
// Falls back to in-memory store on error so the daemon can still run.
func OpenMetadataStore(dbPath string) (*MetadataStore, error) {
	db, err := bolt.Open(dbPath, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open metadata db: %w", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if _, e := tx.CreateBucketIfNotExists(bucketHashes); e != nil {
			return e
		}
		_, e := tx.CreateBucketIfNotExists(bucketStatus)
		return e
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init metadata buckets: %w", err)
	}
	return &MetadataStore{db: db}, nil
}

// Close closes the underlying bbolt database (no-op for in-memory store).
func (m *MetadataStore) Close() error {
	if m.db != nil {
		return m.db.Close()
	}
	return nil
}

// GetFileHash returns the last known hash for a local path.
func (m *MetadataStore) GetFileHash(path string) (string, bool) {
	if m.db == nil {
		m.mu.RLock()
		defer m.mu.RUnlock()
		h, ok := m.hashes[path]
		return h, ok
	}
	var val string
	_ = m.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketHashes)
		v := b.Get([]byte(path))
		if v != nil {
			val = string(v)
		}
		return nil
	})
	return val, val != ""
}

// SetFileHash stores the hash for a local path.
func (m *MetadataStore) SetFileHash(path, hash string) {
	if m.db == nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.hashes[path] = hash
		return
	}
	_ = m.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketHashes).Put([]byte(path), []byte(hash))
	})
}

// GetSyncStatus returns the sync status for a path.
func (m *MetadataStore) GetSyncStatus(path string) (*SyncStatus, bool) {
	if m.db == nil {
		m.mu.RLock()
		defer m.mu.RUnlock()
		s, ok := m.status[path]
		return s, ok
	}
	var s SyncStatus
	found := false
	_ = m.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketStatus).Get([]byte(path))
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, &s)
	})
	if !found {
		return nil, false
	}
	return &s, true
}

// SetSyncStatus stores the sync status for a path.
// Passing nil clears the entry.
func (m *MetadataStore) SetSyncStatus(path string, status *SyncStatus) {
	if m.db == nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		if status == nil {
			delete(m.status, path)
		} else {
			m.status[path] = status
		}
		return
	}
	_ = m.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketStatus)
		if status == nil {
			return b.Delete([]byte(path))
		}
		data, err := json.Marshal(status)
		if err != nil {
			return err
		}
		return b.Put([]byte(path), data)
	})
}

// HashFile computes the SHA-256 hash of a file using streaming I/O.
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
