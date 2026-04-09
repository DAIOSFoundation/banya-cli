package shell

import (
	"fmt"
	"strings"
)

// RiskLevel indicates how dangerous a command might be.
type RiskLevel string

const (
	RiskLow    RiskLevel = "low"
	RiskMedium RiskLevel = "medium"
	RiskHigh   RiskLevel = "high"
)

// highRiskPatterns are commands or patterns that are always high risk.
var highRiskPatterns = []string{
	"rm -rf",
	"mkfs",
	"dd if=",
	"chmod -R 777",
	"> /dev/",
	":(){ :|:&};:",
	"shutdown",
	"reboot",
	"init 0",
	"kill -9 1",
	"systemctl stop",
	"iptables -F",
}

// mediumRiskPatterns require elevated attention.
var mediumRiskPatterns = []string{
	"rm ",
	"mv ",
	"chmod",
	"chown",
	"sudo",
	"apt ",
	"pip install",
	"npm install -g",
	"docker rm",
	"docker stop",
	"git push",
	"git reset",
	"kill",
}

// AssessRisk evaluates the risk level of a command.
func AssessRisk(command string) RiskLevel {
	lower := strings.ToLower(command)

	for _, pattern := range highRiskPatterns {
		if strings.Contains(lower, pattern) {
			return RiskHigh
		}
	}

	for _, pattern := range mediumRiskPatterns {
		if strings.Contains(lower, pattern) {
			return RiskMedium
		}
	}

	return RiskLow
}

// ValidateCommand checks if a command should be allowed to execute.
func ValidateCommand(e *Executor, command string) error {
	if e.IsBlocked(command) {
		return fmt.Errorf("command is blocked by policy: %s", command)
	}
	return nil
}

// NeedsApproval determines if a command requires user confirmation.
func NeedsApproval(e *Executor, command string) bool {
	// Auto-approve mode skips approval for low-risk commands
	if e.cfg.AutoApprove {
		risk := AssessRisk(command)
		if risk == RiskLow {
			return false
		}
		if risk == RiskMedium && e.IsAllowed(command) {
			return false
		}
	}

	// Always approve high-risk commands
	if AssessRisk(command) == RiskHigh {
		return true
	}

	// Allowed commands don't need approval
	if e.IsAllowed(command) {
		return false
	}

	// Default: require approval
	return true
}
