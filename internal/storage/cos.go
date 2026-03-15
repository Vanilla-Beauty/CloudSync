package storage

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/cloudsync/cloudsync/internal/config"
	cos "github.com/tencentyun/cos-go-sdk-v5"
	"go.uber.org/zap"
)

const maxRetries = 3

// BucketInfo holds basic metadata about a COS bucket.
type BucketInfo struct {
	Name         string
	Region       string
	CreationDate string
}

// ListBuckets returns all buckets accessible with the given credentials.
// This uses the service-level endpoint (service.cos.myqcloud.com) and does
// not require a specific bucket or region to be known in advance.
func ListBuckets(ctx context.Context, secretID, secretKey string) ([]BucketInfo, error) {
	client := cos.NewClient(nil, &http.Client{
		Transport: &cos.AuthorizationTransport{
			SecretID:  secretID,
			SecretKey: secretKey,
		},
	})
	result, _, err := client.Service.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("list buckets: %w", err)
	}
	buckets := make([]BucketInfo, 0, len(result.Buckets))
	for _, b := range result.Buckets {
		buckets = append(buckets, BucketInfo{
			Name:         b.Name,
			Region:       b.Region,
			CreationDate: b.CreationDate,
		})
	}
	return buckets, nil
}

// RemoteObjectInfo holds metadata about a remote COS object.
type RemoteObjectInfo struct {
	Key          string
	LastModified time.Time
	ETag         string
	Size         int64
	Exists       bool
}

// COSClient wraps the COS SDK client
type COSClient struct {
	client   *cos.Client
	bucket   string
	prefix   string
	metadata *MetadataStore
	logger   *zap.Logger
}

// NewCOSClient creates a new COSClient from config
func NewCOSClient(cfg *config.COSConfig, metadata *MetadataStore, logger *zap.Logger) (*COSClient, error) {
	if cfg.Bucket == "" || cfg.SecretID == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("COS configuration incomplete: bucket, secret_id, and secret_key are required")
	}

	baseURL := fmt.Sprintf("https://%s.cos.%s.myqcloud.com", cfg.Bucket, cfg.Region)
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid COS URL: %w", err)
	}

	b := &cos.BaseURL{BucketURL: u}
	client := cos.NewClient(b, &http.Client{
		Transport: &cos.AuthorizationTransport{
			SecretID:  cfg.SecretID,
			SecretKey: cfg.SecretKey,
		},
	})

	return &COSClient{
		client:   client,
		bucket:   cfg.Bucket,
		metadata: metadata,
		logger:   logger,
	}, nil
}

// multipartThreshold is the file size above which Object.Upload (multipart) is used.
// COS recommends multipart for files larger than 32 MB; hard limit for single PUT is 5 GB.
const multipartThreshold = 32 * 1024 * 1024 // 32 MB

// Upload uploads a local file to COS.
// Files ≤ 32 MB are sent with a single PUT; larger files use concurrent multipart upload.
// Returns the ETag from the response on success.
func (c *COSClient) Upload(ctx context.Context, localPath, remoteKey string) (etag string, err error) {
	info, err := os.Stat(localPath)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", localPath, err)
	}

	if info.Size() <= multipartThreshold {
		// Small file — simple PUT with retry.
		f, err := os.Open(localPath)
		if err != nil {
			return "", fmt.Errorf("open %s: %w", localPath, err)
		}
		defer f.Close()

		err = c.withRetry(ctx, func() error {
			if _, seekErr := f.Seek(0, 0); seekErr != nil {
				return seekErr
			}
			resp, putErr := c.client.Object.Put(ctx, remoteKey, f, nil)
			if putErr != nil {
				return putErr
			}
			etag = strings.Trim(resp.Header.Get("ETag"), `"`)
			return nil
		})
		return etag, err
	}

	// Large file — concurrent multipart upload (SDK handles splitting, retry per part).
	result, _, err := c.client.Object.Upload(ctx, remoteKey, localPath, nil)
	if err != nil {
		return "", fmt.Errorf("multipart upload %s: %w", localPath, err)
	}
	etag = strings.Trim(result.ETag, `"`)
	return etag, nil
}

