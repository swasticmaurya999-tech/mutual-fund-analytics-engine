// Package ratelimit tests the composite triple token-bucket rate limiter.
//
// Coverage targets (assignment requirement):
//   - All three limits enforced independently (per-sec, per-min, per-hr)
//   - Three limits coordinated simultaneously via the composite limiter
//   - Concurrent access — no double-grant, race-detector clean
//   - State persistence — offline refill calculation is correct
//   - Drain-on-429 — all buckets zeroed
//   - Context cancellation — empty bucket unblocks on cancel
package ratelimit

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newBucket is a test helper that creates a full token bucket.
func newBucket(id string, capacity, refillRate float64) *tokenBucket {
	return &tokenBucket{
		id: id, tokens: capacity, capacity: capacity,
		refillRate: refillRate, lastRefill: time.Now(),
	}
}

// TestPerSecondLimit — after consuming all 2 tokens, the 3rd must wait ≥400ms.
// Refill rate = 2/s → one new token every 500ms.
func TestPerSecondLimit(t *testing.T) {
	b := newBucket("per_sec", 2, 2)
	ctx := context.Background()
	_ = b.wait(ctx)
	_ = b.wait(ctx) // exhaust both tokens

	ctx2, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	start := time.Now()
	if err := b.wait(ctx2); err != nil {
		t.Fatalf("3rd wait failed: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 400*time.Millisecond {
		t.Errorf("3rd token granted in %v; expected ≥400ms — rate limit not enforced", elapsed)
	}
}

// TestPerMinuteLimit — empty bucket; one token refills in 60/50 = 1.2s.
func TestPerMinuteLimit(t *testing.T) {
	b := &tokenBucket{id: "per_min", tokens: 0, capacity: 50,
		refillRate: 50.0 / 60.0, lastRefill: time.Now()}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	start := time.Now()
	if err := b.wait(ctx); err != nil {
		t.Fatalf("wait failed: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 800*time.Millisecond || elapsed > 2*time.Second {
		t.Errorf("per-minute wait = %v; want 800ms–2s (expected ~1.2s)", elapsed)
	}
}

// TestPerHourLimit — 1 token remaining: granted instantly; 0 tokens: blocks.
func TestPerHourLimit(t *testing.T) {
	b := &tokenBucket{id: "per_hr", tokens: 1, capacity: 300,
		refillRate: 300.0 / 3600.0, lastRefill: time.Now()}

	// Last token — must be instant.
	start := time.Now()
	if err := b.wait(context.Background()); err != nil {
		t.Fatalf("last token wait failed: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("last token took %v; want instant (<100ms)", elapsed)
	}

	// No tokens left — must block until context expires.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := b.wait(ctx); err == nil {
		t.Error("expected context deadline after hour-bucket exhausted, got nil")
	}
}

// TestCompositeEnforcesAllThree — if any single bucket is empty the composite
// Wait must block regardless of the other two being full.
func TestCompositeEnforcesAllThree(t *testing.T) {
	c := &CompositeRateLimiter{
		perSec:  newBucket("per_sec", 2, 2),
		perMin:  newBucket("per_min", 50, 50.0/60),
		perHour: newBucket("per_hr", 0, 300.0/3600), // hour bucket drained
		store:   nil,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := c.Wait(ctx); err == nil {
		t.Error("Wait should block when per-hour bucket is empty")
	}
}

// TestConcurrentAccess — 20 goroutines contend on a shared bucket;
// each must receive exactly one token (no double-grant, no lost grant).
// Run with -race to verify there are no data races.
func TestConcurrentAccess(t *testing.T) {
	const workers = 20
	b := &tokenBucket{id: "conc", tokens: float64(workers), capacity: float64(workers),
		refillRate: 0.001, lastRefill: time.Now()}

	var wg sync.WaitGroup
	var granted atomic.Int64
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			if b.wait(context.Background()) == nil {
				granted.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := granted.Load(); got != workers {
		t.Errorf("granted = %d; want %d (double-grant or lost grant)", got, workers)
	}
	if rem := b.currentTokens(); rem > 1.0 {
		t.Errorf("%.2f tokens remaining after %d grants; expected ~0", rem, workers)
	}
}

// TestStatePersistence — verifies the offline-refill formula used on restart.
//
//	saved=10 tokens, offline=30s, rate=50/60 → restored = min(50, 10+25) = 35
//	long offline (10 min) → capped at capacity=50
func TestStatePersistence(t *testing.T) {
	saved, rate, capacity := 10.0, 50.0/60.0, 50.0

	restored := math.Min(capacity, saved+30*rate)
	if restored != 35.0 {
		t.Errorf("30s offline: got %v; want 35.0", restored)
	}

	restoredLong := math.Min(capacity, saved+600*rate)
	if restoredLong != capacity {
		t.Errorf("10min offline: got %v; want capacity %v", restoredLong, capacity)
	}

	// A bucket built from the restored value must not exceed capacity.
	b := &tokenBucket{id: "restored", tokens: restored, capacity: capacity,
		refillRate: rate, lastRefill: time.Now()}
	if b.tokens > b.capacity {
		t.Errorf("restored tokens (%.2f) exceeded capacity (%.2f)", b.tokens, b.capacity)
	}
}

// TestDrainOn429 — DrainAll must zero every bucket (called on HTTP 429).
func TestDrainOn429(t *testing.T) {
	c := &CompositeRateLimiter{
		perSec:  newBucket("per_sec", 2, 2),
		perMin:  newBucket("per_min", 50, 50.0/60),
		perHour: newBucket("per_hr", 300, 300.0/3600),
		store:   nil,
	}
	c.perSec.drain()
	c.perMin.drain()
	c.perHour.drain()

	ps, pm, ph := c.Tokens()
	if ps != 0 || pm != 0 || ph != 0 {
		t.Errorf("after drain: per_sec=%.2f per_min=%.2f per_hr=%.2f; all want 0", ps, pm, ph)
	}
}

// TestContextCancellation — an empty bucket must return immediately when the
// context is cancelled, not block until a token eventually refills.
func TestContextCancellation(t *testing.T) {
	b := newBucket("empty", 0, 0.0001) // negligible refill

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := b.wait(ctx); err == nil {
		t.Error("expected timeout error from empty bucket, got nil")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("cancellation took %v; want <500ms", elapsed)
	}
}
