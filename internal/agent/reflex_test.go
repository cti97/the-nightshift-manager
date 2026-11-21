package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cti97/the-nightshift-manager/internal/provider"
)

// TestReflexIsOneCallWithNoTools: the steward's path is claim, one call, report. No tool
// definitions are sent, because a model that believes it has tools spends its only turn asking
// for them.
func TestReflexIsOneCallWithNoTools(t *testing.T) {
	p := &testProvider{name: "scripted", complete: func(call int, req provider.Request) (provider.Response, error) {
		if len(req.Tools) != 0 {
			t.Fatalf("the reflex request carried %d tool definitions, want none", len(req.Tools))
		}
		if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
			t.Fatalf("messages = %+v, want the brief alone", req.Messages)
		}
		return provider.Response{Content: "  classified: cache  ", Usage: provider.Usage{In: 40, Out: 8}}, nil
	}}
	e := &ReflexExecutor{newProvider: func(provider.Config) (provider.Provider, error) { return p, nil }}
	res, err := e.Run(context.Background(), claimedItem("run-1", "classify this", "reflex"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Summary != "classified: cache" {
		t.Fatalf("summary = %q, want the trimmed answer", res.Summary)
	}
	if res.TokensIn != 40 || res.TokensOut != 8 {
		t.Fatalf("tokens = %d/%d, want the provider's usage", res.TokensIn, res.TokensOut)
	}
	if got := p.callCount(); got != 1 {
		t.Fatalf("provider calls = %d, want exactly one", got)
	}
}

// TestReflexRefusesAnEmptyAnswer: a run that reported "done" with nothing said is the failure
// result_summary exists to prevent.
func TestReflexRefusesAnEmptyAnswer(t *testing.T) {
	p := &testProvider{name: "scripted", complete: func(call int, req provider.Request) (provider.Response, error) {
		return provider.Response{Content: "   ", Usage: provider.Usage{In: 10, Out: 0}}, nil
	}}
	e := &ReflexExecutor{newProvider: func(provider.Config) (provider.Provider, error) { return p, nil }}
	if _, err := e.Run(context.Background(), claimedItem("run-1", "x", "reflex")); err == nil {
		t.Fatal("an empty answer was accepted")
	} else if !strings.Contains(err.Error(), "empty answer") {
		t.Fatalf("err = %v, want it to name the empty answer", err)
	}
}

// TestReflexFailsOverBudget: one call can overspend, and the run must fail rather than finish
// over its budget — the same rule the loop applies after settling.
func TestReflexFailsOverBudget(t *testing.T) {
	p := &testProvider{name: "scripted", complete: func(call int, req provider.Request) (provider.Response, error) {
		return provider.Response{Content: "an expensive answer", Usage: provider.Usage{In: 900, Out: 200}}, nil
	}}
	e := &ReflexExecutor{
		newProvider: func(provider.Config) (provider.Provider, error) { return p, nil },
		TokenBudget: 1000,
	}
	if _, err := e.Run(context.Background(), claimedItem("run-1", "x", "reflex")); !errors.Is(err, ErrTokenBudget) {
		t.Fatalf("err = %v, want ErrTokenBudget", err)
	}
}
