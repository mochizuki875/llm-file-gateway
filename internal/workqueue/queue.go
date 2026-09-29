// Package workqueue provides context-aware FIFO work queues.
package workqueue

import (
	"context"
	"sync"
)

// Queue stores work items until a consumer can process them.
type Queue[T any] interface {
	Add(context.Context, T) bool
	// Get blocks until an item is available or the context is cancelled.
	// The quit result is true when the caller should stop consuming work.
	Get(context.Context) (T, bool)
}

// New constructs a FIFO queue. A capacity of zero creates an unbounded queue.
func New[T any](capacity int) Queue[T] {
	if capacity > 0 {
		return &boundedQueue[T]{items: make(chan T, capacity)}
	}
	return newUnboundedQueue[T]()
}

type boundedQueue[T any] struct {
	items chan T
}

func (queue *boundedQueue[T]) Add(ctx context.Context, item T) bool {
	select {
	case queue.items <- item:
		return true
	case <-ctx.Done():
		return false
	}
}

func (queue *boundedQueue[T]) Get(ctx context.Context) (T, bool) {
	select {
	case <-ctx.Done():
		var zero T
		return zero, true
	case item := <-queue.items:
		return item, false
	}
}

type unboundedQueue[T any] struct {
	mu    sync.Mutex
	items []T
	ready chan struct{}
}

func newUnboundedQueue[T any]() *unboundedQueue[T] {
	return &unboundedQueue[T]{ready: make(chan struct{})}
}

func (queue *unboundedQueue[T]) Add(ctx context.Context, item T) bool {
	select {
	case <-ctx.Done():
		return false
	default:
	}
	queue.mu.Lock()
	queue.items = append(queue.items, item)
	close(queue.ready)
	queue.ready = make(chan struct{})
	queue.mu.Unlock()
	return true
}

func (queue *unboundedQueue[T]) Get(ctx context.Context) (T, bool) {
	for {
		queue.mu.Lock()
		if len(queue.items) > 0 {
			item := queue.items[0]
			var zero T
			queue.items[0] = zero
			queue.items = queue.items[1:]
			queue.mu.Unlock()
			return item, false
		}
		ready := queue.ready
		queue.mu.Unlock()
		select {
		case <-ctx.Done():
			var zero T
			return zero, true
		case <-ready:
		}
	}
}
