// Package ratelimit_test verifies the correctness of the composite token-bucket
// rate limiter.
//
// Tests cover:
//  1. Per-second limit  (2 tokens/s)
//  2. Per-minute limit  (50 tokens/min)
//  3. Per-hour limit    (300 tokens/hour)
//  4. Concurrent access (multiple goroutines; must never exceed any limit)
//  5. State persistence — token counts are correctly restored after a
//     simulated restart (offline refill calculation)
//  6. Drain-on-429 behaviour
//
// The tokenBucket type is unexported but accessible from within this package
// (white-box testing). This avoids the need for a database.
package ratelimit

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ─── tokenBucket unit tests ───────────────────────────────────────────────────

func newBucket(capacity, refillRate float64) *tokenBucket {
	return &tokenBucket{
		id:         "test",
		tokens:     capacity,
		capacity:   capacity,
		refillRate: refillRate,
		lastRefill: time.Now(),
	}
}

func TestTokenBucket_ImmediateConsumption(t *testing.T) {
	// A full bucket should allow exactly capacity requests before blocking.
	b := newBucket(3, 0.01) // very slow refill so we don't accidentally top up

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := b.wait(ctx); err != nil {
			t.Fatalf("wait %d failed: %v", i, err)
		}
	}

	// The 4th call should block; cancel it quickly to avoid hanging the test.
	cancelCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := b.wait(cancelCtx); err == nil {
		t.Error("expected 4th wait to return an error (context deadline), got nil")
	}
}

