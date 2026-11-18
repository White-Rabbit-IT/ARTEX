package db

import (
	"fmt"
	"sort"
	"strings"
)

// OperatorReadinessTemplateStatus is a non-content summary of a fixed template
// bootstrap set. It deliberately carries no template text, identifiers, or
// operator-created data.
type OperatorReadinessTemplateStatus struct {
	Required int  `json:"required"`
	Matching int  `json:"matching"`
	Complete bool `json:"complete"`
	Conflict bool `json:"conflict"`
}

const (
	OperatorReadinessCalibrationMissing         = "missing"
	OperatorReadinessCalibrationPresentTerminal = "present_terminal"
	OperatorReadinessCalibrationUnexpectedState = "unexpected_state"
)

// OperatorReadinessSyntheticCalibrationState returns a deliberately modest
// status for CAL-00. "present_terminal" proves only that exactly one persisted
// CAL-00 row is terminal; it does not claim to re-validate its entire graph or
// make any provider, tool, target, or vulnerability assertion.
func (d *DB) OperatorReadinessSyntheticCalibrationState() (string, error) {
	rows, err := d.Query(`
SELECT status, paused, queued
FROM tasks
WHERE system_kind=$1
  AND deleted_at IS NULL
ORDER BY id
LIMIT 2`, SyntheticCalibrationSystemKind)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	count := 0
	terminal := false
	for rows.Next() {
		count++
		var status string
		var paused, queued bool
		if err := rows.Scan(&status, &paused, &queued); err != nil {
			return "", err
		}
		terminal = status == "done" && !paused && !queued
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if count == 0 {
		return OperatorReadinessCalibrationMissing, nil
	}
	if count == 1 && terminal {
		return OperatorReadinessCalibrationPresentTerminal, nil
	}
	return OperatorReadinessCalibrationUnexpectedState, nil
}

// OperatorReadinessWorkflowTemplateStatus compares a server-owned fixed
// template set with stored rows without returning any template content. A
// same-name user template with different normalized content remains untouched
// and is reported only as a conflict/incomplete bootstrap state.
func (d *DB) OperatorReadinessWorkflowTemplateStatus(inputs []TaskTemplateInput) (OperatorReadinessTemplateStatus, error) {
	status := OperatorReadinessTemplateStatus{Required: len(inputs)}
	type expectedTemplate struct {
		input     TaskTemplateInput
		nkey      string
		rulesJSON []byte
	}
	expected := make(map[string]expectedTemplate, len(inputs))
	for _, input := range inputs {
		normalized, nkey, err := normalizeTaskTemplateInput(input)
		if err != nil {
			return status, err
		}
		if _, duplicate := expected[nkey]; duplicate {
			return status, fmt.Errorf("%w: duplicate template name %q", ErrTaskTemplateInvalid, normalized.Name)
		}
		rulesJSON, err := marshalTemplateRules(normalized.InterceptRules)
		if err != nil {
			return status, err
		}
		expected[nkey] = expectedTemplate{input: normalized, nkey: nkey, rulesJSON: rulesJSON}
	}

	if len(expected) == 0 {
		status.Complete = true
		return status, nil
	}

	// The dashboard polls this status frequently. Read only the fixed nkeys
	// rather than decoding every operator template (which may be numerous and
	// unrelated to bootstrap readiness).
	keys := make([]string, 0, len(expected))
	placeholders := make([]string, 0, len(expected))
	args := make([]any, 0, len(expected))
	for nkey := range expected {
		keys = append(keys, nkey)
	}
	// A deterministic query makes telemetry and test output stable. The keys
	// remain query parameters; only position markers are constructed here.
	sort.Strings(keys)
	for index, nkey := range keys {
		placeholders = append(placeholders, fmt.Sprintf("$%d", index+1))
		args = append(args, nkey)
	}
	rows, err := d.Query(`SELECT `+taskTemplateCols+` FROM task_templates WHERE nkey IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return status, err
	}
	defer rows.Close()
	for rows.Next() {
		template, err := scanTaskTemplate(rows)
		if err != nil {
			return status, err
		}
		spec, needed := expected[template.NKey]
		if !needed {
			continue
		}
		if taskTemplateMatchesInput(template, spec.input, spec.rulesJSON) {
			status.Matching++
		} else {
			status.Conflict = true
		}
	}
	if err := rows.Err(); err != nil {
		return status, err
	}
	status.Complete = status.Matching == status.Required && !status.Conflict
	return status, nil
}

// OperatorReadinessHasProfileFormat answers only whether one or more profile
// rows use the requested format. It intentionally does not load a profile, so
// endpoint, model, proxy, API-key hint, and credentials never reach an
// operator-readiness response.
func (d *DB) OperatorReadinessHasProfileFormat(format string) (bool, error) {
	var present bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM llm_profiles WHERE format=$1)`, format).Scan(&present); err != nil {
		return false, err
	}
	return present, nil
}
