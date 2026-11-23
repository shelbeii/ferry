package ai

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestApprovalDoesNotRunWhenPersistenceFails(t *testing.T) {
	conv := &Conversation{
		ID: "approval-test",
		Pending: &Pending{
			Calls:    []ToolCall{{ID: "one", Name: "test", Input: json.RawMessage("{}")}},
			Approval: Approval{ID: "pinned", State: "preview"},
			Scope:    "scope",
		},
	}
	ran := false
	loop := Loop{
		Scope:      "scope",
		ApprovalID: "pinned",
		Tools: []Tool{{Name: "test", Run: func(ctx context.Context, _ json.RawMessage) (string, error) {
			ran = true
			return ApprovedState(ctx), nil
		}}},
		Emit: func(Event) {},
		Save: func() error { return errors.New("storage unavailable") },
	}
	if err := loop.Decide(context.Background(), conv, Approve); err == nil || !strings.Contains(err.Error(), "storage unavailable") {
		t.Fatalf("approve error = %v", err)
	}
	if ran {
		t.Fatal("tool ran before its approval was recorded")
	}
}

func TestActiveConversationRetainedDuringCleanup(t *testing.T) {
	dir := t.TempDir()
	conv := &Conversation{ID: "approval-test"}
	if err := conv.Save(dir); err != nil {
		t.Fatal(err)
	}
	release, err := LockConversation(dir, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if n := CleanAll(dir); n != 0 {
		t.Fatalf("removed %d conversations, want 0", n)
	}
	if loaded, err := Load(dir, conv.ID); err != nil || loaded.Updated.IsZero() {
		t.Fatalf("active conversation lost: %v", err)
	}
}

func TestAutomaticApprovalRecordsDecisionBeforeRunning(t *testing.T) {
	conv := &Conversation{ID: "automatic-test"}
	call := ToolCall{ID: "app", Name: "app_control", Input: json.RawMessage(`{"apps":["api"],"action":"start"}`)}
	var emitted Event
	var ran bool
	loop := Loop{
		AutoApprove: func(ToolCall, *Approval) bool { return true },
		Tools: []Tool{{Name: call.Name,
			Approve: func(context.Context, json.RawMessage) *Approval { return &Approval{State: "checked"} },
			Run: func(ctx context.Context, _ json.RawMessage) (string, error) {
				ran = true
				if ApprovedState(ctx) != "checked" {
					t.Error("automatic approval must pass the approved state to the tool")
				}
				return "started", nil
			},
		}},
		Emit: func(ev Event) {
			if ev.Type == "call" {
				emitted = ev
			}
		},
		Save: func() error {
			if d := conv.Decisions[call.ID]; !ran && (d.Decision != AutoApproved || len(conv.Executing) != 1) {
				t.Error("automatic approval must be recorded before execution")
			}
			return nil
		},
	}
	paused, err := loop.runCalls(context.Background(), conv, []ToolCall{call}, nil, "")
	if err != nil || paused || !ran || !emitted.AutoApproved || conv.Pending != nil || conv.Decisions[call.ID].Decision != AutoApproved {
		t.Fatalf("automatic approval did not execute and record: paused=%v ran=%v event=%+v error=%v", paused, ran, emitted, err)
	}
}
