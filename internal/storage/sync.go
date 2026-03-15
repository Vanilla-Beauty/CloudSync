package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cloudsync/cloudsync/internal/limiter"
	"go.uber.org/zap"
)

// syncDirection indicates which way a file should be synchronised.
type syncDirection int

const (
	dirSkip     syncDirection = iota
	dirUpload                 // local → COS
	dirDownload               // COS → local
)

// timeTolerance is the window within which two timestamps are considered equal.
const timeTolerance = 2 * time.Second

// SyncStats holds counters for a syncer's activity.
type SyncStats struct {
	Uploads   int64
	Downloads int64
	Deletes   int64
	Errors    int64
	LastSync  time.Time
}

// Syncer orchestrates file synchronization to COS
type Syncer struct {
	cos          *COSClient
	metadata     *MetadataStore
	rateLimiter  *limiter.RateLimiter
	logger       *zap.Logger
	localRoot    string
	remotePrefix string
	shouldIgnore func(string) bool // optional; applied during SyncDirectory / BidirSyncDirectory

	statsMu sync.Mutex
	stats   SyncStats
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

// Stats returns a snapshot of the syncer's activity counters.
func (s *Syncer) Stats() SyncStats {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	return s.stats
}

// SetIgnoreFunc attaches a filter to be applied during SyncDirectory / BidirSyncDirectory.
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

// decideSyncDirection determines whether to upload, download, or skip for a file.
func (s *Syncer) decideSyncDirection(ctx context.Context, localPath, remoteKey string) (syncDirection, *RemoteObjectInfo, error) {
	localStat, localErr := os.Stat(localPath)
	localExists := localErr == nil

	remoteInfo, err := s.cos.Head(ctx, remoteKey)
	if err != nil {
		return dirSkip, nil, err
	}

	switch {
	case !localExists && !remoteInfo.Exists:
		return dirSkip, remoteInfo, nil

	case localExists && !remoteInfo.Exists:
		return dirUpload, remoteInfo, nil

	case !localExists && remoteInfo.Exists:
		return dirDownload, remoteInfo, nil
	}

	// Both exist — use stored baseline when available.
	if baseline, ok := s.metadata.GetSyncStatus(localPath); ok {
		localHash, hashErr := HashFile(localPath)
		if hashErr != nil {
			return dirSkip, remoteInfo, hashErr
		}
		localChanged := localHash != baseline.Hash
		remoteChanged := remoteInfo.ETag != baseline.RemoteETag

		switch {
		case !localChanged && !remoteChanged:
			return dirSkip, remoteInfo, nil
		case localChanged && !remoteChanged:
			return dirUpload, remoteInfo, nil
		case !localChanged && remoteChanged:
			return dirDownload, remoteInfo, nil
		default:
			// Both changed — newer timestamp wins; tie goes to upload.
			localMod := localStat.ModTime()
			if remoteInfo.LastModified.After(localMod.Add(timeTolerance)) {
				return dirDownload, remoteInfo, nil
			}
			return dirUpload, remoteInfo, nil
		}
	}

	// No baseline — compare modification timestamps.
	localMod := localStat.ModTime()
	diff := localMod.Sub(remoteInfo.LastModified)
	if diff > timeTolerance {
		return dirUpload, remoteInfo, nil
	}
	if diff < -timeTolerance {
		return dirDownload, remoteInfo, nil
	}
	return dirSkip, remoteInfo, nil
}

func (s *Syncer) syncOne(ctx context.Context, localPath string) {
	if _, err := os.Stat(localPath); os.IsNotExist(err) {
		remoteKey := s.remoteKey(localPath)
		if err := s.cos.Delete(ctx, remoteKey); err != nil {
			s.logger.Warn("delete remote failed", zap.String("key", remoteKey), zap.Error(err))
			s.statsMu.Lock(); s.stats.Errors++; s.statsMu.Unlock()
			return
		}
		s.logger.Info("deleted remote", zap.String("key", remoteKey))
		s.statsMu.Lock(); s.stats.Deletes++; s.stats.LastSync = time.Now().UTC(); s.statsMu.Unlock()
		s.metadata.SetSyncStatus(localPath, nil)
		return
	}

	remoteKey := s.remoteKey(localPath)
	dir, remoteInfo, err := s.decideSyncDirection(ctx, localPath, remoteKey)
	if err != nil {
		s.logger.Warn("decide sync direction failed", zap.String("path", localPath), zap.Error(err))
		return
	}

	switch dir {
	case dirUpload:
		s.upload(ctx, localPath, remoteKey)
	case dirDownload:
		s.download(ctx, localPath, remoteKey, remoteInfo)
	}
}

// upload uploads localPath to remoteKey and records the sync status.
func (s *Syncer) upload(ctx context.Context, localPath, remoteKey string) {
	hash, err := HashFile(localPath)
	if err != nil {
		s.logger.Warn("hash failed", zap.String("path", localPath), zap.Error(err))
		return
	}

	if err := s.rateLimiter.Acquire(ctx); err != nil {
		return
	}
	defer s.rateLimiter.Release()

	etag, err := s.cos.Upload(ctx, localPath, remoteKey)
	if err != nil {
		s.logger.Error("upload failed", zap.String("path", localPath), zap.String("key", remoteKey), zap.Error(err))
		s.statsMu.Lock(); s.stats.Errors++; s.statsMu.Unlock()
		return
	}

	// Fetch the real LastModified that COS assigned after the upload.
	remoteModTime := time.Now().UTC()
	if info, headErr := s.cos.Head(ctx, remoteKey); headErr == nil && info.Exists {
		remoteModTime = info.LastModified
	}

	s.metadata.SetSyncStatus(localPath, &SyncStatus{
		LastSyncedAt:  time.Now().UTC(),
		RemoteKey:     remoteKey,
		Hash:          hash,
		RemoteETag:    etag,
		RemoteModTime: remoteModTime,
	})
	s.metadata.SetFileHash(localPath, hash)
	s.statsMu.Lock(); s.stats.Uploads++; s.stats.LastSync = time.Now().UTC(); s.statsMu.Unlock()
	s.logger.Info("uploaded", zap.String("path", localPath), zap.String("key", remoteKey))
}

// download downloads remoteKey to localPath and records the sync status.
// If both sides changed (conflict), the local file is preserved as a conflict copy
// before being overwritten by the remote version.
func (s *Syncer) download(ctx context.Context, localPath, remoteKey string, remoteInfo *RemoteObjectInfo) {
	if err := s.rateLimiter.Acquire(ctx); err != nil {
		return
	}
	defer s.rateLimiter.Release()

	// If a local file exists and both sides changed, save a conflict copy first.
	if _, statErr := os.Stat(localPath); statErr == nil {
		if baseline, ok := s.metadata.GetSyncStatus(localPath); ok {
			localHash, _ := HashFile(localPath)
			if localHash != baseline.Hash && remoteInfo.ETag != baseline.RemoteETag {
				if conflictPath := conflictCopyPath(localPath); conflictPath != "" {
					if copyErr := copyFile(localPath, conflictPath); copyErr == nil {
						s.logger.Warn("conflict: saved local copy",
							zap.String("path", localPath),
							zap.String("conflict_copy", conflictPath),
						)
					}
				}
			}
		}
	}

	if err := s.cos.Download(ctx, remoteKey, localPath); err != nil {
		s.logger.Error("download failed", zap.String("path", localPath), zap.String("key", remoteKey), zap.Error(err))
		s.statsMu.Lock(); s.stats.Errors++; s.statsMu.Unlock()
		return
	}

	// Preserve the remote modification time locally.
	if !remoteInfo.LastModified.IsZero() {
		_ = os.Chtimes(localPath, time.Now(), remoteInfo.LastModified)
	}

	hash, err := HashFile(localPath)
	if err != nil {
		s.logger.Warn("hash after download failed", zap.String("path", localPath), zap.Error(err))
		return
	}

	s.metadata.SetSyncStatus(localPath, &SyncStatus{
		LastSyncedAt:  time.Now().UTC(),
		RemoteKey:     remoteKey,
		Hash:          hash,
		RemoteETag:    remoteInfo.ETag,
		RemoteModTime: remoteInfo.LastModified,
	})
	s.metadata.SetFileHash(localPath, hash)
	s.statsMu.Lock(); s.stats.Downloads++; s.stats.LastSync = time.Now().UTC(); s.statsMu.Unlock()
	s.logger.Info("downloaded", zap.String("path", localPath), zap.String("key", remoteKey))
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

// BidirSyncDirectory performs a bidirectional initial sync:
// 1. Walks the local root and syncs each file (upload/download/skip per decideSyncDirection).
// 2. Downloads any remote-only objects that are not ignored.
func (s *Syncer) BidirSyncDirectory(ctx context.Context) error {
	// Fetch all remote objects once so we can detect remote-only files.
	remoteObjects, err := s.cos.ListWithMeta(ctx, s.remotePrefix)
	if err != nil {
		return err
	}
	remoteMap := make(map[string]RemoteObjectInfo, len(remoteObjects))
	for _, obj := range remoteObjects {
		remoteMap[obj.Key] = obj
	}

	// Walk local root; collect paths and track which remote keys have a local counterpart.
	seenRemoteKeys := make(map[string]bool)
	var localPaths []string

	err = filepath.Walk(s.localRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if s.shouldIgnore != nil && s.shouldIgnore(path) {
			s.logger.Debug("bidir scan: ignored", zap.String("path", path))
			return nil
		}
		localPaths = append(localPaths, path)
		seenRemoteKeys[s.remoteKey(path)] = true
		return nil
	})
	if err != nil {
		return err
	}

	// Sync all local files (handles upload / download / skip internally).
	s.SyncFiles(ctx, localPaths)

	// Download remote-only objects in parallel.
	prefix := strings.TrimSuffix(s.remotePrefix, "/")
	var wg sync.WaitGroup
	for key, obj := range remoteMap {
		if seenRemoteKeys[key] {
			continue
		}
		// Compute the local path from the remote key.
		rel := strings.TrimPrefix(key, prefix+"/")
		if rel == key && prefix != "" {
			// Key does not belong to this prefix.
			continue
		}
		localPath := filepath.Join(s.localRoot, filepath.FromSlash(rel))

		if s.shouldIgnore != nil && s.shouldIgnore(localPath) {
			s.logger.Debug("bidir scan: ignored remote-only", zap.String("key", key))
			continue
		}

		key := key // capture loop variable
		obj := obj
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.download(ctx, localPath, key, &obj)
		}()
	}
	wg.Wait()
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

// DeleteDirectory deletes all remote objects whose keys begin with the directory's remote prefix.
// Used when a local directory is deleted.
func (s *Syncer) DeleteDirectory(ctx context.Context, localDirPath string) {
	prefix := s.remoteKey(localDirPath) + "/"
	keys, err := s.cos.List(ctx, prefix)
	if err != nil {
		s.logger.Warn("list remote for dir deletion failed",
			zap.String("prefix", prefix), zap.Error(err))
		return
	}
	for _, key := range keys {
		if err := s.cos.Delete(ctx, key); err != nil {
			s.logger.Warn("delete remote object failed",
				zap.String("key", key), zap.Error(err))
		} else {
			s.logger.Info("deleted remote (dir removed)", zap.String("key", key))
		}
	}
}

// conflictCopyPath returns a path like "file (conflict copy 2026-03-15).txt"
// placed next to the original file. Returns "" if the path cannot be constructed.
func conflictCopyPath(localPath string) string {
	dir := filepath.Dir(localPath)
	base := filepath.Base(localPath)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	date := time.Now().Format("2006-01-02")
	return filepath.Join(dir, fmt.Sprintf("%s (conflict copy %s)%s", stem, date, ext))
}

// copyFile copies src to dst, creating parent dirs as needed.
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}
