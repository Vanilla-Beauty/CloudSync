package storage

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/cloudsync/cloudsync/internal/config"
	cos "github.com/tencentyun/cos-go-sdk-v5"
	"go.uber.org/zap"
)

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

// Upload uploads a local file to COS with exponential backoff retry
func (c *COSClient) Upload(ctx context.Context, localPath, remoteKey string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", localPath, err)
	}
	defer f.Close()

	return c.withRetry(ctx, func() error {
		// Reset file position on retry
		if _, err := f.Seek(0, 0); err != nil {
			return err
		}
		_, err := c.client.Object.Put(ctx, remoteKey, f, nil)
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
