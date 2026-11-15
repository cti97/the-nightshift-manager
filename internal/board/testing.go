package board

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// OpenForTest opens a board on a fixed clock. It lives in a non-_test file so
// the agent package can build the same fixture without duplicating schema
// knowledge. The seeding helpers below share that reason.
func OpenForTest(t *testing.T, path string) (*sql.DB, *FixedClock) {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open board: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, NewFixedClock(1_700_000_000)
}

// SeedNodeForTest registers and heartbeats a node, so it is 'online' with the
// given live capabilities. That is the state a claiming node actually has.
func SeedNodeForTest(t *testing.T, db *sql.DB, clock *FixedClock, name, role string, provides ...string) string {
	t.Helper()
	id, _, err := RegisterNode(context.Background(), db, clock, RegisterRequest{
		Name: name, Role: role, ProvidesDeclared: provides,
		ReachClasses: []string{"host", "lan", "wan"}, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("seed node %s: %v", name, err)
	}
	if _, err := Heartbeat(context.Background(), db, clock, id, HeartbeatRequest{
		ProvidesLive: provides, ReachClasses: []string{"host", "lan", "wan"}, Protocol: ProtocolMax,
	}); err != nil {
		t.Fatalf("heartbeat %s: %v", name, err)
	}
	return id
}

// SeedEndpointForTest inserts one healthy endpoint and its enabled model. The
// node half of placement is not involved: any node can be pointed at it.
func SeedEndpointForTest(t *testing.T, db *sql.DB, clock *FixedClock, name, reach string, cost float64, features ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO endpoints (id,name,provider,base_url,reach,health,created_at) VALUES (?,?,?,?,?,'ok',?)`,
		"ep-"+name, name, "stub", "http://"+name, reach, clock.Now()); err != nil {
		t.Fatalf("insert endpoint: %v", err)
	}
	feat := `["` + strings.Join(features, `","`) + `"]`
	if len(features) == 0 {
		feat = "[]"
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO models (id,endpoint_id,model_id,provides,features,context_window,cost_in_per_mtok)
		 VALUES (?,?,?,'[]',?,200000,?)`,
		"m-"+name, "ep-"+name, name+"-model", feat, cost); err != nil {
		t.Fatalf("insert model: %v", err)
	}
}
