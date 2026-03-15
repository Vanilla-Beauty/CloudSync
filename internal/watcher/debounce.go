package watcher

import (
	"sync"
	"time"
)

// Debouncer delays callback execution until no new triggers arrive within the delay window
type Debouncer struct {
	mu       sync.Mutex
	timers   map[string]*time.Timer
	callback func(string)
	delay    time.Duration
}

// NewDebouncer creates a new Debouncer
func NewDebouncer(delay time.Duration, callback func(string)) *Debouncer {
	return &Debouncer{
		timers:   make(map[string]*time.Timer),
		callback: callback,
		delay:    delay,
	}
}

// Trigger schedules or resets the debounce timer for filePath
func (d *Debouncer) Trigger(filePath string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if t, ok := d.timers[filePath]; ok {
		// Stop and drain to avoid double-fire
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
		t.Reset(d.delay)
		return
	}

	d.timers[filePath] = time.AfterFunc(d.delay, func() {
		d.mu.Lock()
		delete(d.timers, filePath)
		d.mu.Unlock()
		d.callback(filePath)
	})
}

// Cancel removes the pending timer for filePath without firing the callback
func (d *Debouncer) Cancel(filePath string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if t, ok := d.timers[filePath]; ok {
		t.Stop()
		delete(d.timers, filePath)
	}
}

// Close cancels all pending timers
func (d *Debouncer) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()

	for path, t := range d.timers {
		t.Stop()
		delete(d.timers, path)
	}
}
