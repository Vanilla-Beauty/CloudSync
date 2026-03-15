package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cloudsync/cloudsync/internal/limiter"
	"go.uber.org/zap"
)

// Syncer orchestrates file synchronization to COS
type Syncer struct {
	cos         *COSClient
	metadata    *MetadataStore
	rateLimiter *limiter.RateLimiter
	logger      *zap.Logger
	localRoot   string
	remotePrefix string
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
