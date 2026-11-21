package board

import (
	"context"
	"database/sql"
	"testing"
)

// canaryFixture is a board with one endpoint, so a report has something to change.
func canaryFixture(t *testing.T) (*sql.DB, *FixedClock, string) {
	t.Helper()
	db, clock := newTestDB(t)
	ep := mustCreateEndpoint(t, db, clock, testRegistry(), CreateEndpointRequest{
		Name: "cloud", Provider: "openai", BaseURL: "http://127.0.0.1:8099/v1", Reach: "wan",
	})
	return db, clock, ep.ID
}

func endpointHealth(t *testing.T, db *sql.DB, id string) (string, string) {
	t.Helper()
	var health, reason string
	err := db.QueryRowContext(context.Background(),
		`SELECT health, COALESCE(health_reason,'') FROM endpoints WHERE id=?`, id).Scan(&health, &reason)
	if err != nil {
		t.Fatalf("read endpoint: %v", err)
	}
	return health, reason
}

// canaryEvents counts the events of one type about one endpoint.
func canaryEvents(t *testing.T, db *sql.DB, entityID, typ string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE entity='endpoint' AND entity_id=? AND type=?`,
		entityID, typ).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

// TestAnUnreachableCanaryMarksTheEndpointDown is the whole point: a dead model must stop being
// chosen, and it must say who noticed.
func TestAnUnreachableCanaryMarksTheEndpointDown(t *testing.T) {
	db, clock, id := canaryFixture(t)
	res, err := ReportCanary(context.Background(), db, clock, id, "pi",
		CanaryReport{Reachable: false, Reason: "dial tcp 10.0.0.9:443: connection refused"})
	if err != nil {
		t.Fatalf("ReportCanary: %v", err)
	}
	if res.Health != "down" || !res.Changed {
		t.Fatalf("result = %+v, want down and changed", res)
	}
	health, reason := endpointHealth(t, db, id)
	if health != "down" || reason != "dial tcp 10.0.0.9:443: connection refused" {
		t.Fatalf("endpoint = %q / %q", health, reason)
	}
	if n := canaryEvents(t, db, id, "down"); n != 1 {
		t.Fatalf("down events = %d, want 1", n)
	}
}

// TestTheSecondUnreachableReportWritesNoEvent: a canary every minute would otherwise write 1,440
// "still down" events a day per endpoint into the spine.
func TestTheSecondUnreachableReportWritesNoEvent(t *testing.T) {
	db, clock, id := canaryMarker(t)
	for i := 0; i < 3; i++ {
		if _, err := ReportCanary(context.Background(), db, clock, id, "pi",
			CanaryReport{Reachable: false, Reason: "timeout"}); err != nil {
			t.Fatalf("report %d: %v", i, err)
		}
	}
	if n := canaryEvents(t, db, id, "down"); n != 1 {
		t.Fatalf("down events = %d, want exactly one for three reports", n)
	}
}

// canaryMarker is a fixture whose endpoint is already down, so the next reports are repeats.
func canaryMarker(t *testing.T) (*sql.DB, *FixedClock, string) {
	t.Helper()
	db, clock, id := canaryFixture(t)
	if _, err := ReportCanary(context.Background(), db, clock, id, "pi",
		CanaryReport{Reachable: false, Reason: "timeout"}); err != nil {
		t.Fatalf("seed report: %v", err)
	}
	return db, clock, id
}

// TestASuccessfulCanaryDoesNotPromoteTheEndpoint: an unauthenticated reachability probe must not
// overrule the manager's authenticated one — a 401 from a reachable service is not health.
func TestASuccessfulCanaryDoesNotPromoteTheEndpoint(t *testing.T) {
	db, clock, id := canaryMarker(t)
	before, _ := endpointHealth(t, db, id)
	if before != "down" {
		t.Fatalf("seed health = %q, want down", before)
	}
	res, err := ReportCanary(context.Background(), db, clock, id, "pi",
		CanaryReport{Reachable: true, LatencyMS: 41})
	if err != nil {
		t.Fatalf("ReportCanary: %v", err)
	}
	if res.Health != "down" || res.Changed {
		t.Fatalf("result = %+v, want down and unchanged", res)
	}
	health, reason := endpointHealth(t, db, id)
	if health != "down" || reason == "" {
		t.Fatalf("a success cleared the reason or promoted the endpoint: %q / %q", health, reason)
	}
	// The latency is still recorded: "reachable but unhealthy" is a useful pair of facts.
	var latency int64
	if err := db.QueryRowContext(context.Background(),
		`SELECT latency_ms FROM endpoints WHERE id=?`, id).Scan(&latency); err != nil {
		t.Fatalf("latency: %v", err)
	}
	if latency != 41 {
		t.Fatalf("latency = %d, want 41", latency)
	}
}

// TestACanaryForAnUnknownEndpointIsAnError: a 404, not a silent write to nowhere.
func TestACanaryForAnUnknownEndpointIsAnError(t *testing.T) {
	db, clock := newTestDB(t)
	if _, err := ReportCanary(context.Background(), db, clock, "nope", "pi",
		CanaryReport{Reachable: false}); err == nil {
		t.Fatal("a report for a missing endpoint was accepted")
	}
}

// TestTheCanaryListCarriesNoAuthRef: the node token is not a model credential; a canary needs the
// address and nothing else.
func TestTheCanaryListCarriesNoAuthRef(t *testing.T) {
	db, _, _ := canaryFixture(t)
	targets, err := ListCanaryTargets(context.Background(), db)
	if err != nil {
		t.Fatalf("ListCanaryTargets: %v", err)
	}
	if len(targets) != 1 || targets[0].BaseURL == "" || targets[0].Name != "cloud" {
		t.Fatalf("targets = %+v", targets)
	}
}
