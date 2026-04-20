// Package client provides an HTTP client for the mfapi.in mutual fund API.
// It integrates with the rate limiter and logs every request to request_log.
// All requests are retried with exponential backoff on transient failures.
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"sort"
	"strconv"
	"time"

	"mutualfundanalysis/internal/models"
	"mutualfundanalysis/internal/ratelimit"
	"mutualfundanalysis/internal/store"
)

const (
	baseURL    = "https://api.mfapi.in"
	maxRetries = 5
	// Wait after receiving a 429 before resuming.
	// The API imposes a 5-minute block on per-minute violations,
	// so we wait 65s to clear the per-minute window.
	cooldownAfter429 = 65 * time.Second
)

// MFAPIResponse is the top-level API response from GET /mf/{code}.
type MFAPIResponse struct {
	Meta   SchemeMeta `json:"meta"`
	Data   []NAVData  `json:"data"`
	Status string     `json:"status"`
}

// SchemeMeta is the `meta` block returned by the API.
type SchemeMeta struct {
	FundHouse           string  `json:"fund_house"`
	SchemeType          string  `json:"scheme_type"`
	SchemeCategory      string  `json:"scheme_category"`
	SchemeCode          int     `json:"scheme_code"`
	SchemeName          string  `json:"scheme_name"`
	ISINGrowth          *string `json:"isin_growth"`
	ISINDivReinvestment *string `json:"isin_div_reinvestment"`
}

// NAVData is a single date+nav pair in the `data` array.
// Dates arrive as "DD-MM-YYYY"; NAV arrives as a string decimal.
type NAVData struct {
	Date string `json:"date"`
	NAV  string `json:"nav"`
}

// Client wraps the HTTP client with rate limiting and audit logging.
type Client struct {
	http    *http.Client
	limiter *ratelimit.CompositeRateLimiter
	store   *store.Store
	log     *slog.Logger
}

// New creates a Client.
func New(limiter *ratelimit.CompositeRateLimiter, st *store.Store, log *slog.Logger) *Client {
	return &Client{
		http:    &http.Client{Timeout: 30 * time.Second},
		limiter: limiter,
		store:   st,
		log:     log,
	}
}

// FetchSchemeData fetches scheme metadata + NAV history for a date range.
// Retries up to maxRetries times with exponential backoff.
// On HTTP 429: drains rate limiter tokens and waits cooldownAfter429.
func (c *Client) FetchSchemeData(
	ctx context.Context,
	schemeCode string,
	startDate, endDate time.Time,
) (*MFAPIResponse, error) {
	endpoint := fmt.Sprintf("/mf/%s", schemeCode)
	fullURL := fmt.Sprintf("%s%s?startDate=%s&endDate=%s",
		baseURL, endpoint,
		startDate.Format("2006-01-02"),
		endDate.Format("2006-01-02"),
	)

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff with jitter: 1s, 2s, 4s, 8s
			base := time.Duration(1<<uint(attempt-1)) * time.Second
			jitter := time.Duration(rand.Int63n(int64(500 * time.Millisecond)))
			delay := base + jitter
			c.log.Info("retrying after backoff",
				"scheme_code", schemeCode,
				"attempt", attempt+1,
				"delay", delay,
				"last_error", lastErr,
			)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		// Acquire rate limiter tokens before every request
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("rate limiter wait: %w", err)
		}

		resp, durationMS, err := c.doRequest(ctx, fullURL)

		// Build audit log entry
		logEntry := &models.RequestLog{
			Endpoint:     endpoint,
			FullURL:      fullURL,
			DurationMS:   &durationMS,
			RetryAttempt: attempt,
		}
		if schemeCode != "" {
			logEntry.SchemeCode = &schemeCode
		}

		if err != nil {
			errMsg := err.Error()
			logEntry.ErrorMsg = &errMsg
			c.store.LogRequest(ctx, logEntry)
			lastErr = fmt.Errorf("http request: %w", err)
			continue
		}

		status := resp.StatusCode
		logEntry.HTTPStatus = &status

		switch {
		case status == http.StatusOK:
			c.store.LogRequest(ctx, logEntry)
			return resp.body, nil

		case status == http.StatusTooManyRequests:
			// 429: drain all token buckets and wait for cooldown
			errMsg := "HTTP 429 Too Many Requests"
			logEntry.ErrorMsg = &errMsg
			c.store.LogRequest(ctx, logEntry)
			c.log.Warn("received HTTP 429, cooling down",
				"scheme_code", schemeCode,
				"cooldown", cooldownAfter429,
			)
			c.limiter.DrainAll(ctx)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(cooldownAfter429):
			}
			lastErr = fmt.Errorf("rate limited (429)")
			continue

		case status >= 500:
			// Server error — transient, retry
			errMsg := fmt.Sprintf("HTTP %d server error", status)
			logEntry.ErrorMsg = &errMsg
			c.store.LogRequest(ctx, logEntry)
			lastErr = fmt.Errorf("server error %d", status)
			continue

		default:
			// 4xx client error (except 429) — do NOT retry
			errMsg := fmt.Sprintf("HTTP %d client error", status)
			logEntry.ErrorMsg = &errMsg
			c.store.LogRequest(ctx, logEntry)
			return nil, fmt.Errorf("non-retryable client error: HTTP %d for %s", status, fullURL)
		}
	}

	return nil, fmt.Errorf("all %d attempts failed for %s: %w", maxRetries, schemeCode, lastErr)
}

