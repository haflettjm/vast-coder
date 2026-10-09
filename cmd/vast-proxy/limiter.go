package main

import (
	"context"
	"errors"
	"sync"
)

var errQueueFull = errors.New("queue full")

// limiter admits at most max concurrent holders. Waiters are served strictly
// first in, first out, the wait queue is bounded, and a waiter whose context ends
// (for example a client that disconnected) leaves the queue without ever running.
type limiter struct {
	mu       sync.Mutex
	max      int
	maxQueue int
	inflight int
	queue    []*waiter
}

type waiter struct {
	ready   chan struct{}
	granted bool
}

func newLimiter(max, maxQueue int) *limiter {
	return &limiter{max: max, maxQueue: maxQueue}
}

// acquire blocks until a slot is free, the queue is full, or ctx ends.
// The returned release function is safe to call more than once.
func (l *limiter) acquire(ctx context.Context) (func(), error) {
	l.mu.Lock()
	if l.inflight < l.max && len(l.queue) == 0 {
		l.inflight++
		l.mu.Unlock()
		return l.releaser(), nil
	}
	if len(l.queue) >= l.maxQueue {
		l.mu.Unlock()
		return nil, errQueueFull
	}
	w := &waiter{ready: make(chan struct{})}
	l.queue = append(l.queue, w)
	l.mu.Unlock()

	select {
	case <-w.ready:
		return l.releaser(), nil
	case <-ctx.Done():
		l.mu.Lock()
		if w.granted {
			// The slot was handed over just as the context ended. Give it back.
			l.mu.Unlock()
			l.release()
			return nil, ctx.Err()
		}
		for i, q := range l.queue {
			if q == w {
				l.queue = append(l.queue[:i], l.queue[i+1:]...)
				break
			}
		}
		l.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (l *limiter) releaser() func() {
	var once sync.Once
	return func() { once.Do(l.release) }
}

func (l *limiter) release() {
	l.mu.Lock()
	l.inflight--
	for l.inflight < l.max && len(l.queue) > 0 {
		w := l.queue[0]
		l.queue = l.queue[1:]
		w.granted = true
		l.inflight++
		close(w.ready)
	}
	l.mu.Unlock()
}

func (l *limiter) stats() (inflight, queued int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inflight, len(l.queue)
}
