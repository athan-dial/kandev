package service

import (
	"context"
	"testing"

	"github.com/kandev/kandev/internal/task/models"
)

// TestExpirePendingPermissionsForSession_MarksPendingExpired covers the
// turn-complete sweep: any permission_request message whose metadata.status
// is unset (or pending) must be flipped to "expired" so the UI no longer
// shows a stuck approval prompt.
func TestExpirePendingPermissionsForSession_MarksPendingExpired(t *testing.T) {
	svc, eventBus, repo := createTestService(t)
	ctx := context.Background()

	setupTestTask(t, repo)
	sessionID := setupTestSession(t, repo)
	turnID := setupTestTurn(t, repo, sessionID, "task-123", "turn-pending")

	// Tool call message that the permission request points at — created
	// before the permission_request in production. Seeding it here lets
	// ExpirePendingPermissionsForSession's tool-call cancellation actually
	// land on a tool_call row instead of bouncing back onto the permission
	// row via the GetMessageByToolCallID fallback.
	toolCall := &models.Message{
		ID:            "msg-toolcall",
		TaskSessionID: sessionID,
		TaskID:        "task-123",
		TurnID:        turnID,
		AuthorType:    models.MessageAuthorAgent,
		Type:          models.MessageTypeToolCall,
		Content:       "Bash",
		Metadata: map[string]interface{}{
			"tool_call_id": "tc-1",
			"status":       "pending_permission",
		},
	}
	if err := repo.CreateMessage(ctx, toolCall); err != nil {
		t.Fatalf("seed tool_call: %v", err)
	}

	// Pending permission (no status set yet — the state right after creation).
	pending := &models.Message{
		ID:            "msg-pending",
		TaskSessionID: sessionID,
		TaskID:        "task-123",
		TurnID:        turnID,
		AuthorType:    models.MessageAuthorAgent,
		Type:          models.MessageTypePermissionRequest,
		Content:       "Run something",
		Metadata: map[string]interface{}{
			"pending_id":   "pending-1",
			"tool_call_id": "tc-1",
		},
	}
	if err := repo.CreateMessage(ctx, pending); err != nil {
		t.Fatalf("seed pending: %v", err)
	}

	// Already-approved permission — must not be touched by the sweep.
	approved := &models.Message{
		ID:            "msg-approved",
		TaskSessionID: sessionID,
		TaskID:        "task-123",
		TurnID:        turnID,
		AuthorType:    models.MessageAuthorAgent,
		Type:          models.MessageTypePermissionRequest,
		Content:       "Already done",
		Metadata: map[string]interface{}{
			"pending_id": "pending-2",
			"status":     "approved",
		},
	}
	if err := repo.CreateMessage(ctx, approved); err != nil {
		t.Fatalf("seed approved: %v", err)
	}

	// A non-permission message must also be left alone — guards against
	// over-broad SQL filters.
	unrelated := &models.Message{
		ID:            "msg-text",
		TaskSessionID: sessionID,
		TaskID:        "task-123",
		TurnID:        turnID,
		AuthorType:    models.MessageAuthorAgent,
		Type:          models.MessageTypeMessage,
		Content:       "hi",
	}
	if err := repo.CreateMessage(ctx, unrelated); err != nil {
		t.Fatalf("seed unrelated: %v", err)
	}

	eventBus.ClearEvents()

	expired, err := svc.ExpirePendingPermissionsForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("ExpirePendingPermissionsForSession: %v", err)
	}
	if expired != 1 {
		t.Errorf("expired = %d, want 1", expired)
	}

	pendingAfter, err := repo.GetMessage(ctx, "msg-pending")
	if err != nil {
		t.Fatalf("reload pending: %v", err)
	}
	if status, _ := pendingAfter.Metadata["status"].(string); status != "expired" {
		t.Errorf("pending message status = %q, want %q", status, "expired")
	}

	approvedAfter, err := repo.GetMessage(ctx, "msg-approved")
	if err != nil {
		t.Fatalf("reload approved: %v", err)
	}
	if status, _ := approvedAfter.Metadata["status"].(string); status != "approved" {
		t.Errorf("approved message status = %q, want unchanged %q", status, "approved")
	}

	// The related tool_call must drop out of pending_permission so the UI
	// stops spinning on it.
	toolCallAfter, err := repo.GetMessage(ctx, "msg-toolcall")
	if err != nil {
		t.Fatalf("reload tool_call: %v", err)
	}
	if status, _ := toolCallAfter.Metadata["status"].(string); status != "error" {
		t.Errorf("tool_call status = %q, want %q", status, "error")
	}

	// We should see exactly one message.updated event for the expired
	// message (the unrelated tool_call cancellation may publish another
	// event, but the permission's own update must always fire).
	var sawExpiredUpdate bool
	for _, ev := range eventBus.GetPublishedEvents() {
		if ev.Type != "message.updated" {
			continue
		}
		data, _ := ev.Data.(map[string]any)
		if data == nil {
			continue
		}
		if id, _ := data["message_id"].(string); id == "msg-pending" {
			sawExpiredUpdate = true
		}
	}
	if !sawExpiredUpdate {
		t.Error("expected message.updated event for the expired permission message")
	}
}

// TestExpirePendingPermissionsForSession_NoPendingIsNoop guards the empty
// case: a session with no permission_request messages must return (0, nil)
// without publishing events or scanning more than the initial query.
func TestExpirePendingPermissionsForSession_NoPendingIsNoop(t *testing.T) {
	svc, eventBus, repo := createTestService(t)
	ctx := context.Background()

	setupTestTask(t, repo)
	sessionID := setupTestSession(t, repo)
	eventBus.ClearEvents()

	expired, err := svc.ExpirePendingPermissionsForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("ExpirePendingPermissionsForSession: %v", err)
	}
	if expired != 0 {
		t.Errorf("expired = %d, want 0 for empty session", expired)
	}
	if got := len(eventBus.GetPublishedEvents()); got != 0 {
		t.Errorf("published %d events, want 0", got)
	}
}
