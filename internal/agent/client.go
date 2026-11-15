// Package agent is the worker half of a node: it heartbeats, claims work,
// starts the run, delegates to an Executor, and reports the result.
//
// It never invents state. Every transition is a call to the board; a refusal
// is logged and left to the next cycle or to the reaper. The board's clock is
// the only clock that decides leases.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cti97/the-nightshift-manager/internal/board"
)

// requestTimeout bounds one board call. It is longer than the heartbeat
// interval and the claim poll so a slow board looks slow rather than dead.
const requestTimeout = 35 * time.Second

// maxResponseBytes is a sanity bound on a reply. Claim payloads are small; a
// body larger than this is a bug or a wrong endpoint, not work.
const maxResponseBytes = 4 << 20

// Client is the node's HTTP view of the board. It holds no mutable state, so
// the heartbeat goroutine and the claim loop can use it concurrently.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: requestTimeout},
	}
}

// post sends body as JSON and decodes the reply into out unless out is nil. A
// non-2xx reply is an error carrying the board's own message: the caller logs
// it and moves on.
func (c *Client) post(ctx context.Context, path string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s: %s", path, resp.Status, msg)
	}

	// Read to EOF before decoding. WriteJSON ends a reply with a newline, and a
	// bare json.Decoder leaves it unread, which costs the connection when the
	// body is closed.
	raw, err = io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: decode reply: %w", path, err)
	}
	return nil
}

// runPath builds /runs/{id}/{action}. The run id is escaped so a malformed id
// cannot turn into a different endpoint.
func runPath(runID, action string) string {
	return "/runs/" + url.PathEscape(runID) + "/" + action
}

func (c *Client) Heartbeat(ctx context.Context, req board.HeartbeatRequest) (board.HeartbeatResponse, error) {
	var out board.HeartbeatResponse
	err := c.post(ctx, "/nodes/heartbeat", req, &out)
	return out, err
}

// Claim asks for up to slots items. An idle node gets an empty list, not an
// error.
func (c *Client) Claim(ctx context.Context, slots int) ([]board.ClaimedItem, error) {
	var out []board.ClaimedItem
	err := c.post(ctx, "/claim", board.ClaimRequest{FreeSlots: slots}, &out)
	return out, err
}

// Start is the node's "I have begun executing" call. It is what moves the run
// and its item from claimed to running, which is the difference the reaper
// needs to tell an abandoned claim from a lost run.
func (c *Client) Start(ctx context.Context, runID string) error {
	return c.post(ctx, runPath(runID, "start"), map[string]any{}, nil)
}

func (c *Client) Report(ctx context.Context, runID string, req board.ReportRequest) (board.ReportResult, error) {
	var out board.ReportResult
	err := c.post(ctx, runPath(runID, "report"), req, &out)
	return out, err
}
