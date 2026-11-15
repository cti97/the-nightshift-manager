package board

import (
	"strings"
	"testing"
)

func cand(nodeName, role string, provides []string, endpointName, reach, health string, features []string, cost float64) Candidate {
	return Candidate{
		Node: Node{
			Name: nodeName, Role: role, ProvidesLive: provides,
			ReachClasses: []string{"host", "lan", "wan"}, Status: "online",
		},
		Endpoint: Endpoint{Name: endpointName, Reach: reach, Health: health},
		Model: Model{
			ModelID: endpointName + "-m", Features: features,
			CostInPerMtok: cost, ContextWindow: 200000, Enabled: true,
		},
	}
}

func item(needs ...string) WorkItem {
	return WorkItem{ID: "w1", Needs: needs, Status: "queued"}
}

func TestPlaceRejectsEndpointTheNodeCannotReach(t *testing.T) {
	c := cand("laptop", "executor", []string{"agentic", "shell"}, "cloud", "wan", "ok", []string{"tools"}, 1)
	c.Node.ReachClasses = []string{"host"}

	d := Place(item("agentic"), []Candidate{c})
	if d.OK {
		t.Fatal("placed on an unreachable endpoint")
	}
	if len(d.Rejects) != 1 {
		t.Fatalf("got %d rejects, want 1", len(d.Rejects))
	}
}

func TestPlaceNeverCrossesWANForNoEgress(t *testing.T) {
	cheapWAN := cand("laptop", "executor", []string{"agentic"}, "cloud", "wan", "ok", []string{"tools"}, 0.1)
	lanPi := cand("pi", "steward", []string{"reflex", "no_egress"}, "local", "host", "ok", nil, 0)

	d := Place(item("reflex", "no_egress"), []Candidate{cheapWAN, lanPi})
	if !d.OK {
		t.Fatalf("expected a placement, rejects: %+v", d.Rejects)
	}
	if d.Chosen.Endpoint.Reach != "host" {
		t.Fatalf("chose reach %q, want host", d.Chosen.Endpoint.Reach)
	}
}

func TestPlaceStrictLocalRequiresHost(t *testing.T) {
	lan := cand("pi", "steward", []string{"reflex", "strict_local"}, "lan-ollama", "lan", "ok", nil, 0)
	if d := Place(item("reflex", "strict_local"), []Candidate{lan}); d.OK {
		t.Fatal("strict_local placed on a lan endpoint")
	}
}

func TestAgenticRequiresToolsAndExecutor(t *testing.T) {
	noTools := cand("laptop", "executor", []string{"agentic"}, "llamacpp", "host", "ok", nil, 0)
	if d := Place(item("agentic"), []Candidate{noTools}); d.OK {
		t.Fatal("agentic placed on a model without the tools feature")
	}

	stewardWithTools := cand("pi", "steward", []string{"agentic"}, "local", "host", "ok", []string{"tools"}, 0)
	if d := Place(item("agentic"), []Candidate{stewardWithTools}); d.OK {
		t.Fatal("agentic placed on a steward")
	}
}

func TestPlaceSkipsUnhealthyAndPicksCheapest(t *testing.T) {
	expensive := cand("a", "executor", []string{"agentic"}, "claude", "wan", "ok", []string{"tools"}, 9)
	cheap := cand("b", "executor", []string{"agentic"}, "gpt", "wan", "ok", []string{"tools"}, 1)
	warming := cand("c", "executor", []string{"agentic"}, "llamacpp", "host", "warming", []string{"tools"}, 0)

	d := Place(item("agentic"), []Candidate{expensive, warming, cheap})
	if !d.OK {
		t.Fatalf("no placement: %+v", d.Rejects)
	}
	if d.Chosen.Endpoint.Name != "gpt" {
		t.Fatalf("chose %q, want gpt (cheapest healthy)", d.Chosen.Endpoint.Name)
	}
	if d.Reason == "" {
		t.Fatal("Reason is empty; every placement must explain itself")
	}
}

func TestUnsatisfiableExplainsWhatIsMissing(t *testing.T) {
	pi := cand("pi", "steward", []string{"reflex"}, "local", "host", "ok", nil, 0)

	d := Place(item("reflex", "gpu"), []Candidate{pi})
	if d.OK {
		t.Fatal("placed work nothing can do")
	}
	if d.Reason == "" {
		t.Fatal("unsatisfiable placement has no reason")
	}
}

// The six tests above are the plan's. Mutation testing showed four of the
// rules they cover can be deleted with the whole suite still green — the
// no_egress reach rule (the wan candidate above is rejected earlier by the
// capability subset), the reflex role rule, the vision feature rule, and the
// context-window rule (no test uses a tokens: need) — plus the cost tie-break
// on node name. These supplementary cases pin each one so a future refactor
// cannot drop a rule silently. They assert the exact reason strings because
// that string is the only thing a human sees when work sits queued.

