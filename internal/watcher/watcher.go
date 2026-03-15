package watcher

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudsync/cloudsync/internal/config"
	"github.com/cloudsync/cloudsync/internal/filter"
	"github.com/cloudsync/cloudsync/internal/limiter"
	"github.com/cloudsync/cloudsync/internal/storage"
	"github.com/fsnotify/fsnotify"
	"go.uber.org/zap"
)

// SyncWatcher monitors a directory and syncs changed files to COS
type SyncWatcher struct {
	watcher      *fsnotify.Watcher
	ignoreRules  *filter.IgnoreRules
	swapDetector *filter.SwapDetector
	debouncer    *Debouncer
	batcher      *Batcher
	syncer       *storage.Syncer
	localRoot    string
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	logger       *zap.Logger

	paused atomic.Bool // toggled by Pause/Resume; checked in handleEvent

	// watchedDirs tracks all directories currently registered with fsnotify.
	// Used to detect directory Remove events and clean up properly.
	watchedDirsMu sync.RWMutex
	watchedDirs   map[string]struct{}
}

// Config holds the parameters needed to build a SyncWatcher
type Config struct {
	LocalRoot    string
	RemotePrefix string
	IgnoreFile   string
	DetectSwap   bool
	Perf         config.PerformanceConfig
}

// New constructs a SyncWatcher. The caller must call Start().
func New(
	cfg Config,
	cosClient *storage.COSClient,
	metadata *storage.MetadataStore,
	rl *limiter.RateLimiter,
	logger *zap.Logger,
) (*SyncWatcher, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	ignoreRules, err := filter.LoadIgnoreRules(cfg.IgnoreFile)
	if err != nil {
		fw.Close()
		return nil, err
	}

	syncer := storage.NewSyncer(cosClient, metadata, rl, logger, cfg.LocalRoot, cfg.RemotePrefix)

	ctx, cancel := context.WithCancel(context.Background())

	debounceDelay := time.Duration(cfg.Perf.DebounceMs) * time.Millisecond
	batchInterval := time.Duration(cfg.Perf.BatchIntervalMs) * time.Millisecond

	sw := &SyncWatcher{
		watcher:     fw,
		ignoreRules: ignoreRules,
		localRoot:   cfg.LocalRoot,
		ctx:         ctx,
		cancel:      cancel,
		logger:      logger,
		watchedDirs: make(map[string]struct{}),
	}

	if cfg.DetectSwap {
		sw.swapDetector = filter.NewSwapDetector()
	}

	// Wire: debouncer feeds batcher
	sw.batcher = NewBatcher(cfg.Perf.BatchMaxSize, batchInterval, func(paths []string) {
		sw.processBatch(paths)
	})

	sw.debouncer = NewDebouncer(debounceDelay, func(path string) {
		sw.batcher.Add(path)
	})

	sw.syncer = syncer
	return sw, nil
}

// Start adds the root path recursively and begins processing events
func (sw *SyncWatcher) Start() error {
	if err := sw.addPathRecursive(sw.localRoot); err != nil {
		return err
	}
	sw.wg.Add(1)
	go sw.eventLoop()
	return nil
}

// Stop shuts down the watcher, flushing any pending work
func (sw *SyncWatcher) Stop() {
	sw.cancel()
	sw.watcher.Close()
	sw.debouncer.Close()
	sw.batcher.Close()
	sw.wg.Wait()
}

func (sw *SyncWatcher) eventLoop() {
	defer sw.wg.Done()
	for {
		select {
		case event, ok := <-sw.watcher.Events:
			if !ok {
				return
			}
			sw.handleEvent(event)
		case err, ok := <-sw.watcher.Errors:
			if !ok {
				return
			}
			sw.logger.Warn("watcher error", zap.Error(err))
		case <-sw.ctx.Done():
			return
		}
	}
}

func (sw *SyncWatcher) handleEvent(event fsnotify.Event) {
	path := event.Name

	// Drop events while paused (directory structural changes are still tracked).
	if sw.paused.Load() {
		return
	}

	// New directory: register recursively and return (no sync needed for empty dirs).
	if event.Op&fsnotify.Create != 0 {
		info, err := os.Lstat(path)
		if err == nil && info.IsDir() {
			if err := sw.addPathRecursive(path); err != nil {
				sw.logger.Warn("failed to watch new dir", zap.String("path", path), zap.Error(err))
			}
			return
		}
	}

	// Directory removed: propagate deletion to COS and remove from watch set.
	if event.Op&fsnotify.Remove != 0 {
		sw.watchedDirsMu.RLock()
		_, wasDir := sw.watchedDirs[path]
		sw.watchedDirsMu.RUnlock()

		if wasDir {
			sw.watchedDirsMu.Lock()
			delete(sw.watchedDirs, path)
			sw.watchedDirsMu.Unlock()
			_ = sw.watcher.Remove(path)
			sw.debouncer.Cancel(path)
			go sw.syncer.DeleteDirectory(sw.ctx, path)
			return
		}
	}

	if sw.shouldIgnore(path) {
		sw.logger.Debug("ignored", zap.String("path", path))
		return
	}

	sw.debouncer.Trigger(path)
}

func (sw *SyncWatcher) shouldIgnore(path string) bool {
	rel, err := filepath.Rel(sw.localRoot, path)
	if err != nil {
		rel = path
	}
	if sw.ignoreRules.Match(rel) {
		return true
	}
	if sw.swapDetector != nil && sw.swapDetector.IsSwapFile(path) {
		return true
	}
	return false
}

func (sw *SyncWatcher) processBatch(paths []string) {
	sw.logger.Info("syncing batch", zap.Int("count", len(paths)))
	sw.syncer.SyncFiles(sw.ctx, paths)
}

// Stats returns the accumulated sync statistics for this watcher.
func (sw *SyncWatcher) Stats() storage.SyncStats {
	return sw.syncer.Stats()
}

// Pause suspends event processing. In-flight syncs are not cancelled.
func (sw *SyncWatcher) Pause() { sw.paused.Store(true) }

// Resume re-enables event processing after a Pause.
func (sw *SyncWatcher) Resume() { sw.paused.Store(false) }

// IsPaused reports whether the watcher is currently paused.
func (sw *SyncWatcher) IsPaused() bool { return sw.paused.Load() }

// addPathRecursive watches a directory and all its subdirectories, skipping symlinks.
// Each watched directory is recorded in watchedDirs.
func (sw *SyncWatcher) addPathRecursive(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip unreadable
		}
		// Skip symlinks
		linfo, lerr := os.Lstat(path)
		if lerr == nil && linfo.Mode()&os.ModeSymlink != 0 {
			return filepath.SkipDir
		}
		if info.IsDir() {
			sw.watchedDirsMu.Lock()
			sw.watchedDirs[path] = struct{}{}
			sw.watchedDirsMu.Unlock()
			return sw.watcher.Add(path)
		}
		return nil
	})
}
