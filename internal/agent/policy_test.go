package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cti97/the-nightshift-manager/internal/provider"
)

// TestPolicyParsingIsStrict: a policy that silently ignored a typo would be a wall with a hole in
// it, and the operator would believe the node was locked down.
func TestPolicyParsingIsStrict(t *testing.T) {
	if _, err := ParsePolicy([]byte(`{"shell":"deny","sl":"deny"}`)); err == nil {
		t.Fatal("an unknown tool name was accepted")
	}
	if _, err := ParsePolicy([]byte(`{"shell":"forbid"}`)); err == nil {
		t.Fatal("an unknown verdict was accepted")
	}
	if _, err := ParsePolicy([]byte(`{"finish":"deny"}`)); err == nil {
		t.Fatal("denying finish was accepted: the run would have no way to end")
	}
	if _, err := ParsePolicy([]byte(`not json`)); err == nil {
		t.Fatal("a non-JSON policy was accepted")
	}

	p, err := ParsePolicy([]byte(`{"default":"allow","shell":"deny","write_file":"ask"}`))
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	for _, c := range []struct {
		tool string
		want Verdict
	}{
		{"shell", VerdictDeny},
		{"write_file", VerdictAsk},
		{"read_file", VerdictAllow},
	} {
		if got, _ := p.Decide(c.tool); got != c.want {
			t.Fatalf("%s = %q, want %q", c.tool, got, c.want)
		}
	}
}

// TestTheZeroPolicyAllowsEverything: a node without a policy file must behave as it did before
// policy existed. A safety layer that arrives switched on changes every deployment without
// saying so.
func TestTheZeroPolicyAllowsEverything(t *testing.T) {
	var p Policy
	for _, tool := range toolNames() {
		if got, rule := p.Decide(tool); got != VerdictAllow {
			t.Fatalf("%s = %q (%s) under the zero policy, want allow", tool, got, rule)
		}
	}
	if !strings.Contains(p.String(), "no policy") {
		t.Fatalf("String() = %q, want it to say there is no policy", p.String())
	}
}

// TestDeniedToolNeverReachesTheSandbox is the wall itself: the model asks for shell, the policy
// denies it, and no subprocess runs.
//
// Two observables, because one of them was found to be vacuous: the shell writes a RELATIVE marker
// (an absolute path outside the run directory is refused by the Seatbelt profile, which would make
// its absence prove nothing), and the model is told, in the tool result, that the policy refused it.
func TestDeniedToolNeverReachesTheSandbox(t *testing.T) {
	root := t.TempDir()
	const marker = "policy-probe"
	shellArgs, _ := json.Marshal(map[string]string{"command": "touch ./" + marker})
	p := &testProvider{name: "scripted", complete: func(call int, req provider.Request) (provider.Response, error) {
		if call == 0 {
			return callResponse("c1", ToolShell, string(shellArgs), provider.Usage{In: 10, Out: 5}), nil
		}
		// The model has read the refusal and finishes, which is the behaviour the design wants.
		if msg, ok := lastMessageOfRole(req, "tool"); !ok || !strings.Contains(msg.Content, "policy denies shell") {
			t.Fatalf("the model was not told the policy refused shell; tool result = %q (found=%v)", msg.Content, ok)
		}
		return finishResponse("c2", "stopped at the wall", provider.Usage{In: 10, Out: 5}), nil
	}}
	exec := newAgenticInRoot(t, p, root)
	policy, err := ParsePolicy([]byte(`{"default":"allow","shell":"deny"}`))
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	exec.Policy = policy
	res, err := exec.Run(context.Background(), claimedItem("run-1", "do it", "agentic"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Summary != "stopped at the wall" {
		t.Fatalf("summary = %q, want the finish the model reached after the refusal", res.Summary)
	}
	var found string
	err = filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr == nil && info != nil && info.Name() == marker {
			found = path
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the sandbox root: %v", err)
	}
	if found != "" {
		t.Fatalf("the denied shell tool ran and left %s: the policy was consulted after the subprocess, not before", found)
	}
}

// TestAPolicyAskParksTheRun: ask reuses the approval machinery rather than paralleling it — the run
// ends with a request, exactly as request_approval does.
func TestAPolicyAskParksTheRun(t *testing.T) {
	args, _ := json.Marshal(map[string]string{"command": "true"})
	p := &testProvider{name: "scripted", complete: func(call int, req provider.Request) (provider.Response, error) {
		return callResponse("c1", ToolShell, string(args), provider.Usage{In: 10, Out: 5}), nil
	}}
	exec := newAgentic(t, p)
	policy, err := ParsePolicy([]byte(`{"shell":"ask"}`))
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	exec.Policy = policy
	res, err := exec.Run(context.Background(), claimedItem("run-1", "do it", "agentic"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Approval == nil {
		t.Fatal("an ask did not park the run")
	}
	if !strings.Contains(res.Approval.Action, ToolShell) || !strings.Contains(res.Approval.Reason, "policy") {
		t.Fatalf("approval = %+v, want it to name the tool and the policy", res.Approval)
	}
}
