package tools

import (
	"fmt"
	"time"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// ── trigger_alert ──────────────────────────────────────────────────────────

type TriggerAlertArgs struct {
	Severity string `json:"severity" desc:"Alert severity: critical, high, medium, or low"`
	Title    string `json:"title" desc:"Short title for the alert"`
	Summary  string `json:"summary" desc:"Detailed summary of what triggered the alert"`
	Channel  string `json:"channel" desc:"Notification channel (e.g. slack, pagerduty, email)"`
}

type TriggerAlertResult struct {
	AlertID   string `json:"alert_id"`
	Status    string `json:"status"`
	Timestamp string `json:"timestamp"`
}

func NewTriggerAlertTool() (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "trigger_alert",
		Description: "Send an alert notification to the specified channel. Returns alert ID and status.",
	}, triggerAlertHandler)
}

func triggerAlertHandler(_ tool.Context, args TriggerAlertArgs) (TriggerAlertResult, error) {
	// Mock: log the alert and return a confirmation
	alertID := fmt.Sprintf("alert_%d", time.Now().UnixNano()%100000)

	return TriggerAlertResult{
		AlertID:   alertID,
		Status:    "sent",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}, nil
}
