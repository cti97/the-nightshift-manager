package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cti97/the-nightshift-manager/internal/board"
)

// ToolFileItem is the one tool that talks to the board instead of the machine: a run asking for a
// child item (M7). It is not in Tools(): the static set is what every run may do, and filing needs
// the run's own identity, so the executor binds it per run.
const ToolFileItem = "file_item"

// filingClient is the slice of the board the tool needs. Narrow on purpose: a test can stand in for
// it without a server, and nothing the model does can reach the rest of the client.
type filingClient interface {
	FileItem(ctx context.Context, runID string, req board.FileItemRequest) (board.FileItemResult, error)
}

const fileItemSchema = `{
  "description": "File a follow-up item on the board when the work turns out to be bigger than this brief. The board decides whether it is allowed: it may create the item, may return the item that already exists for your client_ref, or may park this run to ask a human. Do not do the extra work inline instead — work filed here gets its own lease, price and audit.",
  "type": "object",
  "properties": {
    "title": {"type": "string", "description": "One line, like a ticket title. Max 512 bytes."},
    "brief": {"type": "string", "description": "What a fresh agent needs to know to do it, including anything you learned that it could not guess. Max 64 KiB."},
    "needs": {"type": "array", "items": {"type": "string"}, "description": "Capabilities the child requires. Every one must already be something this node can do, or the board refuses the filing. Max 16."},
    "client_ref": {"type": "string", "description": "Your own idempotency key for this filing. Send the same value when you retry, so a network error cannot file the same child twice. Max 128 bytes."}
  },
  "required": ["title", "brief"],
  "additionalProperties": false
}`

// filingTool binds the board client and the run id into a tool the loop can call. The closure is the
// reason the loop needs no knowledge of filing at all.
func filingTool(c filingClient, runID string) Tool {
	return Tool{Name: ToolFileItem, Schema: json.RawMessage(fileItemSchema),
		Run: func(ctx context.Context, _ *Sandbox, raw json.RawMessage) (string, error) {
			if err := guardCtx(ctx, ToolFileItem); err != nil {
				return "", err
			}
			var args struct {
				Title     string   `json:"title"`
				Brief     string   `json:"brief"`
				Needs     []string `json:"needs"`
				ClientRef string   `json:"client_ref"`
			}
			if err := decodeToolArgs(raw, &args); err != nil {
				return "", err
			}
			title := strings.TrimSpace(args.Title)
			brief := strings.TrimSpace(args.Brief)
			if title == "" || brief == "" {
				return "", fmt.Errorf("agent: file_item needs both a title and a brief; nothing was filed")
			}
			// The limits are checked here as well as on the board, so the model gets a sentence it can
			// act on rather than a 400 it has to guess about. The board remains the authority: it
			// enforces the same numbers, because a tool can be bypassed and a route cannot.
			if len(title) > maxFileTitleBytes {
				return "", fmt.Errorf("agent: file_item: the title is %d bytes and the limit is %d; say it in one line, nothing was filed", len(title), maxFileTitleBytes)
			}
			if len(brief) > maxFileBriefBytes {
				return "", fmt.Errorf("agent: file_item: the brief is %d bytes and the limit is %d; move the detail into the child's own work, nothing was filed", len(brief), maxFileBriefBytes)
			}
			if len(args.ClientRef) > maxFileRefBytes {
				return "", fmt.Errorf("agent: file_item: client_ref is %d bytes and the limit is %d; nothing was filed", len(args.ClientRef), maxFileRefBytes)
			}
			if len(args.Needs) > maxFileNeeds {
				return "", fmt.Errorf("agent: file_item: %d capabilities and the limit is %d; nothing was filed", len(args.Needs), maxFileNeeds)
			}

			res, err := c.FileItem(ctx, runID, board.FileItemRequest{
				Title: title, Brief: brief, Needs: args.Needs, ClientRef: strings.TrimSpace(args.ClientRef),
			})
			if err != nil {
				return "", fmt.Errorf("agent: file_item: %w", err)
			}
			switch {
			case res.Parked:
				// The run is parked now: the model's next turn may never happen, so the message says
				// what the human will be asked rather than pretending the filing failed.
				return fmt.Sprintf("the board parked this run to ask a human: %s. Nothing was filed yet; if a human approves, the run resumes and this filing may be retried.", res.Reason), nil
			case !res.Created:
				return fmt.Sprintf("already filed: item %s carries this client_ref. Do not file it again.", res.ItemID), nil
			default:
				return fmt.Sprintf("filed item %s (queued); it will be placed like any other work.", res.ItemID), nil
			}
		}}
}

const (
	// These mirror the board's own bounds. They are duplicated rather than exported from the board
	// because they are part of the TOOL's message to the model, and the board's numbers may tighten
	// without the model's sentence changing meaning.
	maxFileTitleBytes = 512
	maxFileBriefBytes = 64 << 10
	maxFileRefBytes   = 128
	maxFileNeeds      = 16
)
