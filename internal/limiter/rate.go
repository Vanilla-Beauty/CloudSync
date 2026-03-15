package limiter

import (
	"context"

	"golang.org/x/time/rate"
)

// RateLimiter combines a semaphore for concurrency and a token bucket for QPS
type RateLimiter struct {
	sem     chan struct{}
	limiter *rate.Limiter
}

// NewRateLimiter creates a RateLimiter with the given max concurrency and QPS
func NewRateLimiter(maxConcurrent int, qps float64) *RateLimiter {
	return &RateLimiter{
		sem:     make(chan struct{}, maxConcurrent),
		limiter: rate.NewLimiter(rate.Limit(qps), maxConcurrent),
	}
}

// Acquire blocks until both a concurrency slot and a token are available
func (rl *RateLimiter) Acquire(ctx context.Context) error {
	if err := rl.limiter.Wait(ctx); err != nil {
		return err
	}
	select {
	case rl.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release frees a concurrency slot
func (rl *RateLimiter) Release() {
	<-rl.sem
}
