package server

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestDiscoveryLoopRescansAfterFailureAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var passes atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		runDiscoveryLoop(ctx, time.Millisecond, func(context.Context) error {
			switch passes.Add(1) {
			case 1:
				return errors.New("fixture scan failure")
			case 3:
				cancel()
			}
			return nil
		})
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("discovery did not stop")
	}
	if passes.Load() != 3 {
		t.Fatalf("passes = %d, want 3", passes.Load())
	}
}
func TestDiscoveryLoopDoesNotOverlapSlowPasses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		runDiscoveryLoop(ctx, time.Millisecond, func(context.Context) error {
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no immediate scan")
	}
	select {
	case <-entered:
		t.Fatal("overlapping scan")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no later scan")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loop leaked")
	}
}
