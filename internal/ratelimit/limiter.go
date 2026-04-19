// Package ratelimit implements a composite triple token bucket rate limiter.
// It enforces three concurrent limits simultaneously:
//   - 2 requests/second
//   - 50 requests/minute
//   - 300 requests/hour
//
// Every outbound API call must acquire a token from ALL THREE buckets before
// proceeding. The most restrictive bucket at any point in time controls the
// effective throughput.
//
// State is persisted to PostgreSQL on every token consumption, ensuring rate
// limits are never violated across service restarts.
package ratelimit

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"mutualfundanalysis/internal/models"
	"mutualfundanalysis/internal/store"
)

// tokenBucket is a single token bucket with continuous refill.
type tokenBucket struct {
	mu         sync.Mutex
	id         string
	tokens     float64
	capacity   float64
	refillRate float64 // tokens per second
	lastRefill time.Time
}

// refill adds tokens proportional to elapsed time since last refill.
// Must be called with mu held.
func (b *tokenBucket) refill() {
	now := time.Now()
	elapsed := now.Sub(b.lastRefill).Seconds()
	b.tokens = math.Min(b.capacity, b.tokens+elapsed*b.refillRate)
	b.lastRefill = now
}

// wait blocks until a token is available or the context is cancelled.
func (b *tokenBucket) wait(ctx context.Context) error {
	for {
		b.mu.Lock()
		b.refill()
		if b.tokens >= 1.0 {
			b.tokens--
			b.mu.Unlock()
			return nil
		}
		// How long until the next token arrives?
		waitDuration := time.Duration((1.0-b.tokens)/b.refillRate*1000) * time.Millisecond
		b.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitDuration):
			// try again
		}
	}
}

// currentTokens returns the current (refilled) token count.
func (b *tokenBucket) currentTokens() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill()
	return b.tokens
}

// drain sets tokens to 0 — called when a 429 is received.
func (b *tokenBucket) drain() {
	b.mu.Lock()
	b.tokens = 0
	b.lastRefill = time.Now()
	b.mu.Unlock()
}

// CompositeRateLimiter wraps three token buckets and enforces all simultaneously.
type CompositeRateLimiter struct {
	perSec  *tokenBucket
	perMin  *tokenBucket
	perHour *tokenBucket
	store   *store.Store
	log     *slog.Logger
}

// New creates a CompositeRateLimiter, restoring token counts from the database
// to account for any time elapsed since the last save. This ensures the rate
// limits are never violated across restarts.
func New(ctx context.Context, st *store.Store, log *slog.Logger) (*CompositeRateLimiter, error) {
	states, err := st.LoadAllRateLimiterStates(ctx)
	if err != nil {
		return nil, fmt.Errorf("load rate limiter states: %w", err)
	}

	// Build a lookup map from DB records
	stateMap := make(map[string]*models.RateLimiterState, 3)
	for _, s := range states {
		stateMap[s.LimiterID] = s
	}

	makeBucket := func(id string) *tokenBucket {
		st, ok := stateMap[id]
		if !ok {
			// Fallback defaults if DB row is missing
			defaults := map[string][2]float64{
				"per_sec": {2, 2},
				"per_min": {50.0 / 60, 50},
				"per_hr":  {300.0 / 3600, 300},
			}
			d := defaults[id]
			return &tokenBucket{id: id, tokens: d[1], capacity: d[1], refillRate: d[0], lastRefill: time.Now()}
		}

		// Restore tokens: add what would have refilled during downtime
		elapsed := time.Since(st.LastUpdated).Seconds()
		restored := math.Min(st.Capacity, st.Tokens+elapsed*st.RefillRate)
		log.Info("rate limiter restored",
			"limiter", id,
			"saved_tokens", st.Tokens,
			"elapsed_sec", math.Round(elapsed),
			"restored_tokens", math.Round(restored*100)/100,
		)
		return &tokenBucket{
			id:         id,
			tokens:     restored,
			capacity:   st.Capacity,
			refillRate: st.RefillRate,
			lastRefill: time.Now(),
		}
	}

	return &CompositeRateLimiter{
		perSec:  makeBucket("per_sec"),
		perMin:  makeBucket("per_min"),
		perHour: makeBucket("per_hr"),
		store:   st,
		log:     log,
	}, nil
}

// Wait blocks until all three token buckets grant a token, then immediately
// persists the updated counts to the database. Using context.Background() for
// the persist ensures the write always completes even if the caller's context
// is cancelled right after tokens are consumed.
func (c *CompositeRateLimiter) Wait(ctx context.Context) error {
	// Acquire in order: slowest refill first to fail fast on exhausted budgets
	if err := c.perHour.wait(ctx); err != nil {
		return fmt.Errorf("per-hour rate limit wait: %w", err)
	}
	if err := c.perMin.wait(ctx); err != nil {
		return fmt.Errorf("per-minute rate limit wait: %w", err)
	}
	if err := c.perSec.wait(ctx); err != nil {
		return fmt.Errorf("per-second rate limit wait: %w", err)
	}
	c.persist(context.Background())
	return nil
}

// DrainAll sets all bucket token counts to zero and persists to DB.
// Called when a HTTP 429 is received to force a full cooldown.
func (c *CompositeRateLimiter) DrainAll(ctx context.Context) {
	c.perSec.drain()
	c.perMin.drain()
	c.perHour.drain()
	if err := c.store.DrainAllRateLimiters(ctx); err != nil {
		c.log.Warn("failed to drain rate limiter state in DB", "error", err)
	}
	c.log.Warn("rate limiter drained after HTTP 429")
}

// Tokens returns current token counts for observability.
func (c *CompositeRateLimiter) Tokens() (perSec, perMin, perHour float64) {
	return c.perSec.currentTokens(), c.perMin.currentTokens(), c.perHour.currentTokens()
}

// persist saves current token counts to the database.
func (c *CompositeRateLimiter) persist(ctx context.Context) {
	for _, b := range []*tokenBucket{c.perSec, c.perMin, c.perHour} {
		t := b.currentTokens()
		if err := c.store.SaveRateLimiterState(ctx, b.id, t); err != nil {
			c.log.Warn("failed to persist rate limiter state", "limiter", b.id, "error", err)
		}
	}
}

