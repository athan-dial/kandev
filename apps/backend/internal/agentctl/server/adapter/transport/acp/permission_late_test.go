package acp

import (
	"context"
	"fmt"
	"testing"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kandev/kandev/internal/agentctl/types/streams"
)

// TestHandlePermissionRequest_LateForCompletedToolCall reproduces the
// "stuck pending approval" bug: an agent emits session/request_permission
// after the same tool_call has already reported a terminal status.
//
// The adapter must auto-cancel the request (returning Cancelled=true) and
// must NOT forward it to the upstream handler — otherwise the orchestrator
// creates a permission_request message that the user can never resolve.
func TestHandlePermissionRequest_LateForCompletedToolCall(t *testing.T) {
	a := newTestAdapter()

	// Hook a handler that asserts it never gets called: a late permission for
	// a completed tool must short-circuit before reaching the upstream handler.
	handlerCalled := false
	a.permissionHandler = func(ctx context.Context, req *PermissionRequest) (*PermissionResponse, error) {
		handlerCalled = true
		return &PermissionResponse{OptionID: "allow"}, nil
	}

	const toolCallID = "tc-late"

	// Seed a tool_call and then drive it to a terminal status. After this
	// point the activeToolCalls entry is gone but completedToolCalls retains
	// the ID — exactly the state the bug requires.
	seedExecuteToolCall(t, a, toolCallID)
	completed := acp.ToolCallStatus("completed")
	tcu := &acp.SessionToolCallUpdate{
		ToolCallId: acp.ToolCallId(toolCallID),
		Status:     &completed,
		RawOutput:  "ok",
	}
	if ev := a.convertToolCallResultUpdate("session-1", tcu); ev == nil {
		t.Fatalf("seed: convertToolCallResultUpdate returned nil")
	}

	// Drain seed events so the assertion below only sees what handlePermissionRequest emits.
	drainEvents(a)

	resp, err := a.handlePermissionRequest(context.Background(), &PermissionRequest{
		SessionID:  "session-1",
		ToolCallID: toolCallID,
		Title:      "Run something",
		Options: []PermissionOption{
			{OptionID: "allow", Kind: "allow_once", Name: "Allow"},
		},
	})

	if err != nil {
		t.Fatalf("handlePermissionRequest returned error: %v", err)
	}
	if resp == nil {
		t.Fatal("handlePermissionRequest returned nil response")
	}
	if !resp.Cancelled {
		t.Errorf("expected Cancelled=true for late permission on completed tool call, got %+v", resp)
	}
	if handlerCalled {
		t.Error("upstream permission handler must not be called for late permissions")
	}

	// And no synthetic tool_call event should leak out — the original
	// tool_call already exists in the UI in a terminal state.
	for _, ev := range drainEvents(a) {
		if ev.Type == streams.EventTypeToolCall && ev.ToolCallID == toolCallID {
			t.Errorf("unexpected synthetic tool_call event for already-completed tool: %+v", ev)
		}
	}
}

// TestHandlePermissionRequest_NormalForActiveToolCall guards the happy path:
// a permission request for a still-active tool call must be forwarded to the
// upstream handler and its response returned to the agent.
func TestHandlePermissionRequest_NormalForActiveToolCall(t *testing.T) {
	a := newTestAdapter()

	a.permissionHandler = func(ctx context.Context, req *PermissionRequest) (*PermissionResponse, error) {
		return &PermissionResponse{OptionID: "allow"}, nil
	}

	const toolCallID = "tc-active"
	seedExecuteToolCall(t, a, toolCallID)
	drainEvents(a)

	resp, err := a.handlePermissionRequest(context.Background(), &PermissionRequest{
		SessionID:  "session-1",
		ToolCallID: toolCallID,
		Title:      "Run something",
		Options: []PermissionOption{
			{OptionID: "allow", Kind: "allow_once", Name: "Allow"},
		},
	})
	if err != nil {
		t.Fatalf("handlePermissionRequest returned error: %v", err)
	}
	if resp == nil || resp.Cancelled {
		t.Fatalf("expected forwarded response with Cancelled=false, got %+v", resp)
	}
	if resp.OptionID != "allow" {
		t.Errorf("expected upstream OptionID=allow, got %q", resp.OptionID)
	}
}

// TestMarkToolCallCompleted_FIFOEviction guards the bound: completedToolCalls
// must not grow unbounded across a long-running adapter. Once the FIFO is
// over capacity, the oldest entries are evicted so the set size stays at the
// cap.
func TestMarkToolCallCompleted_FIFOEviction(t *testing.T) {
	a := newTestAdapter()

	// Push capacity+50 IDs; the oldest 50 should be evicted.
	const overflow = 50
	a.mu.Lock()
	for i := 0; i < maxCompletedToolCalls+overflow; i++ {
		a.markToolCallCompletedLocked(toolCallIDForIndex(i))
	}
	a.mu.Unlock()

	if got := len(a.completedToolCalls); got != maxCompletedToolCalls {
		t.Errorf("len(completedToolCalls) = %d, want %d", got, maxCompletedToolCalls)
	}
	if got := len(a.completedToolCallsFIFO); got != maxCompletedToolCalls {
		t.Errorf("len(completedToolCallsFIFO) = %d, want %d", got, maxCompletedToolCalls)
	}

	// The first `overflow` IDs must have been evicted; the last
	// `maxCompletedToolCalls` IDs must still be present.
	for i := 0; i < overflow; i++ {
		if a.isToolCallCompleted(toolCallIDForIndex(i)) {
			t.Errorf("ID #%d should have been evicted", i)
		}
	}
	for i := overflow; i < maxCompletedToolCalls+overflow; i++ {
		if !a.isToolCallCompleted(toolCallIDForIndex(i)) {
			t.Errorf("ID #%d should still be present", i)
		}
	}
}

func toolCallIDForIndex(i int) string {
	return fmt.Sprintf("tc-%04d", i)
}
