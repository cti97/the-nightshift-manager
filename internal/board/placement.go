package board

import (
	"fmt"
	"sort"
	"strings"

	"github.com/cti97/the-nightshift-manager/internal/caps"
)

type Node struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Role         string   `json:"role"`
	ProvidesLive []string `json:"provides_live"`
	ReachClasses []string `json:"reach_classes"`
	Concurrency  int      `json:"concurrency"`
	Status       string   `json:"status"`
	AgentVersion string   `json:"agent_version"`
}

type Endpoint struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Reach    string `json:"reach"`
	Health   string `json:"health"`
	// HealthReason is what the last probe recorded, and it is here because
	// Eligible needs it, not because placement ranks by it (Task 9 decision 3).
	// The explanation path prints Eligible's reason, and "endpoint cloud is
	// down: 401 from /v1/models" is the difference between an operator checking
	// their network and an operator checking their credential. The loader
	// selects the column; nothing in placement reads it except the refusal
	// message.
	HealthReason string `json:"health_reason,omitempty"`
}

type Model struct {
	ID            string   `json:"id"`
	ModelID       string   `json:"model_id"`
	Provides      []string `json:"provides"`
	Features      []string `json:"features"`
	ContextWindow int      `json:"context_window"`
	CostInPerMtok float64  `json:"cost_in_per_mtok"`
	Enabled       bool     `json:"enabled"`
}

type WorkItem struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Needs   []string `json:"needs"`
	Status  string   `json:"status"`
	Attempt int      `json:"attempt"`
}

type Candidate struct {
	Node     Node
	Endpoint Endpoint
	Model    Model
}

type Reject struct {
	NodeName string `json:"node"`
	Why      string `json:"why"`
}

type Decision struct {
	OK      bool
	Chosen  *Candidate
	Reason  string
	Rejects []Reject
}

// Eligible applies spec §9. It returns false plus a human-readable reason;
// the reason is what /why prints, so it must name the rule that failed.
//
// Availability is checked first, then capability, then policy. That order is
// deliberate: "your only gpu node is offline" is the answer an operator needs,
// and it would be hidden behind "no provider for gpu" if capability came first.
// Every rule is here, not in the loader, so the claiming path and the
// explanation path cannot drift apart.
func Eligible(item WorkItem, c Candidate) (bool, string) {
	if c.Node.Status != "online" {
		return false, "node " + c.Node.Name + " is " + c.Node.Status
	}
	if c.Endpoint.Health != "ok" {
		return false, unhealthyWhy(c.Endpoint)
	}
	if !c.Model.Enabled {
		return false, "model " + c.Model.ModelID + " is disabled"
	}
	provides := append(append([]string{}, c.Node.ProvidesLive...), c.Model.Provides...)
	if miss := caps.Missing(item.Needs, provides); len(miss) > 0 {
		return false, "no provider for " + strings.Join(miss, ",")
	}
	if !contains(c.Node.ReachClasses, c.Endpoint.Reach) {
		return false, fmt.Sprintf("node cannot reach %s endpoints", c.Endpoint.Reach)
	}
	for _, need := range item.Needs {
		switch need {
		case "no_egress":
			if c.Endpoint.Reach != "host" && c.Endpoint.Reach != "lan" {
				return false, "no_egress forbids wan"
			}
		case "strict_local":
			if c.Endpoint.Reach != "host" {
				return false, "strict_local requires a host endpoint"
			}
		}
	}
	if contains(item.Needs, "agentic") {
		if c.Node.Role != "executor" {
			return false, "agentic requires an executor"
		}
		if !contains(c.Model.Features, "tools") {
			return false, "agentic requires the model tools feature"
		}
	}
	if contains(item.Needs, "reflex") && c.Node.Role != "executor" && c.Node.Role != "steward" {
		return false, "reflex requires an executor or steward"
	}
	if contains(item.Needs, "vision") && !contains(c.Model.Features, "vision") {
		return false, "vision requires the model vision feature"
	}
	for _, need := range item.Needs {
		if n, ok := tokenNeed(need); ok && c.Model.ContextWindow < n {
			return false, fmt.Sprintf("context %d < %d", c.Model.ContextWindow, n)
		}
	}
	return true, ""
}

// Place picks the cheapest eligible candidate. Ranking is cost, then node name
// for determinism — never a model's opinion (spec D5). The model id and
// endpoint name keys make the order total: two candidates tying on cost and
// node must not fall back to input order, because the store's query has no
// ORDER BY.
func Place(item WorkItem, cands []Candidate) Decision {
	sorted := append([]Candidate{}, cands...)
	sort.Slice(sorted, func(i, j int) bool {
		ci, cj := sorted[i].Model.CostInPerMtok, sorted[j].Model.CostInPerMtok
		if ci != cj {
			return ci < cj
		}
		if sorted[i].Node.Name != sorted[j].Node.Name {
			return sorted[i].Node.Name < sorted[j].Node.Name
		}
		if sorted[i].Model.ModelID != sorted[j].Model.ModelID {
			return sorted[i].Model.ModelID < sorted[j].Model.ModelID
		}
		return sorted[i].Endpoint.Name < sorted[j].Endpoint.Name
	})

	var rejects []Reject
	for i := range sorted {
		ok, why := Eligible(item, sorted[i])
		if ok {
			chosen := sorted[i]
			return Decision{
				OK:      true,
				Chosen:  &chosen,
				Reason:  reasonFor(item, chosen, len(rejects)),
				Rejects: rejects,
			}
		}
		rejects = append(rejects, Reject{NodeName: sorted[i].Node.Name, Why: why})
	}

	return Decision{
		OK:      false,
		Reason:  fmt.Sprintf("no eligible candidate for [%s]", strings.Join(item.Needs, ",")),
		Rejects: rejects,
	}
}

// unhealthyWhy is the refusal text for an endpoint whose health is not ok.
//
// It names the state, and then the reason the last probe recorded when there is
// one. /why prints this text verbatim, so the probe's own sentence has to travel
// with it: "down" alone sends the operator looking for a network fault when the
// answer was a rejected credential, and "unknown" alone does not say that
// nobody has asked yet (Task 9 decision 3). A state with no reason gets no
// dangling separator.
func unhealthyWhy(e Endpoint) string {
	why := "endpoint " + e.Name + " is " + e.Health
	if e.HealthReason != "" {
		why += ": " + e.HealthReason
	}
	return why
}

func reasonFor(item WorkItem, c Candidate, skipped int) string {
	return fmt.Sprintf("%s on %s via %s @%s (%.2f/Mtok), %d cheaper/earlier rejected",
		item.ID, c.Node.Name, c.Model.ModelID, c.Endpoint.Reach, c.Model.CostInPerMtok, skipped)
}

func tokenNeed(c string) (int, bool) {
	const p = "tokens:"
	if !strings.HasPrefix(c, p) {
		return 0, false
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimPrefix(c, p), "%d", &n); err != nil {
		return 0, false
	}
	return n, true
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
