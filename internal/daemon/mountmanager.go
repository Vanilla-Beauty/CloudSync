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
	"strings"
	"sync"
	"time"

	"github.com/cloudsync/cloudsync/internal/config"
	"github.com/cloudsync/cloudsync/internal/filter"
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
	cos     *storage.COSClient // may differ from mm.cos when per-mount bucket is set
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
		if err := mm.startWatcher(rec, false); err != nil {
			mm.logger.Warn("failed to restore mount", zap.String("path", rec.LocalPath), zap.Error(err))
		}
	}
	return nil
}

// Add creates a new mount, starts its watcher, and persists.
// If downloadFirst is true, remote objects are downloaded before the initial upload scan.
// bucket overrides the default bucket; empty string uses the daemon-configured bucket.
func (mm *MountManager) Add(localPath, remotePrefix string, downloadFirst bool, bucket string) (ipc.MountRecord, error) {
	rec := ipc.MountRecord{
		ID:           randomID(),
		LocalPath:    localPath,
		RemotePrefix: remotePrefix,
		Bucket:       bucket,
		AddedAt:      time.Now().UTC(),
	}

	if err := mm.startWatcher(rec, downloadFirst); err != nil {
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
		keys, err := found.cos.List(ctx, found.record.RemotePrefix)
		if err != nil {
			mm.logger.Warn("list remote for deletion failed", zap.Error(err))
		} else {
			for _, k := range keys {
				if err := found.cos.Delete(ctx, k); err != nil {
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

// DeleteObjects asynchronously deletes all COS objects under remotePrefix.
// It also removes the corresponding local files if a mount covers that prefix.
// The call returns immediately; deletion happens in a background goroutine.
func (mm *MountManager) DeleteObjects(remotePrefix string) {
	// Find the entry whose RemotePrefix matches, to get the right COSClient.
	mm.mu.RLock()
	var matchCOS *storage.COSClient
	var localDir string
	for _, e := range mm.entries {
		if e.record.RemotePrefix == remotePrefix ||
			strings.HasPrefix(remotePrefix, e.record.RemotePrefix) {
			matchCOS = e.cos
			rel := strings.TrimPrefix(remotePrefix, e.record.RemotePrefix)
			localDir = filepath.Join(e.record.LocalPath, filepath.FromSlash(rel))
			break
		}
	}
	mm.mu.RUnlock()

	cosClient := matchCOS
	if cosClient == nil {
		cosClient = mm.cos // fall back to default
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		keys, err := cosClient.List(ctx, remotePrefix)
		if err != nil {
			mm.logger.Warn("DeleteObjects: list failed",
				zap.String("prefix", remotePrefix), zap.Error(err))
			return
		}
		for _, k := range keys {
			if err := cosClient.Delete(ctx, k); err != nil {
				mm.logger.Warn("DeleteObjects: delete failed",
					zap.String("key", k), zap.Error(err))
			}
		}
		// Best-effort: remove local files if this prefix is under a mount.
		if localDir != "" {
			_ = os.RemoveAll(localDir)
		}
		mm.logger.Info("DeleteObjects: complete",
			zap.String("prefix", remotePrefix), zap.Int("count", len(keys)))
	}()
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
// If downloadFirst is true, remote files are downloaded before the initial upload scan.
func (mm *MountManager) startWatcher(rec ipc.MountRecord, downloadFirst bool) error {
	// Ensure the local directory exists. This allows AddMount to be called
	// for a path that does not yet exist (e.g. from the TUI browser).
	if err := os.MkdirAll(rec.LocalPath, 0755); err != nil {
		return fmt.Errorf("create local path %s: %w", rec.LocalPath, err)
	}

	// Resolve the COSClient for this mount: use a per-mount client when a
	// bucket override is set, otherwise fall back to the daemon default.
	mountCOS := mm.cos
	if rec.Bucket != "" && rec.Bucket != mm.cfg.COS.Bucket {
		overrideCfg := mm.cfg.COS
		overrideCfg.Bucket = rec.Bucket
		// Derive region from the bucket name (format: name-appid, region in config).
		// We keep the configured region unless the daemon has none.
		if overrideCfg.Region == "" {
			overrideCfg.Region = "ap-guangzhou"
		}
		var err error
		mountCOS, err = storage.NewCOSClient(&overrideCfg, mm.metadata, mm.logger)
		if err != nil {
			return fmt.Errorf("create cos client for bucket %s: %w", rec.Bucket, err)
		}
	}

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
		mountCOS,
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
	mm.entries[rec.ID] = &watcherEntry{record: rec, watcher: sw, cos: mountCOS}
	mm.mu.Unlock()

	// Initial full-directory scan in background, with the same filters as the watcher.
	syncer := storage.NewSyncer(mountCOS, mm.metadata, mm.rl, mm.logger, rec.LocalPath, rec.RemotePrefix)
	ignoreRules, _ := filter.LoadIgnoreRules(ignorePath)
	swapDetector := filter.NewSwapDetector()
	syncer.SetIgnoreFunc(func(path string) bool {
		rel, err := filepath.Rel(rec.LocalPath, path)
		if err != nil {
			rel = path
		}
		return ignoreRules.Match(rel) || swapDetector.IsSwapFile(path)
	})
	go func() {
		ctx := context.Background()
		if downloadFirst {
			if err := syncer.DownloadDirectory(ctx); err != nil {
				mm.logger.Warn("initial download failed", zap.String("path", rec.LocalPath), zap.Error(err))
			}
		}
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
