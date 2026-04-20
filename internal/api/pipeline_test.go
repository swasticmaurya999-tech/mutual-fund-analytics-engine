// Package api_test — pipeline resumability tests.
//
// "Pipeline resumability after crash" is observable via the API:
//   - After a crash, schemes will be stuck in running/error/pending states.
//   - GET /sync/status must surface all states so an operator can diagnose.
//   - POST /sync/trigger must signal the pipeline to resume processing.
//
// These tests are in a separate file to clearly separate the concern
// (pipeline recovery) from the general API endpoint contract tests.
package api_test

import (
	"net/http"
	"testing"
	"time"

	"mutualfundanalysis/internal/models"
)

// TestCrashRecoveryStates verifies that after a simulated crash — where schemes
// are left in all four possible states — GET /sync/status surfaces each state
// accurately in the summary and the schemes array.
//
// The four states model the following real crash scenarios:
//
//	done    — NAV sync completed and analytics computed before crash
//	pending — never started; service restarted before reaching this scheme
//	running — was mid-sync when process was killed (stale lock)
//	error   — previous attempt failed with a retriable error
func TestCrashRecoveryStates(t *testing.T) {
	now := time.Now()
	ms := &mockStore{
		syncStates: []*models.SyncState{
			{SchemeCode: "S1", Status: "done", NavCount: 2513, UpdatedAt: now},
			{SchemeCode: "S2", Status: "pending", NavCount: 0, UpdatedAt: now},
			{SchemeCode: "S3", Status: "running", NavCount: 500, UpdatedAt: now},
			{SchemeCode: "S4", Status: "error", NavCount: 0, UpdatedAt: now},
		},
	}
	h := newRouter(ms)
	rr := do(t, h, http.MethodGet, "/sync/status")

	var b map[string]any
	decode(t, rr, http.StatusOK, &b)

	t.Run("all 4 states visible in summary", func(t *testing.T) {
		sum := b["summary"].(map[string]any)
		want := map[string]float64{
			"total": 4, "done": 1, "pending": 1, "running": 1, "error": 1,
		}
		for field, wantVal := range want {
			if got := sum[field].(float64); got != wantVal {
				t.Errorf("summary.%s=%v want=%v", field, got, wantVal)
			}
		}
	})

	t.Run("all 4 schemes present in schemes array", func(t *testing.T) {
		schemes := b["schemes"].([]any)
		if len(schemes) != 4 {
			t.Errorf("schemes count=%d want=4", len(schemes))
		}
		// Build a status map to verify each scheme_code has the right status.
		statusMap := map[string]string{}
		for _, s := range schemes {
			m := s.(map[string]any)
			statusMap[m["scheme_code"].(string)] = m["status"].(string)
		}
		expected := map[string]string{
			"S1": "done", "S2": "pending", "S3": "running", "S4": "error",
		}
		for code, wantStatus := range expected {
			if statusMap[code] != wantStatus {
				t.Errorf("scheme %s status=%q want=%q", code, statusMap[code], wantStatus)
			}
		}
	})

	t.Run("error scheme is identifiable — status=error in schemes array", func(t *testing.T) {
		schemes := b["schemes"].([]any)
		var errCount int
		for _, s := range schemes {
			if s.(map[string]any)["status"] == "error" {
				errCount++
			}
		}
		if errCount != 1 {
			t.Errorf("error schemes=%d want=1", errCount)
		}
	})
}

// TestPipelineTrigger verifies that POST /sync/trigger immediately returns
// 202 Accepted (non-blocking) and signals the pipeline to run.
//
// The pipeline runs asynchronously after the response is sent, so the test
// only verifies the HTTP contract (202 + status:accepted message).
func TestPipelineTrigger(t *testing.T) {
	rr := do(t, newRouter(&mockStore{}), http.MethodPost, "/sync/trigger")

	t.Run("returns 202 Accepted immediately — non-blocking", func(t *testing.T) {
		var b map[string]string
		decode(t, rr, http.StatusAccepted, &b)
		if b["status"] != "accepted" {
			t.Errorf("status=%q want=accepted", b["status"])
		}
		if b["message"] == "" {
			t.Error("message field should describe the triggered action")
		}
	})
}
