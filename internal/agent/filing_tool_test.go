package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/cti97/the-nightshift-manager/internal/board"
)

// fakeFiler records what the tool asked the board for, and answers whatever the test needs.
type fakeFiler struct {
	mu     sync.Mutex
	reqs   []board.FileItemRequest
	runIDs []string
	result board.FileItemResult
	err    error
	calls  int
}

func (f *fakeFiler) FileItem(ctx context.Context, runID string, req board.FileItemRequest) (board.FileItemResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.runIDs = append(f.runIDs, runID)
	f.reqs = append(f.reqs, req)
	return f.result, f.err
}

func callFileItem(t *testing.T, tool Tool, body string) (string, error) {
	t.Helper()
	return tool.Run(context.Background(), nil, []byte(body))
}

// TestTheFilingToolCarriesTheRunsOwnIdentity: the model supplies the work; the run id comes from the
// executor. A tool that let the model name its run would let it file against someone else's work.
func TestTheFilingToolCarriesTheRunsOwnIdentity(t *testing.T) {
	f := &fakeFiler{result: board.FileItemResult{ItemID: "item-1", Created: true}}
	tool := filingTool(f, "run-42")
	out, err := callFileItem(t, tool, `{"title":"  split the migration  ","brief":"two halves","needs":["agentic"],"client_ref":"ref-1"}`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "item-1") || !strings.Contains(out, "filed") {
		t.Fatalf("the model was told: %q", out)
	}
	if len(f.runIDs) != 1 || f.runIDs[0] != "run-42" {
		t.Fatalf("run ids = %v, want run-42", f.runIDs)
	}
	if f.reqs[0].Title != "split the migration" {
		t.Fatalf("title = %q, want it trimmed", f.reqs[0].Title)
	}
	if f.reqs[0].ClientRef != "ref-1" {
		t.Fatalf("client_ref = %q", f.reqs[0].ClientRef)
	}
}

// TestTheFilingToolSaysWhenTheRunParked: a park is not a failure and not a success — the next turn
// may never happen, so the message has to say what the human will be asked.
func TestTheFilingToolSaysWhenTheRunParked(t *testing.T) {
	f := &fakeFiler{result: board.FileItemResult{Parked: true, Reason: "this item has already filed 5 children"}}
	out, err := callFileItem(t, filingTool(f, "run-1"), `{"title":"t","brief":"b"}`)
	if err != nil {
		t.Fatalf("a park is not an error: %v", err)
	}
	if !strings.Contains(out, "parked") || !strings.Contains(out, "5 children") {
		t.Fatalf("the model was told: %q", out)
	}
}

// TestTheFilingToolSaysWhenItAlreadyExisted: a retry must not look like a new filing, or the model
// files the work twice under a new ref.
func TestTheFilingToolSaysWhenItAlreadyExisted(t *testing.T) {
	f := &fakeFiler{result: board.FileItemResult{ItemID: "item-9", Created: false}}
	out, err := callFileItem(t, filingTool(f, "run-1"), `{"title":"t","brief":"b","client_ref":"same"}`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "already filed") || !strings.Contains(out, "item-9") {
		t.Fatalf("the model was told: %q", out)
	}
}

// TestTheFilingToolRefusesBadArgumentsBeforeTheBoard: the model gets a sentence it can act on, and a
// bad filing never becomes a request.
func TestTheFilingToolRefusesBadArgumentsBeforeTheBoard(t *testing.T) {
	cases := map[string]string{
		"no title":          `{"brief":"b"}`,
		"no brief":          `{"title":"t"}`,
		"too many needs":    `{"title":"t","brief":"b","needs":["a","b","c","d","e","f","g","h","i","j","k","l","m","n","o","p","q"]}`,
		"an overlong ref":   `{"title":"t","brief":"b","client_ref":"` + strings.Repeat("r", 129) + `"}`,
		"an overlong title": `{"title":"` + strings.Repeat("t", 513) + `","brief":"b"}`,
	}
	for label, body := range cases {
		f := &fakeFiler{result: board.FileItemResult{ItemID: "x", Created: true}}
		if _, err := callFileItem(t, filingTool(f, "run-1"), body); err == nil {
			t.Fatalf("%s was accepted", label)
		}
		if f.calls != 0 {
			t.Fatalf("%s reached the board", label)
		}
	}
}

// TestTheFilingToolPassesTheBoardsRefusalThrough: a 409 or a capability refusal is the model's cue to
// answer differently, so the error has to carry the board's words.
func TestTheFilingToolPassesTheBoardsRefusalThrough(t *testing.T) {
	f := &fakeFiler{err: errors.New("this node cannot provide \"sandbox:seatbelt\"")}
	_, err := callFileItem(t, filingTool(f, "run-1"), `{"title":"t","brief":"b"}`)
	if err == nil || !strings.Contains(err.Error(), "sandbox:seatbelt") {
		t.Fatalf("err = %v, want the board's reason", err)
	}
}

// TestTheFilingToolIsOfferedOnlyWhenItCanWork: a model told it can file will try, so the tool joins
// the per-run set only when the executor has a client — and it is bound to THAT run.
func TestTheFilingToolIsOfferedOnlyWhenItCanWork(t *testing.T) {
	bare := (&AgenticExecutor{}).toolSet("run-1")
	if _, ok := findTool(bare, ToolFileItem); ok {
		t.Fatal("an executor with no board client offers filing")
	}
	if len(bare) != len(Tools()) {
		t.Fatalf("the bare set has %d tools, want %d", len(bare), len(Tools()))
	}

	f := &fakeFiler{result: board.FileItemResult{ItemID: "x", Created: true}}
	withFiling := (&AgenticExecutor{Filer: f}).toolSet("run-7")
	tool, ok := findTool(withFiling, ToolFileItem)
	if !ok {
		t.Fatal("an executor with a client does not offer filing")
	}
	if len(withFiling) != len(bare)+1 {
		t.Fatalf("the per-run set has %d tools, want %d", len(withFiling), len(bare)+1)
	}
	// The bound run id is the one the tool posts against.
	if _, err := tool.Run(context.Background(), nil, []byte(`{"title":"t","brief":"b"}`)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(f.runIDs) != 1 || f.runIDs[0] != "run-7" {
		t.Fatalf("run ids = %v, want run-7", f.runIDs)
	}
}
