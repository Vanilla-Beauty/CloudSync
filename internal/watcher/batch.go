package watcher

import (
	"context"
	"sync"
	"time"
)

// Batcher collects file paths and fires the callback in bulk
type Batcher struct {
	mu       sync.Mutex
	batch    map[string]struct{}
	maxSize  int
	callback func([]string)
	ticker   *time.Ticker
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
}

// NewBatcher creates and starts a Batcher
func NewBatcher(maxSize int, interval time.Duration, callback func([]string)) *Batcher {
	ctx, cancel := context.WithCancel(context.Background())
	b := &Batcher{
		batch:    make(map[string]struct{}),
		maxSize:  maxSize,
		callback: callback,
		ticker:   time.NewTicker(interval),
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	go b.run()
	return b
}

// Add queues a file path for the next batch, flushing immediately if maxSize is reached
func (b *Batcher) Add(filePath string) {
	b.mu.Lock()
	b.batch[filePath] = struct{}{}
	shouldFlush := len(b.batch) >= b.maxSize
	b.mu.Unlock()

	if shouldFlush {
		b.Flush()
	}
}

// Flush drains the current batch and fires the callback
func (b *Batcher) Flush() {
	b.mu.Lock()
	if len(b.batch) == 0 {
		b.mu.Unlock()
		return
	}
	snapshot := make([]string, 0, len(b.batch))
	for p := range b.batch {
		snapshot = append(snapshot, p)
	}
	b.batch = make(map[string]struct{})
	b.mu.Unlock()

	b.callback(snapshot)
}

// Close stops the background goroutine after flushing any pending items
func (b *Batcher) Close() {
	b.cancel()
	b.ticker.Stop()
	<-b.done
	b.Flush()
}

func (b *Batcher) run() {
	defer close(b.done)
	for {
		select {
		case <-b.ticker.C:
			b.Flush()
		case <-b.ctx.Done():
			return
		}
	}
}
