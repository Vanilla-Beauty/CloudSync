package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cloudsync/cloudsync/internal/limiter"
	"go.uber.org/zap"
)

// Syncer orchestrates file synchronization to COS
type Syncer struct {
	cos          *COSClient
	metadata     *MetadataStore
	rateLimiter  *limiter.RateLimiter
	logger       *zap.Logger
	localRoot    string
	remotePrefix string
	shouldIgnore func(string) bool // optional; applied during SyncDirectory
}

// NewSyncer creates a Syncer for a given local root and remote prefix
func NewSyncer(cos *COSClient, metadata *MetadataStore, rl *limiter.RateLimiter, logger *zap.Logger, localRoot, remotePrefix string) *Syncer {
	return &Syncer{
		cos:          cos,
		metadata:     metadata,
		rateLimiter:  rl,
		logger:       logger,
		localRoot:    localRoot,
		remotePrefix: remotePrefix,
	}
}

// SetIgnoreFunc attaches a filter to be applied during SyncDirectory.
// paths for which fn returns true are skipped entirely.
func (s *Syncer) SetIgnoreFunc(fn func(string) bool) {
	s.shouldIgnore = fn
}

// SyncFiles syncs a batch of local file paths concurrently
func (s *Syncer) SyncFiles(ctx context.Context, paths []string) {
	var wg sync.WaitGroup
	for _, p := range paths {
		wg.Add(1)
		go func(localPath string) {
			defer wg.Done()
			s.syncOne(ctx, localPath)
		}(p)
	}
	wg.Wait()
}

func (s *Syncer) syncOne(ctx context.Context, localPath string) {
	// Check if file still exists (may have been deleted)
	if _, err := os.Stat(localPath); os.IsNotExist(err) {
		remoteKey := s.remoteKey(localPath)
		if err := s.cos.Delete(ctx, remoteKey); err != nil {
			s.logger.Warn("delete remote failed", zap.String("key", remoteKey), zap.Error(err))
			return
		}
		s.logger.Info("deleted remote", zap.String("key", remoteKey))
		s.metadata.SetSyncStatus(localPath, nil)
		return
	}

	// Compute hash and skip if unchanged
	hash, err := HashFile(localPath)
	if err != nil {
		s.logger.Warn("hash failed", zap.String("path", localPath), zap.Error(err))
		return
	}

	if stored, ok := s.metadata.GetFileHash(localPath); ok && stored == hash {
		s.logger.Debug("skipping unchanged file", zap.String("path", localPath))
		return
	}

	remoteKey := s.remoteKey(localPath)

	if err := s.rateLimiter.Acquire(ctx); err != nil {
		return
	}
	defer s.rateLimiter.Release()

	if err := s.cos.Upload(ctx, localPath, remoteKey); err != nil {
		s.logger.Error("upload failed", zap.String("path", localPath), zap.String("key", remoteKey), zap.Error(err))
		return
	}

	s.metadata.SetFileHash(localPath, hash)
	s.logger.Info("uploaded", zap.String("path", localPath), zap.String("key", remoteKey))
}

// DownloadDirectory downloads all objects under remotePrefix to localRoot.
// After downloading each file, it records its hash in the MetadataStore so
// that the subsequent SyncDirectory scan does not re-upload unchanged files.
func (s *Syncer) DownloadDirectory(ctx context.Context) error {
	keys, err := s.cos.List(ctx, s.remotePrefix)
	if err != nil {
		return fmt.Errorf("list remote prefix %q: %w", s.remotePrefix, err)
	}

	prefix := strings.TrimSuffix(s.remotePrefix, "/")

	var wg sync.WaitGroup
	for _, key := range keys {
		wg.Add(1)
		go func(remoteKey string) {
			defer wg.Done()

			// Derive local path from the remote key
			rel := remoteKey
			if prefix != "" {
				rel = strings.TrimPrefix(remoteKey, prefix+"/")
			}
			rel = filepath.FromSlash(rel)
			localPath := filepath.Join(s.localRoot, rel)

			if err := s.rateLimiter.Acquire(ctx); err != nil {
				return
			}
			defer s.rateLimiter.Release()

			if err := s.cos.Download(ctx, remoteKey, localPath); err != nil {
				s.logger.Error("download failed",
					zap.String("key", remoteKey),
					zap.String("local", localPath),
					zap.Error(err))
				return
			}

			hash, err := HashFile(localPath)
			if err != nil {
				s.logger.Warn("hash after download failed", zap.String("path", localPath), zap.Error(err))
				return
			}
			s.metadata.SetFileHash(localPath, hash)
			s.logger.Info("downloaded", zap.String("key", remoteKey), zap.String("local", localPath))
		}(key)
	}
	wg.Wait()
	return nil
}

// SyncDirectory walks localRoot and syncs all files to COS.
// Files matching the ignore function (set via SetIgnoreFunc) are skipped.
// Used for initial full-scan when a mount is (re-)added.
func (s *Syncer) SyncDirectory(ctx context.Context) error {
	var paths []string
	err := filepath.Walk(s.localRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if info.IsDir() {
			return nil
		}
		if s.shouldIgnore != nil && s.shouldIgnore(path) {
			s.logger.Debug("initial scan: ignored", zap.String("path", path))
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return err
	}
	s.SyncFiles(ctx, paths)
	return nil
}

// remoteKey converts a local file path to its COS object key
func (s *Syncer) remoteKey(localPath string) string {
	rel, err := filepath.Rel(s.localRoot, localPath)
	if err != nil {
		rel = localPath
	}
	rel = filepath.ToSlash(rel)
	rel = strings.TrimPrefix(rel, "./")

	prefix := strings.TrimSuffix(s.remotePrefix, "/")
	if prefix == "" {
		return rel
	}
	return prefix + "/" + rel
}
