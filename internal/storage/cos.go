package storage

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/cloudsync/cloudsync/internal/config"
	cos "github.com/tencentyun/cos-go-sdk-v5"
	"go.uber.org/zap"
)

// BucketInfo contains the name and region of a COS bucket.
type BucketInfo struct {
	Name   string
	Region string
}

// ListBuckets returns all buckets accessible with the given credentials.
// It does not require a pre-existing COSClient since it uses the service-level endpoint.
func ListBuckets(ctx context.Context, secretID, secretKey string) ([]BucketInfo, error) {
	serviceURL, _ := url.Parse("https://service.cos.myqcloud.com")
	b := &cos.BaseURL{ServiceURL: serviceURL}
	client := cos.NewClient(b, &http.Client{
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
		buckets = append(buckets, BucketInfo{Name: b.Name, Region: b.Region})
	}
	return buckets, nil
}

const maxRetries = 3

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

// Upload uploads a local file to COS with exponential backoff retry.
// The file is opened once before entering the retry loop so that a
// non-existent or permission-denied path fails immediately without burning
// retry budget. io.NopCloser prevents the HTTP transport from closing the
// underlying *os.File between attempts, allowing Seek(0,0) to rewind it.
func (c *COSClient) Upload(ctx context.Context, localPath, remoteKey string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", localPath, err)
	}
	defer f.Close()

	return c.withRetry(ctx, func() error {
		if _, err := f.Seek(0, 0); err != nil {
			return fmt.Errorf("seek %s: %w", localPath, err)
		}
		_, err := c.client.Object.Put(ctx, remoteKey, io.NopCloser(f), nil)
		return err
	})
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

// Download downloads a remote object to a local file path.
func (c *COSClient) Download(ctx context.Context, remoteKey, localPath string) error {
	return c.withRetry(ctx, func() error {
		resp, err := c.client.Object.Get(ctx, remoteKey, nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
			return fmt.Errorf("create dirs for %s: %w", localPath, err)
		}

		f, err := os.Create(localPath)
		if err != nil {
			return fmt.Errorf("create %s: %w", localPath, err)
		}
		defer f.Close()

		buf := make([]byte, 32*1024)
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				if _, writeErr := f.Write(buf[:n]); writeErr != nil {
					return fmt.Errorf("write %s: %w", localPath, writeErr)
				}
			}
			if readErr != nil {
				if readErr == io.EOF {
					break
				}
				return fmt.Errorf("read response body: %w", readErr)
			}
		}
		return nil
	})
}

// DirEntry represents one entry returned by ListDir.
type DirEntry struct {
	Key          string // full key (files only)
	Prefix       string // full prefix ending with "/" (directories only)
	Size         int64
	LastModified string
	IsDir        bool
}

// ListDir lists one level under prefix using delimiter="/".
// Returns subdirectory prefixes and object entries.
func (c *COSClient) ListDir(ctx context.Context, prefix string) ([]DirEntry, error) {
	var entries []DirEntry
	opt := &cos.BucketGetOptions{
		Prefix:    prefix,
		Delimiter: "/",
		MaxKeys:   1000,
	}
	for {
		result, _, err := c.client.Bucket.Get(ctx, opt)
		if err != nil {
			return nil, fmt.Errorf("list dir %q: %w", prefix, err)
		}
		for _, p := range result.CommonPrefixes {
			entries = append(entries, DirEntry{Prefix: p, IsDir: true})
		}
		for _, obj := range result.Contents {
			entries = append(entries, DirEntry{
				Key:          obj.Key,
				Size:         obj.Size,
				LastModified: obj.LastModified,
				IsDir:        false,
			})
		}
		if !result.IsTruncated {
			break
		}
		opt.Marker = result.NextMarker
	}
	return entries, nil
}

// NewCOSClientForBucket creates a temporary COSClient for the given bucket+region
// without requiring a MetadataStore. Useful for read-only browsing operations.
func NewCOSClientForBucket(secretID, secretKey, bucket, region string) (*COSClient, error) {
	cfg := &config.COSConfig{
		SecretID:  secretID,
		SecretKey: secretKey,
		Bucket:    bucket,
		Region:    region,
	}
	return NewCOSClient(cfg, nil, zap.NewNop())
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