// Head fetches metadata for a remote object without downloading it.
// Returns an info struct with Exists=false (no error) for 404.
func (c *COSClient) Head(ctx context.Context, remoteKey string) (*RemoteObjectInfo, error) {
	var info RemoteObjectInfo
	info.Key = remoteKey

	// HEAD does not retry on 404 — that is a definitive answer.
	resp, err := c.client.Object.Head(ctx, remoteKey, nil)
	if err != nil {
		// Treat HTTP 404 as "does not exist", not a retryable error.
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return &RemoteObjectInfo{Key: remoteKey, Exists: false}, nil
		}
		// For other errors, wrap and return.
		return nil, fmt.Errorf("head %s: %w", remoteKey, err)
	}

	info.Exists = true
	info.ETag = strings.Trim(resp.Header.Get("ETag"), `"`)

	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		if t, parseErr := http.ParseTime(lm); parseErr == nil {
			info.LastModified = t
		}
	}

	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, parseErr := strconv.ParseInt(cl, 10, 64); parseErr == nil {
			info.Size = n
		}
	}

	return &info, nil
}

// Download downloads a remote object to a local path.
// Writes to a temp file first, then atomically renames it.
func (c *COSClient) Download(ctx context.Context, remoteKey, localPath string) error {
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return fmt.Errorf("mkdir for download: %w", err)
	}

	tmpPath := localPath + ".cloudsync.tmp"

	err := c.withRetry(ctx, func() error {
		_, getErr := c.client.Object.GetToFile(ctx, remoteKey, tmpPath, nil)
		return getErr
	})

	if err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("download %s: %w", remoteKey, err)
	}

	// Windows requires removing destination before rename.
	if runtime.GOOS == "windows" {
		_ = os.Remove(localPath)
	}
	if err := os.Rename(tmpPath, localPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename downloaded file: %w", err)
	}
	return nil
}

// ListWithMeta returns all remote objects under prefix with their metadata.
func (c *COSClient) ListWithMeta(ctx context.Context, prefix string) ([]RemoteObjectInfo, error) {
	var objects []RemoteObjectInfo
	opt := &cos.BucketGetOptions{Prefix: prefix, MaxKeys: 1000}

	for {
		result, _, err := c.client.Bucket.Get(ctx, opt)
		if err != nil {
			return nil, err
		}
		for _, obj := range result.Contents {
			info := RemoteObjectInfo{
				Key:    obj.Key,
				ETag:   strings.Trim(obj.ETag, `"`),
				Size:   obj.Size,
				Exists: true,
			}
			if t, parseErr := time.Parse(time.RFC3339, obj.LastModified); parseErr == nil {
				info.LastModified = t
			}
			objects = append(objects, info)
		}
		if !result.IsTruncated {
			break
		}
		opt.Marker = result.NextMarker
	}
	return objects, nil
}

// Delete removes a remote object
func (c *COSClient) Delete(ctx context.Context, remoteKey string) error {
	return c.withRetry(ctx, func() error {
		_, err := c.client.Object.Delete(ctx, remoteKey)
		return err
	})
}

// Exists checks if a remote key exists
func (c *COSClient) Exists(ctx context.Context, remoteKey string) (bool, error) {
	var exists bool
	err := c.withRetry(ctx, func() error {
		ok, err := c.client.Object.IsExist(ctx, remoteKey)
		exists = ok
		return err
	})
	return exists, err
}

// List returns all object keys with the given prefix
func (c *COSClient) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	opt := &cos.BucketGetOptions{Prefix: prefix, MaxKeys: 1000}

	for {
		result, _, err := c.client.Bucket.Get(ctx, opt)
		if err != nil {
			return nil, err
		}
		for _, obj := range result.Contents {
			keys = append(keys, obj.Key)
		}
		if !result.IsTruncated {
			break
		}
		opt.Marker = result.NextMarker
	}
	return keys, nil
}

// withRetry runs fn up to maxRetries times with exponential backoff
func (c *COSClient) withRetry(ctx context.Context, fn func() error) error {
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		if i < maxRetries-1 {
			backoff := time.Duration(math.Pow(2, float64(i))) * 500 * time.Millisecond
			c.logger.Warn("COS operation failed, retrying",
				zap.Error(lastErr),
				zap.Int("attempt", i+1),
				zap.Duration("backoff", backoff),
			)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return fmt.Errorf("after %d retries: %w", maxRetries, lastErr)
}