func TestPlaceNoEgressRejectsWanEvenWhenCapabilitiesMatch(t *testing.T) {
	c := cand("laptop", "executor", []string{"reflex", "no_egress"}, "cloud", "wan", "ok", nil, 0)

	ok, why := Eligible(item("reflex", "no_egress"), c)
	if ok {
		t.Fatal("no_egress placed work on a wan endpoint")
	}
	if why != "no_egress forbids wan" {
		t.Fatalf("reason %q does not name the no_egress rule", why)
	}
}

func TestPlaceReflexRequiresExecutorOrSteward(t *testing.T) {
	c := cand("manager", "manager", []string{"reflex"}, "local", "host", "ok", nil, 0)

	ok, why := Eligible(item("reflex"), c)
	if ok {
		t.Fatal("reflex placed on a manager node")
	}
	if why != "reflex requires an executor or steward" {
		t.Fatalf("reason %q does not name the reflex rule", why)
	}
}

func TestPlaceVisionRequiresModelFeature(t *testing.T) {
	noVision := cand("laptop", "executor", []string{"vision"}, "vlm", "host", "ok", nil, 0)

	ok, why := Eligible(item("vision"), noVision)
	if ok {
		t.Fatal("vision placed on a model without the vision feature")
	}
	if why != "vision requires the model vision feature" {
		t.Fatalf("reason %q does not name the vision rule", why)
	}

	withVision := cand("laptop", "executor", []string{"vision"}, "vlm", "host", "ok", []string{"vision"}, 0)
	if ok, why := Eligible(item("vision"), withVision); !ok {
		t.Fatalf("vision rejected a model with the vision feature: %s", why)
	}
}

func TestPlaceContextWindowMustCoverTokenNeed(t *testing.T) {
	c := cand("laptop", "executor", []string{"tokens:500000"}, "small", "host", "ok", nil, 0)
	// cand() pins ContextWindow at 200000, so the provider claim exceeds the
	// window: the typed capability alone must not win.

	ok, why := Eligible(item("tokens:500000"), c)
	if ok {
		t.Fatal("placed a 500k-token need on a 200k window")
	}
	if why != "context 200000 < 500000" {
		t.Fatalf("reason %q does not name the context rule", why)
	}

	c.Model.ContextWindow = 600000
	if ok, why := Eligible(item("tokens:500000"), c); !ok {
		t.Fatalf("rejected a sufficient window: %s", why)
	}
}

func TestPlaceTieBreaksOnNodeName(t *testing.T) {
	b := cand("b", "executor", []string{"agentic"}, "gpt-b", "wan", "ok", []string{"tools"}, 1)
	a := cand("a", "executor", []string{"agentic"}, "gpt-a", "wan", "ok", []string{"tools"}, 1)

	d := Place(item("agentic"), []Candidate{b, a})
	if !d.OK {
		t.Fatalf("no placement: %+v", d.Rejects)
	}
	if d.Chosen.Node.Name != "a" {
		t.Fatalf("tie chose node %q, want a (cost, then node name)", d.Chosen.Node.Name)
	}
}

// Fix round: the model half of the capability union, the text of Rejects (the
// only thing /why shows a human), and a total ranking order. Each pins
// behaviour that could be deleted with the rest of the suite still green.

func TestPlaceAcceptsCapabilityProvidedOnlyByTheModel(t *testing.T) {
	// tokens: and vision are realistically provided by the model, not the
	// node, so the union at the top of Eligible must include Model.Provides.
	c := cand("laptop", "executor", nil, "vlm", "host", "ok", []string{"vision"}, 0)
	c.Model.Provides = []string{"vision"}

	d := Place(item("vision"), []Candidate{c})
	if !d.OK {
		t.Fatalf("model-only provides not honoured: %+v", d.Rejects)
	}
}

func TestPlaceRejectNamesTheUnreachableReachClass(t *testing.T) {
	c := cand("laptop", "executor", []string{"agentic"}, "cloud", "wan", "ok", []string{"tools"}, 0)
	c.Node.ReachClasses = []string{"host"}

	d := Place(item("agentic"), []Candidate{c})
	if d.OK || len(d.Rejects) != 1 {
		t.Fatalf("OK=%v rejects=%d, want one rejection", d.OK, len(d.Rejects))
	}
	if d.Rejects[0].NodeName != "laptop" {
		t.Fatalf("reject names node %q, want laptop", d.Rejects[0].NodeName)
	}
	if d.Rejects[0].Why != "node cannot reach wan endpoints" {
		t.Fatalf("reject why %q does not name the reach rule", d.Rejects[0].Why)
	}
}

