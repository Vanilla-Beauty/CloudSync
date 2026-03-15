package watcher

import (
	"context"
	"os"
	"path/filepath"
	"sync"
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
		watcher:      fw,
		ignoreRules:  ignoreRules,
		localRoot:    cfg.LocalRoot,
		ctx:          ctx,
		cancel:       cancel,
		logger:       logger,
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

	// If new directory created, watch it recursively
	if event.Op&fsnotify.Create != 0 {
		info, err := os.Lstat(path)
		if err == nil && info.IsDir() {
			if err := sw.addPathRecursive(path); err != nil {
				sw.logger.Warn("failed to watch new dir", zap.String("path", path), zap.Error(err))
			}
			// Scan existing contents to catch files that arrived before watch was registered
			// (race window between directory creation and watch registration)
			sw.scanAndEnqueue(path)
			return
		}
	}

	// Remove events: still pass to syncer (it will handle missing file as delete)
	// Filter first
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

// scanAndEnqueue walks a directory and feeds all non-ignored files into the debouncer.
// This plugs the race window between directory creation and watch registration: files
// already present inside a newly-created (or moved-in) directory would otherwise be
// missed because their CREATE events fired before fsnotify started watching that dir.
func (sw *SyncWatcher) scanAndEnqueue(dir string) {
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if sw.shouldIgnore(path) {
			return nil
		}
		sw.debouncer.Trigger(path)
		return nil
	})
}

// addPathRecursive watches a directory and all its subdirectories, skipping symlinks
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
			return sw.watcher.Add(path)
		}
		return nil
	})
}
