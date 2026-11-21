package agent

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cti97/the-nightshift-manager/internal/board"
)

// CanarySettings is the steward's watch on the endpoints it can reach from its own network path.
// The manager knows whether ITS requests succeed; only the steward knows whether a model is dark
// from inside the house, which is where the work actually runs.
type CanarySettings struct {
	// Interval is how often the whole list is walked. Zero disables the canary.
	Interval time.Duration
	// Threshold is how many consecutive failures make a demotion worth reporting. One is a blip: a
	// WAN endpoint that drops a packet must not take the fleet's model offline for a minute.
	Threshold int
	// Timeout bounds one probe.
	Timeout time.Duration
}

const (
	defaultCanaryThreshold = 3
	defaultCanaryTimeout   = 5 * time.Second
)

func (s CanarySettings) withDefaults() CanarySettings {
	if s.Threshold <= 0 {
		s.Threshold = defaultCanaryThreshold
	}
	if s.Timeout <= 0 {
		s.Timeout = defaultCanaryTimeout
	}
	return s
}

// canaryClient is the slice of the board the canary needs. Narrow on purpose: a test can count what
// was reported without a server, and a future board route cannot widen the canary's reach.
type canaryClient interface {
	CanaryTargets(ctx context.Context) ([]board.CanaryTarget, error)
	ReportCanary(ctx context.Context, id string, rep board.CanaryReport) (board.CanaryResult, error)
}

// canary is stateful only in the ways hysteresis requires: how many times in a row an endpoint has
// failed, and whether the board is currently down because of this canary (so the recovery is
// reported exactly once).
type canary struct {
	c     canaryClient
	s     CanarySettings
	fails map[string]int
	down  map[string]bool
	http  *http.Client
}

func newCanary(c canaryClient, s CanarySettings) *canary {
	s = s.withDefaults()
	return &canary{
		c:     c,
		s:     s,
		fails: make(map[string]int),
		down:  make(map[string]bool),
		http:  &http.Client{Timeout: s.Timeout},
	}
}

// Loop probes until the context ends. A failed pass is logged and retried on the next tick: the
// canary is a background duty, not a reason to stop working.
func (k *canary) Loop(ctx context.Context, logf func(string, ...any)) {
	tick := time.NewTicker(k.s.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if err := k.pass(ctx); err != nil && ctx.Err() == nil {
			logf("canary: %v", err)
		}
	}
}

// pass probes every target once. Probes are sequential: a handful of endpoints every minute does
// not need concurrency, and sequential reporting keeps the board's event order readable.
func (k *canary) pass(ctx context.Context) error {
	targets, err := k.c.CanaryTargets(ctx)
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	for _, t := range targets {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rep := k.probe(ctx, t)
		switch {
		case rep.Reachable:
			delete(k.fails, t.ID)
			if k.down[t.ID] {
				// The recovery is reported once, which is what records last_ok_at and clears nothing
				// else: promotion belongs to the manager's authenticated probe.
				if _, err := k.c.ReportCanary(ctx, t.ID, rep); err != nil {
					return fmt.Errorf("report %s: %w", t.ID, err)
				}
				delete(k.down, t.ID)
			}
		default:
			k.fails[t.ID]++
			if k.fails[t.ID] < k.s.Threshold {
				continue
			}
			// Report the crossing, then reset the counter: the next demotion is another Threshold
			// failures away, so a permanently dead endpoint is reported every Threshold ticks rather
			// than on every one.
			k.fails[t.ID] = 0
			if _, err := k.c.ReportCanary(ctx, t.ID, rep); err != nil {
				return fmt.Errorf("report %s: %w", t.ID, err)
			}
			k.down[t.ID] = true
		}
	}
	return nil
}

// probe asks whether the endpoint answers at all, from here. Any HTTP response below 500 means the
// service is up; the status code is not the canary's business (a 401 means the manager's key is
// wrong, which its own probe will say precisely). A 5xx is the service answering that it is broken,
// which is not reachability worth declaring.
func (k *canary) probe(ctx context.Context, t board.CanaryTarget) board.CanaryReport {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(t.BaseURL, "/"), nil)
	if err != nil {
		return board.CanaryReport{Reachable: false, Reason: "bad address: " + err.Error()}
	}
	resp, err := k.http.Do(req)
	latency := int(time.Since(start).Milliseconds())
	if err != nil {
		return board.CanaryReport{Reachable: false, LatencyMS: latency, Reason: err.Error()}
	}
	// The body is never read: the canary measured a connection, and draining a model server's HTML
	// error page costs more than the fact is worth.
	_ = resp.Body.Close()
	if resp.StatusCode >= 500 {
		return board.CanaryReport{Reachable: false, LatencyMS: latency,
			Reason: fmt.Sprintf("the endpoint answered %s", resp.Status)}
	}
	return board.CanaryReport{Reachable: true, LatencyMS: latency}
}