func TestPlaceRejectNamesTheMissingCapability(t *testing.T) {
	pi := cand("pi", "steward", []string{"reflex"}, "local", "host", "ok", nil, 0)

	d := Place(item("reflex", "gpu"), []Candidate{pi})
	if d.OK || len(d.Rejects) != 1 {
		t.Fatalf("OK=%v rejects=%d, want one rejection", d.OK, len(d.Rejects))
	}
	if d.Rejects[0].Why != "no provider for gpu" {
		t.Fatalf("reject why %q does not name the missing capability", d.Rejects[0].Why)
	}
}

func TestPlaceRejectsAnOfflineNode(t *testing.T) {
	c := cand("pi", "steward", []string{"reflex"}, "local", "host", "ok", nil, 0)
	c.Node.Status = "offline"

	ok, why := Eligible(item("reflex"), c)
	if ok {
		t.Fatal("placed work on an offline node")
	}
	if why != "node pi is offline" {
		t.Fatalf("reason %q does not name the node-status rule", why)
	}
}

func TestPlaceRejectsADisabledModel(t *testing.T) {
	c := cand("laptop", "executor", []string{"agentic"}, "gpt", "wan", "ok", []string{"tools"}, 0)
	c.Model.Enabled = false

	ok, why := Eligible(item("agentic"), c)
	if ok {
		t.Fatal("placed work on a disabled model")
	}
	if why != "model gpt-m is disabled" {
		t.Fatalf("reason %q does not name the model-enabled rule", why)
	}
}

func TestPlaceOrderIsTotalAcrossEqualCostModelsOnOneNode(t *testing.T) {
	// Same node and same price: cost and node name both tie, so the model id
	// must decide. The endpoint names are deliberately ordered opposite to the
	// model ids, so losing the model-id key flips the winner.
	b := cand("laptop", "executor", []string{"agentic"}, "a-endpoint", "wan", "ok", []string{"tools"}, 1)
	b.Model.ModelID = "m-b"
	a := cand("laptop", "executor", []string{"agentic"}, "z-endpoint", "wan", "ok", []string{"tools"}, 1)
	a.Model.ModelID = "m-a"

	for _, in := range [][]Candidate{{b, a}, {a, b}} {
		d := Place(item("agentic"), in)
		if !d.OK {
			t.Fatalf("no placement: %+v", d.Rejects)
		}
		if d.Chosen.Model.ModelID != "m-a" {
			t.Fatalf("chose model %q, want m-a regardless of input order", d.Chosen.Model.ModelID)
		}
	}
}

// TestPlaceRefusesWarmingAndAuthFailed pins Task 9's acceptance for the two
// states that are not `down`: placement requires health "ok", so neither is
// placeable, and the reason names the state rather than a bare refusal.
func TestPlaceRefusesWarmingAndAuthFailed(t *testing.T) {
	for _, state := range []string{"warming", "auth_failed", "unknown"} {
		t.Run(state, func(t *testing.T) {
			c := cand("laptop", "executor", []string{"agentic"}, "local", "host", state, []string{"tools"}, 0)
			d := Place(item("agentic"), []Candidate{c})
			if d.OK {
				t.Fatalf("placed on an endpoint whose health is %q", state)
			}
			if len(d.Rejects) != 1 {
				t.Fatalf("got %d rejects, want 1", len(d.Rejects))
			}
			if !strings.Contains(d.Rejects[0].Why, state) {
				t.Fatalf("reject %q does not name the health state %q", d.Rejects[0].Why, state)
			}
		})
	}
}

// TestEligibleCarriesTheProbe'sReason is decision 3 at its source: the reject
// text is what /why prints, so the reason the probe wrote has to survive into
// it. Without this the operator gets "down" and no next step.
func TestEligibleCarriesTheProbeReason(t *testing.T) {
	c := cand("laptop", "executor", []string{"agentic"}, "cloud", "wan", "down", []string{"tools"}, 0)
	c.Endpoint.HealthReason = "401 from /v1/models"
	ok, why := Eligible(item("agentic"), c)
	if ok {
		t.Fatal("Eligible accepted a down endpoint")
	}
	if !strings.Contains(why, "401 from /v1/models") {
		t.Fatalf("why = %q, want it to carry the probe's reason", why)
	}

	// A state with no reason must not sprout a dangling separator.
	quiet := cand("laptop", "executor", []string{"agentic"}, "cloud", "wan", "warming", []string{"tools"}, 0)
	if _, why := Eligible(item("agentic"), quiet); strings.Contains(why, ": ") {
		t.Fatalf("why = %q, want no colon when there is no reason", why)
	}
}
