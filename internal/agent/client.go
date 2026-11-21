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
// StatusError is a refusal from the board, with the status and the board's own error code kept
// apart from the prose. A caller that only has the message has to parse English to tell a stale
// lease from a malformed body, and the two call for opposite behaviour: one is abandoned, the
// other is fixed and retried.
type StatusError struct {
	Path       string
	HTTPStatus int
	Status     string // "409 Conflict"
	Code       string // the board's error code, e.g. "stale_lease"; empty if the body was not JSON
	Body       string
}

func (e *StatusError) Error() string { return fmt.Sprintf("%s: %s: %s", e.Path, e.Status, e.Body) }

// Terminal reports whether retrying the same request can never succeed. A 4xx is a verdict about
// the request — a bad body, someone else's run, a lease that is gone — and repeating it is a hot
// loop with no chance. 408 and 429 are the exceptions: the board is asking to be asked later.
func (e *StatusError) Terminal() bool {
	return e.HTTPStatus >= 400 && e.HTTPStatus < 500 &&
		e.HTTPStatus != http.StatusRequestTimeout && e.HTTPStatus != http.StatusTooManyRequests
}

func newStatusError(path string, httpStatus int, status string, body []byte) *StatusError {
	se := &StatusError{
		Path: path, HTTPStatus: httpStatus, Status: status,
		Body: strings.TrimSpace(string(body)),
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil {
		se.Code = env.Error.Code
	}
	return se
}

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
		return newStatusError(path, resp.StatusCode, resp.Status, msg)
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

// get is post's read-only twin. The canary list is the only GET the agent makes with its node
// token today, and it deserves the same error shape: a 4xx is a verdict, not a glitch.
func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return newStatusError(path, resp.StatusCode, resp.Status, msg)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
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

// CanaryTargets is the endpoint list the steward may watch.
func (c *Client) CanaryTargets(ctx context.Context) ([]board.CanaryTarget, error) {
	var out struct {
		Endpoints []board.CanaryTarget `json:"endpoints"`
	}
	if err := c.get(ctx, "/canary", &out); err != nil {
		return nil, err
	}
	return out.Endpoints, nil
}

// ReportCanary posts one observation and says whether it changed the endpoint's health.
func (c *Client) ReportCanary(ctx context.Context, id string, rep board.CanaryReport) (board.CanaryResult, error) {
	var out board.CanaryResult
	err := c.post(ctx, "/canary/"+url.PathEscape(id), rep, &out)
	return out, err
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

// Escalate asks the board to park the run's item for a human decision. It is not
// a report: the board moves the item to needs_approval, closes the run as
// escalated, releases the lease, and prices the run from the token counts in the
// same body, so the day's ledger stays whole (M3 §11).
func (c *Client) Escalate(ctx context.Context, runID string, req board.EscalationRequest) error {
	return c.post(ctx, runPath(runID, "approval"), req, nil)
}

func (c *Client) Report(ctx context.Context, runID string, req board.ReportRequest) (board.ReportResult, error) {
	var out board.ReportResult
	err := c.post(ctx, runPath(runID, "report"), req, &out)
	return out, err
}

// Enroll registers this node with the board and returns the node id and the
// token it must use from then on. It is the one call a node makes before it has
// a node token, so it carries the enrollment credential instead — a secret the
// manager can use to register nodes and nothing else (never the admin token).
func Enroll(ctx context.Context, baseURL, enrollToken string, req board.RegisterRequest) (board.RegisterResponse, error) {
	var out board.RegisterResponse
	c := NewClient(baseURL, enrollToken)
	if err := c.post(ctx, "/nodes/register", req, &out); err != nil {
		return out, err
	}
	return out, nil
}
