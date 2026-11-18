package agent

import (
	"os"
	"strings"
)

// UntrustedOutputBoundaryEnabled reports whether Norma's untrusted-output boundary
// (agentcore.Options.UntrustedOutputBoundary: tool results framed between
// nonce-tagged markers) is on. It is on by default: unset, empty or any other
// value keeps it on; only ARTEX_UNTRUSTED_BOUNDARY set to "0", "false", "off"
// or "no" (case- and space-insensitive) turns it off.
//
// This read-only helper is shared with authenticated operator-status surfaces
// so a dashboard cannot drift from the policy that actually frames tool output.
func UntrustedOutputBoundaryEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ARTEX_UNTRUSTED_BOUNDARY"))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// Keep the internal spelling for existing call sites and tests while routing
// every decision through the exported source of truth above.
func untrustedBoundaryEnabled() bool { return UntrustedOutputBoundaryEnabled() }
