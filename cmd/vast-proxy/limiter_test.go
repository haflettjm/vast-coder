package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestLimiterCapsConcurrencyAndIsFIFO(t *testing.T) {
	l := newLimiter(1, 8)
	first, err := l.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			release, err := l.acquire(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			release()
		}(i)
		waitFor(t, func() bool { _, q := l.stats(); return q == i+1 }) // enqueue in a known order
	}
	if in, q := l.stats(); in != 1 || q != 3 {
		t.Fatalf("inflight=%d queued=%d, want 1 and 3", in, q)
	}
	first()
	wg.Wait()
	for i, v := range order {
		if v != i {
			t.Fatalf("order %v is not first in, first out", order)
		}
	}
}

func TestLimiterQueueFull(t *testing.T) {
	l := newLimiter(1, 1)
	hold, _ := l.acquire(context.Background())
	defer hold()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _ = l.acquire(ctx) }()
	waitFor(t, func() bool { _, q := l.stats(); return q == 1 })
	if _, err := l.acquire(context.Background()); !errors.Is(err, errQueueFull) {
		t.Fatalf("got %v, want errQueueFull", err)
	}
}

func TestLimiterCancelledWaiterLeavesQueueWithoutRunning(t *testing.T) {
	l := newLimiter(1, 4)
	hold, _ := l.acquire(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := l.acquire(ctx); errc <- err }()
	waitFor(t, func() bool { _, q := l.stats(); return q == 1 })
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if _, q := l.stats(); q != 0 {
		t.Fatalf("cancelled waiter still queued (%d)", q)
	}
	hold()
	if in, _ := l.stats(); in != 0 {
		t.Fatalf("slot leaked: inflight=%d", in)
	}
	again, err := l.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	again()
}

func TestLimiterReleaseIsIdempotent(t *testing.T) {
	l := newLimiter(1, 1)
	release, _ := l.acquire(context.Background())
	release()
	release()
	if in, _ := l.stats(); in != 0 {
		t.Fatalf("inflight=%d after double release", in)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not reached in time")
}
