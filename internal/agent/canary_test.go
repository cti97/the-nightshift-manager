package agent

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cti97/the-nightshift-manager/internal/board"
)

// fakeCanary is a board that counts reports, so hysteresis can be asserted as "how many times did
// the steward speak" rather than by inspecting internal counters.
type fakeCanary struct {
	mu      sync.Mutex
	targets []board.CanaryTarget
	reports []board.CanaryReport
	ids     []string
}

func (f *fakeCanary) CanaryTargets(ctx context.Context) ([]board.CanaryTarget, error) {
	return f.targets, nil
}

func (f *fakeCanary) ReportCanary(ctx context.Context, id string, rep board.CanaryReport) (board.CanaryResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids = append(f.ids, id)
	f.reports = append(f.reports, rep)
	return board.CanaryResult{EndpointID: id, Health: "down", Changed: true}, nil
}

func (f *fakeCanary) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reports)
}

// TestTheCanaryWaitsForTheThreshold: one dropped packet is not a dead model. Three in a row is a
// report; two more failures stay silent.
func TestTheCanaryWaitsForTheThreshold(t *testing.T) {
	// A port with nothing on it: every probe fails for a real reason.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	f := &fakeCanary{targets: []board.CanaryTarget{{ID: "ep-1", Name: "cloud", BaseURL: "http://" + addr}}}
	k := newCanary(f, CanarySettings{Interval: time.Hour, Threshold: 3, Timeout: 300 * time.Millisecond})
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := k.pass(ctx); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if got := f.count(); got != 0 {
		t.Fatalf("reported %d times before the threshold, want 0", got)
	}
	if err := k.pass(ctx); err != nil {
		t.Fatalf("pass 3: %v", err)
	}
	if got := f.count(); got != 1 {
		t.Fatalf("reported %d times at the threshold, want 1", got)
	}
	if f.reports[0].Reachable {
		t.Fatal("the report says reachable")
	}
	// Three more failures are another crossing, not six individual reports.
	for i := 0; i < 3; i++ {
		_ = k.pass(ctx)
	}
	if got := f.count(); got != 2 {
		t.Fatalf("reported %d times after two crossings, want 2", got)
	}
}

// TestTheCanaryReportsARecoveryOnce: after a demotion, the first success is reported (that is what
// records last_ok_at) and the ones after it are silent.
func TestTheCanaryReportsARecoveryOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // reachable, wrong key: not the canary's verdict
	}))
	defer srv.Close()

	f := &fakeCanary{targets: []board.CanaryTarget{{ID: "ep-1", BaseURL: srv.URL}}}
	k := newCanary(f, CanarySettings{Interval: time.Hour, Threshold: 1, Timeout: time.Second})
	// Pretend the board already believes it is down because of this canary.
	k.down["ep-1"] = true
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := k.pass(ctx); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if got := f.count(); got != 1 {
		t.Fatalf("reported %d times, want exactly one recovery", got)
	}
	if !f.reports[0].Reachable {
		t.Fatal("the recovery report says unreachable")
	}
	if k.down["ep-1"] {
		t.Fatal("the canary still believes it reported a demotion")
	}
}

// TestTheProbeTellsUpFromBroken: 401 is a reachable service (its key is someone else's problem),
// 503 is not a working endpoint, and a closed port is not reachable at all.
func TestTheProbeTellsUpFromBroken(t *testing.T) {
	status := http.StatusUnauthorized
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))
	defer srv.Close()

	k := newCanary(&fakeCanary{}, CanarySettings{Timeout: time.Second})
	if rep := k.probe(context.Background(), board.CanaryTarget{BaseURL: srv.URL}); !rep.Reachable {
		t.Fatalf("401 counted as unreachable: %+v", rep)
	}
	status = http.StatusServiceUnavailable
	if rep := k.probe(context.Background(), board.CanaryTarget{BaseURL: srv.URL}); rep.Reachable {
		t.Fatalf("503 counted as reachable: %+v", rep)
	} else if !strings.Contains(rep.Reason, "503") {
		t.Fatalf("the reason does not name the status: %q", rep.Reason)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	if rep := k.probe(context.Background(), board.CanaryTarget{BaseURL: "http://" + addr}); rep.Reachable {
		t.Fatalf("a closed port counted as reachable: %+v", rep)
	}
}

// TestTheCanaryLoopStopsWithItsContext: Run waits for this goroutine, so a loop that ignores the
// context turns shutdown into a hang.
func TestTheCanaryLoopStopsWithItsContext(t *testing.T) {
	f := &fakeCanary{}
	k := newCanary(f, CanarySettings{Interval: 10 * time.Millisecond, Timeout: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		k.Loop(ctx, func(string, ...any) {})
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the canary loop did not stop when its context ended")
	}
}
