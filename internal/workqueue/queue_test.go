package workqueue

import (
	"context"
	"testing"
	"time"
)

func TestUnboundedQueuePreservesOrder(t *testing.T) {
	queue := New[string](0)
	for _, item := range []string{"first", "second"} {
		if !queue.Add(context.Background(), item) {
			t.Fatalf("Add(%q) = false, want true", item)
		}
	}

	for _, want := range []string{"first", "second"} {
		item, quit := queue.Get(context.Background())
		if quit || item != want {
			t.Fatalf("Get() = %q, %t; want %q, false", item, quit, want)
		}
	}
}

func TestQueueRejectsCancelledAdd(t *testing.T) {
	queue := New[string](0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if queue.Add(ctx, "cancelled") {
		t.Fatal("Add() = true with cancelled context")
	}
}

func TestBoundedQueueBlocksAtCapacity(t *testing.T) {
	queue := New[string](1)
	if !queue.Add(context.Background(), "first") {
		t.Fatal("first Add() = false")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if queue.Add(ctx, "second") {
		t.Fatal("Add() succeeded while queue was full")
	}

	item, quit := queue.Get(context.Background())
	if quit || item != "first" {
		t.Fatalf("Get() = %q, %t; want first, false", item, quit)
	}
}

func TestGetReturnsWhenContextCancelled(t *testing.T) {
	queue := New[string](0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if item, quit := queue.Get(ctx); !quit || item != "" {
		t.Fatalf("Get() = %q, %t; want zero value, true", item, quit)
	}
}