// httpResp holds the parsed response body and status code.
type httpResp struct {
	body       *MFAPIResponse
	StatusCode int
}

// doRequest performs a single HTTP GET and returns parsed response + latency.
func (c *Client) doRequest(ctx context.Context, url string) (*httpResp, int, error) {
	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	res, err := c.http.Do(req)
	durationMS := int(time.Since(start).Milliseconds())
	if err != nil {
		return nil, durationMS, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return &httpResp{StatusCode: res.StatusCode}, durationMS, nil
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, durationMS, fmt.Errorf("read body: %w", err)
	}

	var apiResp MFAPIResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, durationMS, fmt.Errorf("unmarshal response: %w", err)
	}

	if apiResp.Status != "SUCCESS" {
		return nil, durationMS, fmt.Errorf("API returned status=%q", apiResp.Status)
	}

	return &httpResp{body: &apiResp, StatusCode: res.StatusCode}, durationMS, nil
}

// ParseResponse converts a raw API response into a Scheme and sorted NAVRows.
// Dates are parsed from "DD-MM-YYYY" and NAVs from decimal strings.
// Invalid rows (zero NAV, unparseable date/NAV) are skipped with a warning.
func ParseResponse(schemeCode string, resp *MFAPIResponse, log *slog.Logger) (*models.Scheme, []models.NAVRow, error) {
	if resp == nil {
		return nil, nil, fmt.Errorf("nil API response for scheme %s", schemeCode)
	}

	scheme := &models.Scheme{
		Code:                schemeCode,
		Name:                resp.Meta.SchemeName,
		AMC:                 resp.Meta.FundHouse,
		Category:            resp.Meta.SchemeCategory,
		SchemeType:          resp.Meta.SchemeType,
		ISINGrowth:          resp.Meta.ISINGrowth,
		ISINDivReinvestment: resp.Meta.ISINDivReinvestment,
	}

	var navRows []models.NAVRow
	skipped := 0
	for _, d := range resp.Data {
		navDate, err := time.Parse("02-01-2006", d.Date)
		if err != nil {
			skipped++
			continue
		}

		navVal, err := strconv.ParseFloat(d.NAV, 64)
		if err != nil || navVal <= 0 {
			skipped++
			continue
		}

		navRows = append(navRows, models.NAVRow{
			SchemeCode: schemeCode,
			NavDate:    navDate,
			NAV:        navVal,
		})
	}

	if skipped > 0 {
		log.Warn("skipped invalid NAV rows", "scheme_code", schemeCode, "skipped", skipped)
	}

	// API returns dates newest-first; sort ascending for analytics engine
	sortNAVRowsAsc(navRows)

	return scheme, navRows, nil
}

// sortNAVRowsAsc sorts NAV rows oldest-first in place.
func sortNAVRowsAsc(rows []models.NAVRow) {
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].NavDate.Before(rows[j].NavDate)
	})
}
