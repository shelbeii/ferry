package engine

import (
	"encoding/json"
	"strings"

	"github.com/captain/ferry/internal/ai"
	"github.com/captain/ferry/internal/config"
)

const aiManualApprovalReason = "每项操作单独确认"

// Only the named app explicitly requested in the current message can be operated
// automatically. Model output and historical messages cannot grant this approval.
func (e *Engine) aiAutomaticApproval(t *config.Target, mode string, o AIOptions) func(ai.ToolCall, *ai.Approval) bool {
	if mode != AIConfirm || !t.AllowAIChanges() || t.Production || t.Plain || o.Decide != "" || (!o.AutoConfirmWarning && !o.AutoConfirmDanger) {
		return nil
	}
	message := strings.Join(strings.Fields(o.Message), " ")
	used := false
	return func(call ai.ToolCall, approval *ai.Approval) bool {
		if used || call.Name != "app_control" || approval.Once != aiManualApprovalReason || len(approval.Fields) != 0 {
			return false
		}
		var args struct {
			Apps   []string
			Action string
		}
		if json.Unmarshal(call.Input, &args) != nil || len(args.Apps) != 1 {
			return false
		}
		verb := map[string]string{"start": "启动", "restart": "重启", "stop": "停止"}[args.Action]
		name := args.Apps[0]
		if verb == "" || (message != verb+"应用 "+name && message != verb+"应用"+name && message != verb+" "+name) {
			return false
		}
		if args.Action == "stop" {
			if !o.AutoConfirmDanger || approval.Danger == "" {
				return false
			}
		} else if !o.AutoConfirmWarning || approval.Danger != "" {
			return false
		}
		apps, err := e.File.Select(t, args.Apps)
		if err != nil || len(apps) != 1 || apps[0].Name != name {
			return false
		}
		app := apps[0]
		managedUnit := e.File.Project + "-" + strings.ToLower(name) + ".service"
		if app.Job != nil || app.Type == "static" || app.Service.Unit != managedUnit || len(app.Service.Conflicts) != 0 {
			return false
		}
		used = true
		return true
	}
}