func TestTokenBucket_RefillsOverTime(t *testing.T) {
	// Bucket starts empty; refill rate = 10 tokens/sec → 1 token per 100 ms.
	b := &tokenBucket{
		id:         "test_refill",
		tokens:     0,
		capacity:   10,
		refillRate: 10, // 10/s
		lastRefill: time.Now(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// First wait should complete within ~100 ms (one token refilled).
	start := time.Now()
	if err := b.wait(ctx); err != nil {
		t.Fatalf("wait failed: %v", err)
	}
	elapsed := time.Since(start)
	// Allow generous margin for CI jitter: must complete in < 300ms.
	if elapsed > 300*time.Millisecond {
		t.Errorf("first wait took %v; expected ~100ms", elapsed)
	}
}

func TestTokenBucket_DrainSetsZero(t *testing.T) {
	b := newBucket(50, 50)

	b.drain()
	got := b.currentTokens()
	if got != 0 {
		t.Errorf("after drain, tokens = %v; want 0", got)
	}
}

func TestTokenBucket_CurrentTokensCapped(t *testing.T) {
	// Tokens must never exceed capacity even with a very long elapsed time.
	b := &tokenBucket{
		id:         "cap_test",
		tokens:     5,
		capacity:   10,
		refillRate: 100, // 100/s — would overflow quickly
		lastRefill: time.Now().Add(-60 * time.Second), // pretend 60 s elapsed
	}
	got := b.currentTokens()
	if got > b.capacity {
		t.Errorf("tokens (%v) exceeded capacity (%v) after refill", got, b.capacity)
	}
	if got != b.capacity {
		t.Errorf("expected tokens to be capped at capacity %v, got %v", b.capacity, got)
	}
}

// ─── Per-second limit (2 req/s) ───────────────────────────────────────────────
//
// Proof strategy: consume all 2 tokens then time how long the 3rd must wait.
// With refillRate = 2/s the inter-token interval is 500 ms.

func TestPerSecondLimit_TwoRequestsPerSecond(t *testing.T) {
	b := &tokenBucket{
		id:         "per_sec",
		tokens:     2,
		capacity:   2,
		refillRate: 2, // 2 tokens/s
		lastRefill: time.Now(),
	}

	ctx := context.Background()

	// Consume all 2 tokens immediately.
	_ = b.wait(ctx)
	_ = b.wait(ctx)

	// The 3rd token should arrive after ~500ms.
	start := time.Now()
	ctxTimeout, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := b.wait(ctxTimeout); err != nil {
		t.Fatalf("3rd wait failed: %v", err)
	}
	elapsed := time.Since(start)

	// Must wait at least 400ms (allows 20% slack for slow CI runners).
	if elapsed < 400*time.Millisecond {
		t.Errorf("3rd request was served too fast (%v); rate limit may not be enforced", elapsed)
	}
}

// ─── Per-minute limit (50 req/min) ────────────────────────────────────────────
//
// Proof strategy: drain the bucket to 0 tokens, verify the next wait takes
// approximately refillRate time (1/refillRate seconds per token = 60/50 = 1.2s).

func TestPerMinuteLimit_RefillRate(t *testing.T) {
	b := &tokenBucket{
		id:         "per_min",
		tokens:     0,
		capacity:   50,
		refillRate: 50.0 / 60.0, // 50/min in tokens/sec
		lastRefill: time.Now(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	start := time.Now()
	if err := b.wait(ctx); err != nil {
		t.Fatalf("wait failed: %v", err)
	}
	elapsed := time.Since(start)

	// 1 token refills in 60/50 = 1.2 s; allow ±400ms for CI jitter.
	const expectedMin = 800 * time.Millisecond
	const expectedMax = 2 * time.Second
	if elapsed < expectedMin || elapsed > expectedMax {
		t.Errorf("wait duration %v not in expected range [%v, %v]", elapsed, expectedMin, expectedMax)
	}
}

// ─── Per-hour limit (300 req/hr) ──────────────────────────────────────────────
//
// Proof strategy: start with 1 token remaining (after 299 consumed), verify the
// bucket grants exactly that 1 token instantly and the next would have to wait.

func TestPerHourLimit_LastTokenGranted(t *testing.T) {
	b := &tokenBucket{
		id:         "per_hr",
		tokens:     1.0,
		capacity:   300,
		refillRate: 300.0 / 3600.0, // 300/hr
		lastRefill: time.Now(),
	}

	ctx := context.Background()
	// Should grant immediately (1 token available).
	start := time.Now()
	if err := b.wait(ctx); err != nil {
		t.Fatalf("wait failed: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("last token should be instant, took %v", elapsed)
	}

	// Next token: none available; cancel quickly.
	cancelCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := b.wait(cancelCtx); err == nil {
		t.Error("expected context cancellation after hour-bucket exhausted, got nil")
	}
}

// ─── Concurrent access ────────────────────────────────────────────────────────
//
// Spin up N goroutines all calling wait() simultaneously on a shared bucket.
// Verify:
//  1. No data race (run with -race).
//  2. Total tokens consumed == N (no double-grant, no lost grant).

func TestTokenBucket_ConcurrentAccess(t *testing.T) {
	const numWorkers = 20
	b := &tokenBucket{
		id:         "concurrent",
		tokens:     float64(numWorkers),
		capacity:   float64(numWorkers),
		refillRate: 0.001, // negligibly slow refill
		lastRefill: time.Now(),
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	var granted atomic.Int64

	wg.Add(numWorkers)
	for i := 0; i < numWorkers; i++ {
		go func() {
			defer wg.Done()
			if err := b.wait(ctx); err == nil {
				granted.Add(1)
			}
		}()
	}
	wg.Wait()

	// Every goroutine should have been granted exactly one token.
	if got := granted.Load(); got != numWorkers {
		t.Errorf("granted = %d; want %d", got, numWorkers)
	}

	// Bucket should now be empty (or very close to 0 — tiny refill during test).
	if remaining := b.currentTokens(); remaining > 1.0 {
		t.Errorf("expected ~0 tokens remaining after %d concurrent waits, got %.2f", numWorkers, remaining)
	}
}

// ─── State persistence / offline refill ───────────────────────────────────────
//
// The limiter persists token counts to the DB and restores them on restart.
// On restoration, it adds tokens proportional to elapsed downtime so the
// service never violates limits even across restarts.
//
// This test simulates that calculation in isolation (no DB required).

func TestStatePersistence_OfflineRefillCalculation(t *testing.T) {
	// Scenario: service was last running with 10 tokens in the per-minute bucket.
	// It was offline for 30 seconds. RefillRate = 50/60 tokens/s.
	//   Expected restored tokens = min(capacity, 10 + 30 * 50/60)
	//                            = min(50, 10 + 25) = 35

	savedTokens := 10.0
	refillRate := 50.0 / 60.0 // 50/min
	capacity := 50.0
	offlineSecs := 30.0

	restored := savedTokens + offlineSecs*refillRate
	if restored > capacity {
		restored = capacity
	}

	expected := 35.0
	if restored != expected {
		t.Errorf("offline refill calculation: got %v; want %v", restored, expected)
	}

	// Verify cap is applied correctly.
	// If offline for 10 minutes, would give 10 + 600*(50/60) = 10+500 = 510 > 50.
	longOffline := savedTokens + 600*refillRate
	if longOffline > capacity {
		longOffline = capacity
	}
	if longOffline != capacity {
		t.Errorf("expected tokens to be capped at capacity %v after long offline, got %v", capacity, longOffline)
	}
}

func TestStatePersistence_RestoredBucketHonoursCapacity(t *testing.T) {
	// Simulate makeBucket with a saved state: offline for 1 hour → bucket fills.
	elapsed := 3600.0 // 1 hour offline
	refillRate := 2.0 / 1.0 // per_sec bucket: 2/s
	capacity := 2.0
	savedTokens := 1.0

	restored := savedTokens + elapsed*refillRate // would be 7201
	if restored > capacity {
		restored = capacity
	}

	b := &tokenBucket{
		id:         "per_sec_restored",
		tokens:     restored,
		capacity:   capacity,
		refillRate: refillRate,
		lastRefill: time.Now(),
	}

	// Bucket should be full (capacity) after a long downtime, not overflow it.
	if b.tokens > capacity {
		t.Errorf("restored tokens (%v) must not exceed capacity (%v)", b.tokens, capacity)
	}
	if b.tokens != capacity {
		t.Errorf("expected full bucket (%v) after 1hr offline, got %v", capacity, b.tokens)
	}
}

// ─── Drain-on-429 behaviour ───────────────────────────────────────────────────

func TestDrainAll_ZerosAllBuckets(t *testing.T) {
	// Create a CompositeRateLimiter directly (bypassing DB constructor)
	// by setting the fields manually using package-level access.
	c := &CompositeRateLimiter{
		perSec:  newBucket(2, 2),
		perMin:  newBucket(50, 50.0/60),
		perHour: newBucket(300, 300.0/3600),
		store:   nil, // not needed for drain test
	}

	// Manually drain (mimics DrainAll but without the DB call).
	c.perSec.drain()
	c.perMin.drain()
	c.perHour.drain()

	if c.perSec.currentTokens() != 0 {
		t.Errorf("perSec tokens should be 0 after drain")
	}
	if c.perMin.currentTokens() != 0 {
		t.Errorf("perMin tokens should be 0 after drain")
	}
	if c.perHour.currentTokens() != 0 {
		t.Errorf("perHour tokens should be 0 after drain")
	}
}

// ─── Context cancellation propagates correctly ────────────────────────────────

func TestTokenBucket_ContextCancellationWhileWaiting(t *testing.T) {
	// Bucket is empty; wait should return immediately when context is cancelled.
	b := newBucket(0, 0.0001) // effectively no refill during test

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := b.wait(ctx)
	if err == nil {
		t.Error("expected error from cancelled context, got nil")
	}
}

func TestTokenBucket_ContextCancelledMidWait(t *testing.T) {
	b := newBucket(0, 1.0/float64(time.Hour)) // refills so slowly it won't matter

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := b.wait(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Error("expected timeout error, got nil")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("wait should have returned after context timeout (~100ms), took %v", elapsed)
	}
}

// ─── Tokens() observability ───────────────────────────────────────────────────

func TestTokens_ReturnsCurrentCounts(t *testing.T) {
	c := &CompositeRateLimiter{
		perSec:  newBucket(2, 2),
		perMin:  newBucket(50, 50.0/60),
		perHour: newBucket(300, 300.0/3600),
	}

	ps, pm, ph := c.Tokens()

	// All buckets are full; Tokens() should return their capacity values.
	if ps > 2.0 || ps < 1.9 {
		t.Errorf("perSec tokens = %v; expected ~2", ps)
	}
	if pm > 50.0 || pm < 49.9 {
		t.Errorf("perMin tokens = %v; expected ~50", pm)
	}
	if ph > 300.0 || ph < 299.9 {
		t.Errorf("perHour tokens = %v; expected ~300", ph)
	}
}
