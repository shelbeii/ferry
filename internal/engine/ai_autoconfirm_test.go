package engine

import (
	"encoding/json"
	"testing"

	"github.com/captain/ferry/internal/ai"
	"github.com/captain/ferry/internal/config"
)

func TestAIAutomaticApprovalScope(t *testing.T) {
	e := &Engine{File: &config.File{Project: "demo", Apps: []*config.App{
		{Name: "api", Type: "binary", Service: config.Service{Unit: "demo-api.service"}},
		{Name: "other", Type: "binary", Service: config.Service{Unit: "demo-other.service"}},
		{Name: "custom", Type: "binary", Service: config.Service{Unit: "custom.service"}},
	}}}
	restart := ai.ToolCall{Name: "app_control", Input: json.RawMessage(`{"apps":["api"],"action":"restart"}`)}
	stop := ai.ToolCall{Name: "app_control", Input: json.RawMessage(`{"apps":["api"],"action":"stop"}`)}
	warning := &ai.Approval{Once: aiManualApprovalReason}
	danger := &ai.Approval{Once: aiManualApprovalReason, Danger: "停止后应用不再提供服务"}
	warnOptions := AIOptions{Message: "重启应用 api", AutoConfirmWarning: true}
	stopOptions := AIOptions{Message: "停止应用api", AutoConfirmDanger: true}
	target := &config.Target{Name: "staging"}
	for _, tc := range []struct {
		name     string
		target   *config.Target
		mode     string
		options  AIOptions
		call     ai.ToolCall
		approval *ai.Approval
		want     bool
	}{
		{"restart explicitly requested", target, AIConfirm, warnOptions, restart, warning, true},
		{"stop explicitly requested", target, AIConfirm, stopOptions, stop, danger, true},
		{"off by default", target, AIConfirm, AIOptions{Message: warnOptions.Message}, restart, warning, false},
		{"production", &config.Target{Production: true}, AIConfirm, warnOptions, restart, warning, false},
		{"plain connection", &config.Target{Plain: true}, AIConfirm, warnOptions, restart, warning, false},
		{"read only", target, AIReadOnly, warnOptions, restart, warning, false},
		{"unavailable app on target", &config.Target{Apps: []string{"other"}}, AIConfirm, warnOptions, restart, warning, false},
		{"different level", target, AIConfirm, AIOptions{Message: stopOptions.Message, AutoConfirmWarning: true}, stop, danger, false},
		{"inferred action", target, AIConfirm, AIOptions{Message: "看看 api 是否正常", AutoConfirmDanger: true}, stop, danger, false},
		{"different app", target, AIConfirm, AIOptions{Message: "重启应用 other", AutoConfirmWarning: true}, restart, warning, false},
		{"custom unit", target, AIConfirm, AIOptions{Message: "重启应用 custom", AutoConfirmWarning: true}, ai.ToolCall{Name: "app_control", Input: json.RawMessage(`{"apps":["custom"],"action":"restart"}`)}, warning, false},
		{"a batch of apps", target, AIConfirm, warnOptions, ai.ToolCall{Name: "app_control", Input: json.RawMessage(`{"apps":["api","other"],"action":"restart"}`)}, warning, false},
		{"custom command", target, AIConfirm, warnOptions, ai.ToolCall{Name: "run_command", Input: restart.Input}, warning, false},
		{"dangerous database change", target, AIConfirm, stopOptions, ai.ToolCall{Name: "db_execute", Input: stop.Input}, danger, false},
		{"app-specific confirmation rule", target, AIConfirm, warnOptions, restart, &ai.Approval{Once: "另有确认要求"}, false},
		{"decision continuation", target, AIConfirm, AIOptions{Message: warnOptions.Message, AutoConfirmWarning: true, Decide: ai.Approve}, restart, warning, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			approve := e.aiAutomaticApproval(tc.target, tc.mode, tc.options)
			if got := approve != nil && approve(tc.call, tc.approval); got != tc.want {
				t.Fatalf("automatic approval = %v, want %v", got, tc.want)
			}
			if tc.want && approve(tc.call, tc.approval) {
				t.Fatal("automatic approval must be single-use within a user message")
			}
		})
	}
}
