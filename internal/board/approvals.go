package board

import "encoding/json"

// ApprovalRequest is why a run stopped and what it wants. Stored, not evented,
// so the approvals page is one query rather than a replay (M3 D1).
type ApprovalRequest struct {
	Action      string   `json:"action"`
	Reason      string   `json:"reason"`
	Options     []string `json:"options"`
	RunID       string   `json:"run_id"`
	RequestedBy string   `json:"requested_by"` // node id
	RequestedAt int64    `json:"requested_at"`
}

// MarshalJSON writes an empty Options list as [] rather than null, matching
// emptyJSONList: the stored bytes of a request must not depend on whether the
// caller passed nil or an empty slice, and a JSON consumer iterating the
// options must not meet null.
//
// The wire type is a distinct type, not an alias, so it carries no MarshalJSON
// method and encoding the fields cannot recurse.
func (a ApprovalRequest) MarshalJSON() ([]byte, error) {
	type wire ApprovalRequest
	w := wire(a)
	if len(w.Options) == 0 {
		w.Options = []string{}
	}
	return json.Marshal(w)
}
