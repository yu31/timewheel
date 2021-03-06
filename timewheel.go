// Copyright (c) 2020, Yu Wu <yu.771991@gmail.com> All rights reserved.
//
// Use of this source code is governed by a MIT-style license that can be
// found in the LICENSE file.

package timewheel

import (
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/yu31/dqueue"
)

const (
	defaultTick = time.Millisecond
	defaultSize = int64(32)
)

// TimeWheel is an implementation of Hierarchical Timing Wheels.
type TimeWheel struct {
	tick     int64 // in nanoseconds.
	size     int64 // TimeWheel Size.
	interval int64 // in nanoseconds.
	current  int64 // in nanoseconds.

	buckets []*bucket
	queue   *dqueue.DQueue

	// The higher-level overflow TimeWheel.
	//
	// NOTICE: This field may be updated and read concurrently, through tw.add().
	overflow unsafe.Pointer // type: *TimingWheel
}

// Default creates an TimeWheel with default parameters.
func Default() *TimeWheel {
	return New(defaultTick, defaultSize)
}

// New creates an TimeWheel with the given tick and wheel size.
// The value of tick must >= 1ms, the size must >= 1.
func New(tick time.Duration, size int64) *TimeWheel {
	if tick < time.Millisecond {
		panic("timewheel: tick must be greater than or equal to 1ms")
	}
	if size < 1 {
		panic("timewheel: size must be greater than 0")
	}
	return newTimeWheel(int64(tick), size, time.Now().UnixNano(), dqueue.Default())
}

// truncate returns the result of rounding x toward zero to a multiple of m.
// If m <= 0, Truncate returns x unchanged.
func truncate(x, m int64) int64 {
	if m <= 0 {
		return x
	}
	return x - x%m
}

// newTimeWheel is an internal helper function that really creates an TimeWheel.
func newTimeWheel(tick int64, size int64, start int64, queue *dqueue.DQueue) *TimeWheel {
	return &TimeWheel{
		tick:     tick,
		size:     size,
		interval: tick * size,
		current:  truncate(start, tick),
		buckets:  createBuckets(int(size)),
		queue:    queue,
		overflow: nil,
	}
}

// Start starts the current time wheel in a goroutine.
// You can call the Wait method to blocks the main process after.
func (tw *TimeWheel) Start() {
	tw.queue.Consume(tw.process)
}

// Stop stops the current time wheel.
//
// If there is any timer's task being running in its own goroutine, Stop does
// not wait for the task to complete before returning. If the caller needs to
// know whether the task is completed, it must coordinate with the task explicitly.
func (tw *TimeWheel) Stop() {
	tw.queue.Close()
}

// advance push the clock forward.
func (tw *TimeWheel) advance(expiration int64) {
	current := atomic.LoadInt64(&tw.current)
	if expiration >= current+tw.tick {
		current = truncate(expiration, tw.tick)
		atomic.StoreInt64(&tw.current, current)

		// Try to advance the clock of the overflow wheel if present
		overflow := atomic.LoadPointer(&tw.overflow)
		if overflow != nil {
			(*TimeWheel)(overflow).advance(current)
		}
	}
}

// process the expiration's bucket
func (tw *TimeWheel) process(msg *dqueue.Message) {
	b := msg.Value.(*bucket)
	tw.advance(b.getExpiration())

	b.flush(tw.submit)
}

// submit inserts the timer t into the current timing wheel, or run the
// timer's task if it has been expired.
func (tw *TimeWheel) submit(t *Timer) {
	if !tw.add(t) {
		t.task()
	}
}

// add inserts the timer t into the current timing wheel.
// return false means the Timer has been expired.
func (tw *TimeWheel) add(t *Timer) bool {
	current := atomic.LoadInt64(&tw.current)
	if t.expiration < current+tw.tick {
		// Already expired.
		return false
	} else if t.expiration < current+tw.interval {
		// Put it into its own bucket.
		virtualID := t.expiration / tw.tick
		b := tw.buckets[virtualID%tw.size]
		b.insert(t)

		// Set the bucket expiration timestamp.
		if b.setExpiration(virtualID * tw.tick) {
			// The bucket needs to be enqueued since it was an expired bucket.
			// We only need to enqueue the bucket when its expiration time has changed,
			// i.e. the wheel has advanced and this bucket get reused with a new expiration.
			// Any further calls to set the expiration within the same wheel cycle will
			// pass in the same value and hence return false, thus the bucket with the
			// same expiration will not be enqueued multiple times.
			tw.queue.Expire(b.getExpiration(), b)
		}
		return true
	} else {
		// Out of the interval. Put it into the overflow TimeWheel.
		var overflow unsafe.Pointer

		overflow = atomic.LoadPointer(&tw.overflow)
		if overflow == nil {
			// Creates and save overflow TimeWheel.
			ntw := newTimeWheel(tw.interval, tw.size, current, tw.queue)
			atomic.CompareAndSwapPointer(&tw.overflow, nil, unsafe.Pointer(ntw))

			// Load safe to avoid concurrent operations.
			overflow = atomic.LoadPointer(&tw.overflow)
		}

		return (*TimeWheel)(overflow).add(t)
	}
}
