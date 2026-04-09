package shell

import "fmt"

// ApprovalRequest describes a command awaiting user approval.
type ApprovalRequest struct {
	Command     string
	Description string
	Risk        RiskLevel
	WorkDir     string
}

// FormatApproval creates a human-readable description of the approval request.
func FormatApproval(req ApprovalRequest) string {
	risk := string(req.Risk)
	switch req.Risk {
	case RiskHigh:
		risk = "HIGH"
	case RiskMedium:
		risk = "MEDIUM"
	case RiskLow:
		risk = "LOW"
	}

	msg := fmt.Sprintf("[%s Risk] %s", risk, req.Command)
	if req.Description != "" {
		msg += fmt.Sprintf("\n  Description: %s", req.Description)
	}
	if req.WorkDir != "" {
		msg += fmt.Sprintf("\n  Working directory: %s", req.WorkDir)
	}
	return msg
}
