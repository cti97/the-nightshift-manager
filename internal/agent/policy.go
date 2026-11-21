// internal/agent/policy.go
package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Verdict is what a node's policy says about one tool.
type Verdict string

const (
	VerdictAllow Verdict = "allow"
	VerdictDeny  Verdict = "deny"
	VerdictAsk   Verdict = "ask"
)

// Policy is a node's local rule about which tools may run (spec §22.1, the review's A4). It exists
// because request_approval is a tool the model CHOOSES: "anything real gets approved" is a
// convention a model can decline, and an unattended run needs a wall it cannot decline.
//
// The zero Policy allows everything, so a node with no policy file behaves exactly as it did
// before the policy existed. A safety layer that arrived switched on would have changed every
// existing deployment's semantics without saying so, and the first symptom would be runs parking
// for a human who is asleep.
type Policy struct {
	// Default is what Decide answers for a tool the file does not name. Empty means allow.
	Default Verdict
	rules   map[string]Verdict
}

// ParsePolicy reads a policy document:
//
//	{"default": "allow", "shell": "deny", "write_file": "ask"}
//
// Every key must be a tool this binary has, and every value one of allow/deny/ask. A typo in
// either direction fails at load time: a policy that silently ignored "sl" would be a wall with a
// hole in it, and the operator would believe the node was locked down.
func ParsePolicy(raw []byte) (Policy, error) {
	var doc map[string]string
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Policy{}, fmt.Errorf("policy is not a JSON object of tool: verdict: %w", err)
	}
	p := Policy{rules: make(map[string]Verdict, len(doc))}
	for name, verdict := range doc {
		v := Verdict(strings.TrimSpace(verdict))
		switch v {
		case VerdictAllow, VerdictDeny, VerdictAsk:
		default:
			return Policy{}, fmt.Errorf("policy: verdict %q for %q is not allow, deny or ask", verdict, name)
		}
		if name == "default" {
			p.Default = v
			continue
		}
		if _, ok := Find(name); !ok {
			return Policy{}, fmt.Errorf("policy: %q is not a tool this binary has (want one of %s)",
				name, strings.Join(toolNames(), ", "))
		}
		if name == ToolFinish && v == VerdictDeny {
			// Denying finish would leave a run with no way to end: the loop would keep asking the
			// model to continue until the step cap. A run that must not proceed is stopped by the
			// spend ceiling or by a deny on the tool it would use, not by removing its exit.
			return Policy{}, fmt.Errorf("policy: finish cannot be denied; deny the tools a run may use instead, or stop it with the spend ceiling")
		}
		p.rules[name] = v
	}
	return p, nil
}

// Decide answers for one tool, with the rule that decided so a refusal can name it.
func (p Policy) Decide(tool string) (Verdict, string) {
	if v, ok := p.rules[tool]; ok {
		return v, fmt.Sprintf("the policy names %s", tool)
	}
	if p.Default != "" {
		return p.Default, "the policy's default"
	}
	return VerdictAllow, "no policy"
}

// String is the one-line summary the agent logs at start, so an operator reading a log knows what
// wall the run was behind without opening the file.
func (p Policy) String() string {
	if len(p.rules) == 0 && p.Default == "" {
		return "no policy: every tool is allowed"
	}
	def := p.Default
	if def == "" {
		def = VerdictAllow
	}
	names := make([]string, 0, len(p.rules))
	for name := range p.rules {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+"="+string(p.rules[name]))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("default %s", def)
	}
	return fmt.Sprintf("default %s; %s", def, strings.Join(parts, ", "))
}

// toolNames is the closed set, sorted, for error messages that let an operator fix a typo without
// opening the source.
func toolNames() []string {
	tools := Tools()
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	return names
}

// policyRefusal is what a model is told when the policy denies a tool. It is a tool result, not a
// failed run: a model that reads it can finish or try another approach, and one that cannot has
// still been stopped, which is the point.
func policyRefusal(tool, rule string) string {
	return fmt.Sprintf("refused: this node's policy denies %s (%s). This is not a request you can retry: "+
		"the wall is on the machine, not in this conversation. Finish with what you have, or report why "+
		"the work cannot be done without it.", tool, rule)
}
