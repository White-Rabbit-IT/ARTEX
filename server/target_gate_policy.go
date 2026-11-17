package server

import (
	"errors"
	"os"
)

// RequireTargetGateEnv is an operator-owned deployment policy. When it is set
// to the literal value "1", ARTEX requires target approval for every task and
// conversation execution path. Other values intentionally leave the upstream
// compatibility behavior unchanged.
const RequireTargetGateEnv = "ARTEX_REQUIRE_TARGET_GATE"

const targetGateRequiredMessage = "the operator policy requires target approval; target_gate cannot be disabled"

// ErrTargetGateRequired is returned when an API caller explicitly tries to
// disable the target gate while the deployment policy requires it.
var ErrTargetGateRequired = errors.New(targetGateRequiredMessage)

// targetGateRequiredFromEnv deliberately accepts only the literal value "1".
// A typo, an inherited boolean-looking value, or an unset variable must retain
// the upstream-compatible policy-off behavior rather than changing it silently.
func targetGateRequiredFromEnv() bool {
	return os.Getenv(RequireTargetGateEnv) == "1"
}

// RequireTargetGate reports whether this Manager's process was started with
// the operator policy enabled. It is read-only for the lifetime of the process
// so a running task cannot lose its guard through a mutable setting.
func (m *Manager) RequireTargetGate() bool {
	return m != nil && m.requireTargetGate
}

// normalizeRequiredTargetGate preserves the legacy nil/default behavior when
// the policy is off. When it is on, omission becomes an explicit enabled gate
// and an explicit false value is rejected instead of being silently upgraded.
func (m *Manager) normalizeRequiredTargetGate(requested *bool) (*bool, error) {
	if !m.RequireTargetGate() {
		return requested, nil
	}
	if requested != nil && !*requested {
		return nil, ErrTargetGateRequired
	}
	if requested == nil {
		enabled := true
		return &enabled, nil
	}
	return requested, nil
}

// requireTargetGateUpdate stops a gated record being downgraded later. Nil
// means the caller did not modify the field and remains valid.
func (m *Manager) requireTargetGateUpdate(requested *bool) error {
	if m.RequireTargetGate() && requested != nil && !*requested {
		return ErrTargetGateRequired
	}
	return nil
}

// effectiveTargetGate is the runtime backstop for rows that predate the policy
// or were written directly by a database administrator. Direct database access
// is outside ARTEX's application trust boundary, but it must never turn an
// executing task or conversation into an ungated target path.
func (m *Manager) effectiveTargetGate(stored bool) bool {
	return stored || m.RequireTargetGate()
}
