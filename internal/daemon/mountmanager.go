package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/cloudsync/cloudsync/internal/config"
	"github.com/cloudsync/cloudsync/internal/ipc"
	"github.com/cloudsync/cloudsync/internal/limiter"
	"github.com/cloudsync/cloudsync/internal/storage"
	"github.com/cloudsync/cloudsync/internal/watcher"
	"go.uber.org/zap"
)

// watcherEntry pairs a MountRecord with its running SyncWatcher
type watcherEntry struct {
	record  ipc.MountRecord
	watcher *watcher.SyncWatcher
}

// MountManager manages the lifecycle of watched directories and persists them.
type MountManager struct {
	mu         sync.RWMutex
	entries    map[string]*watcherEntry // keyed by MountRecord.ID
	mountsPath string
	cos        *storage.COSClient
	metadata   *storage.MetadataStore
	rl         *limiter.RateLimiter
	cfg        *config.Config
	logger     *zap.Logger
}

// NewMountManager creates a MountManager.
func NewMountManager(mountsPath string, cos *storage.COSClient, metadata *storage.MetadataStore, rl *limiter.RateLimiter, cfg *config.Config, logger *zap.Logger) *MountManager {
	return &MountManager{
		entries:    make(map[string]*watcherEntry),
		mountsPath: mountsPath,
		cos:        cos,
		metadata:   metadata,
		rl:         rl,
		cfg:        cfg,
		logger:     logger,
	}
}

// LoadSaved reads mounts.json and restarts watchers for all saved mounts.
func (mm *MountManager) LoadSaved() error {
	data, err := os.ReadFile(mm.mountsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no saved mounts yet
		}
		return fmt.Errorf("read mounts.json: %w", err)
	}

	var mf ipc.MountsFile
	if err := json.Unmarshal(data, &mf); err != nil {
		return fmt.Errorf("parse mounts.json: %w", err)
	}

	for _, rec := range mf.Mounts {
		if err := mm.startWatcher(rec); err != nil {
			mm.logger.Warn("failed to restore mount", zap.String("path", rec.LocalPath), zap.Error(err))
		}
	}
	return nil
}

// Add creates a new mount, starts its watcher, and persists.
func (mm *MountManager) Add(localPath, remotePrefix string) (ipc.MountRecord, error) {
	rec := ipc.MountRecord{
		ID:           randomID(),
		LocalPath:    localPath,
		RemotePrefix: remotePrefix,
		AddedAt:      time.Now().UTC(),
	}

	if err := mm.startWatcher(rec); err != nil {
		return ipc.MountRecord{}, err
	}

	if err := mm.save(); err != nil {
		// Undo on save failure
		mm.mu.Lock()
		if e, ok := mm.entries[rec.ID]; ok {
			e.watcher.Stop()
			delete(mm.entries, rec.ID)
		}
		mm.mu.Unlock()
		return ipc.MountRecord{}, err
	}
	return rec, nil
}

// Remove stops and removes the mount with the given localPath.
// If deleteRemote is true, it also deletes all remote objects.
func (mm *MountManager) Remove(localPath string, deleteRemote bool) error {
	mm.mu.Lock()
	var found *watcherEntry
	for _, e := range mm.entries {
		if e.record.LocalPath == localPath {
			found = e
			break
		}
	}
	if found == nil {
		mm.mu.Unlock()
		return fmt.Errorf("no mount found for path: %s", localPath)
	}
	delete(mm.entries, found.record.ID)
	mm.mu.Unlock()

	found.watcher.Stop()

	if deleteRemote {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		keys, err := mm.cos.List(ctx, found.record.RemotePrefix)
		if err != nil {
			mm.logger.Warn("list remote for deletion failed", zap.Error(err))
		} else {
			for _, k := range keys {
				if err := mm.cos.Delete(ctx, k); err != nil {
					mm.logger.Warn("delete remote object failed", zap.String("key", k), zap.Error(err))
				}
			}
		}
	}

	return mm.save()
}

// List returns a copy of all current mount records.
func (mm *MountManager) List() []ipc.MountRecord {
	mm.mu.RLock()
	defer mm.mu.RUnlock()
	result := make([]ipc.MountRecord, 0, len(mm.entries))
	for _, e := range mm.entries {
		result = append(result, e.record)
	}
	return result
}

// Count returns the number of active mounts.
func (mm *MountManager) Count() int {
	mm.mu.RLock()
	defer mm.mu.RUnlock()
	return len(mm.entries)
}

// StopAll stops all watchers (called on daemon shutdown).
func (mm *MountManager) StopAll() {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	for _, e := range mm.entries {
		e.watcher.Stop()
	}
}

// startWatcher creates and starts a SyncWatcher for the given record.
func (mm *MountManager) startWatcher(rec ipc.MountRecord) error {
	ignorePath := filepath.Join(rec.LocalPath, ".syncignore")

	sw, err := watcher.New(
		watcher.Config{
			LocalRoot:    rec.LocalPath,
			RemotePrefix: rec.RemotePrefix,
			IgnoreFile:   ignorePath,
			DetectSwap:   true,
			Perf: config.PerformanceConfig{
				DebounceMs:      mm.cfg.Performance.DebounceMs,
				BatchIntervalMs: mm.cfg.Performance.BatchIntervalMs,
				BatchMaxSize:    mm.cfg.Performance.BatchMaxSize,
				MaxConcurrent:   mm.cfg.Performance.MaxConcurrent,
				QPS:             mm.cfg.Performance.QPS,
			},
		},
		mm.cos,
		mm.metadata,
		mm.rl,
		mm.logger,
	)
	if err != nil {
		return fmt.Errorf("create watcher: %w", err)
	}

	if err := sw.Start(); err != nil {
		return fmt.Errorf("start watcher: %w", err)
	}

	mm.mu.Lock()
	mm.entries[rec.ID] = &watcherEntry{record: rec, watcher: sw}
	mm.mu.Unlock()

	// Initial full-directory scan in background
	syncer := storage.NewSyncer(mm.cos, mm.metadata, mm.rl, mm.logger, rec.LocalPath, rec.RemotePrefix)
	go func() {
		ctx := context.Background()
		if err := syncer.SyncDirectory(ctx); err != nil {
			mm.logger.Warn("initial sync failed", zap.String("path", rec.LocalPath), zap.Error(err))
		}
	}()

	return nil
}

// save atomically writes the current mount list to mounts.json.
func (mm *MountManager) save() error {
	mm.mu.RLock()
	mounts := make([]ipc.MountRecord, 0, len(mm.entries))
	for _, e := range mm.entries {
		mounts = append(mounts, e.record)
	}
	mm.mu.RUnlock()

	mf := ipc.MountsFile{Mounts: mounts}
	data, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal mounts: %w", err)
	}

	tmpPath := mm.mountsPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return fmt.Errorf("write mounts tmp: %w", err)
	}

	// Windows requires removing destination before rename
	if runtime.GOOS == "windows" {
		_ = os.Remove(mm.mountsPath)
	}
	if err := os.Rename(tmpPath, mm.mountsPath); err != nil {
		return fmt.Errorf("rename mounts: %w", err)
	}
	return nil
}

func randomID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
