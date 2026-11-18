package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// SyntheticAgentCalibrationTaskName is the fixed task displayed by the
// dedicated agent-calibration action. It is not operator-authored target text.
const SyntheticAgentCalibrationTaskName = "ARTEX CAL-AGENT-01 — bounded synthetic agent calibration"

// SyntheticAgentCalibrationTaskTimeoutSeconds matches the fixed Worker wall
// clock. The policy does not inherit ARTEX_* task/intent budget environment
// variables, generic planner settlement, or profile-provided turn limits.
const SyntheticAgentCalibrationTaskTimeoutSeconds = 90

const (
	syntheticAgentCalibrationMarker      = "artex-cal-agent-synthetic-v1"
	syntheticAgentCalibrationSource      = "synthetic_agent_calibration"
	syntheticAgentCalibrationDescription = "Fixed synthetic agent calibration. It uses one code-owned receipt intent only; it has no target, URL, source tree, credential, traffic, browser, scanner, shell, filesystem, MCP, or skill input."
	syntheticAgentCalibrationGoal        = "Record one bounded synthetic-agent receipt using only the product-owned synthetic receipt capability."
	syntheticAgentCalibrationIntent      = "Record the fixed synthetic receipt. Do not inspect, create, contact, scan, fetch, execute, read, write, or modify anything."
)

var ErrSyntheticAgentCalibrationConflict = errors.New("synthetic agent calibration task is not canonical")

// SyntheticAgentCalibrationResult is the fixed database receipt returned by
// EnsureSyntheticAgentCalibrationTask. IntentID is created before any engine
// work, so the specialized one-worker engine has no dynamic setup or replay
// ambiguity. AlreadyPresent distinguishes a safe replay from a new task.
type SyntheticAgentCalibrationResult struct {
	Task           *Task
	IntentID       int64
	AlreadyPresent bool
}

// SyntheticAgentCalibrationRun is the sole hand-off from the transactional
// start/retry gate to the dedicated engine. ClaimToken is an in-process lease,
// not a UI/API value and must never be persisted in activity metadata.
//
// Started=false is a harmless observation of an already-running or terminal
// task. It never authorizes a second provider call.
type SyntheticAgentCalibrationRun struct {
	Task       *Task
	IntentID   int64
	ClaimToken string
	Attempt    int
	Retried    bool
	Started    bool
}

type syntheticAgentCalibrationIntentState struct {
	id            int64
	state         string
	owner         sql.NullString
	token         sql.NullString
	blockedReason sql.NullString
	completedAt   sql.NullTime
	heartbeatAt   sql.NullTime
}

// EnsureSyntheticAgentCalibrationTask creates exactly one fixed, runnable
// synthetic calibration task for one reviewed profile. It accepts no caller
// supplied goal, target, URL, source, credential, tool, skill, or task options.
// The caller may only choose the already-reviewed LLM profile by database ID;
// provider-route validation belongs to the server's dedicated admission path.
func (d *DB) EnsureSyntheticAgentCalibrationTask(llmProfileID int64) (*SyntheticAgentCalibrationResult, error) {
	if llmProfileID <= 0 {
		return nil, fmt.Errorf("%w: LLM profile id is required", ErrSyntheticAgentCalibrationConflict)
	}

	var lastErr error
	for attempt := 0; attempt < syntheticCalibrationSerializationMaxAttempts; attempt++ {
		stored, err := d.ensureSyntheticAgentCalibrationTaskOnce(llmProfileID)
		if err == nil {
			task, err := d.GetTask(stored.taskID)
			if err != nil {
				return nil, fmt.Errorf("read synthetic agent calibration task: %w", err)
			}
			if task == nil {
				return nil, fmt.Errorf("%w: task %d disappeared after commit", ErrSyntheticAgentCalibrationConflict, stored.taskID)
			}
			return &SyntheticAgentCalibrationResult{
				Task:           task,
				IntentID:       stored.intentID,
				AlreadyPresent: stored.alreadyPresent,
			}, nil
		}
		lastErr = err
		if !isSyntheticCalibrationSerializationFailure(err) {
			return nil, err
		}
	}
	return nil, lastErr
}

type syntheticAgentCalibrationStored struct {
	taskID, intentID int64
	alreadyPresent   bool
}

func (d *DB) ensureSyntheticAgentCalibrationTaskOnce(llmProfileID int64) (*syntheticAgentCalibrationStored, error) {
	tx, err := d.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // a successful commit makes this a no-op

	// This key is fixed and internal. The partial unique index from migration
	// 0007 remains a second line of defence if a future caller bypasses the lock.
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext($1)::bigint)`, "artex-calibration:synthetic-agent:v1"); err != nil {
		return nil, fmt.Errorf("lock synthetic agent calibration: %w", err)
	}
	existing, found, err := syntheticAgentCalibrationExisting(tx, llmProfileID)
	if err != nil {
		return nil, err
	}
	if found {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return existing, nil
	}
	// Existing-task lookup above locks the task before this profile reference,
	// matching DeleteProfile's task -> profile lock order. On first creation
	// there is no task row yet, so a concurrent profile delete simply wins and
	// this reference check fails without creating a fallback-bound task.
	if err := lockLLMProfileForReference(tx, &llmProfileID); err != nil {
		return nil, fmt.Errorf("lock synthetic agent calibration profile: %w", err)
	}

	var explorationID int64
	if err := tx.QueryRow(`
INSERT INTO explorations(description, goal)
VALUES ($1, $2)
RETURNING id`, syntheticAgentCalibrationDescription, syntheticAgentCalibrationGoal).Scan(&explorationID); err != nil {
		return nil, fmt.Errorf("create synthetic agent calibration exploration: %w", err)
	}

	originPayload, err := syntheticAgentCalibrationOriginPayload()
	if err != nil {
		return nil, err
	}
	var originFactID int64
	if err := tx.QueryRow(`
INSERT INTO exploration_nodes(exploration_id, kind, payload, priority, state, origin)
VALUES ($1, $2, $3, 0, $4, $5)
RETURNING id`, explorationID, KindFact, originPayload, StateOrigin, syntheticAgentCalibrationSource).Scan(&originFactID); err != nil {
		return nil, fmt.Errorf("create synthetic agent calibration origin: %w", err)
	}

	var taskID int64
	if err := tx.QueryRow(`
INSERT INTO tasks(
    name, description, goal, exploration_id,
    status, paused, queued,
    llm_profile_id, active_llm_profile_id,
    timeout_seconds, plan_heartbeat_seconds, plan_quiet_mode,
    coverage_enabled, llm_rotate, target_gate, execution_policy
)
VALUES ($1, $2, $3, $4, 'created', false, false, $5, $5, $6, $7, true, false, false, true, $8)
RETURNING id`,
		SyntheticAgentCalibrationTaskName,
		syntheticAgentCalibrationDescription,
		syntheticAgentCalibrationGoal,
		explorationID,
		llmProfileID,
		SyntheticAgentCalibrationTaskTimeoutSeconds,
		MinPlanHeartbeatSeconds,
		SyntheticAgentCalibrationExecutionPolicy,
	).Scan(&taskID); err != nil {
		return nil, fmt.Errorf("create synthetic agent calibration task: %w", err)
	}
	if err := insertTaskLLMProfiles(tx, taskID, []int64{llmProfileID}); err != nil {
		return nil, fmt.Errorf("bind synthetic agent calibration profile: %w", err)
	}

	intentPayload, err := syntheticAgentCalibrationIntentPayload()
	if err != nil {
		return nil, err
	}
	var intentID int64
	if err := tx.QueryRow(`
INSERT INTO exploration_nodes(exploration_id, kind, payload, priority, state, origin)
VALUES ($1, $2, $3, 0, 'open', $4)
RETURNING id`, explorationID, KindIntent, intentPayload, syntheticAgentCalibrationSource).Scan(&intentID); err != nil {
		return nil, fmt.Errorf("create synthetic agent calibration intent: %w", err)
	}
	if _, err := tx.Exec(`
INSERT INTO exploration_edges(exploration_id, src_id, rel, dst_id)
VALUES ($1, $2, $3, $4)`, explorationID, originFactID, RelDerivedFrom, intentID); err != nil {
		return nil, fmt.Errorf("link synthetic agent calibration intent: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &syntheticAgentCalibrationStored{taskID: taskID, intentID: intentID}, nil
}

func syntheticAgentCalibrationOriginPayload() (string, error) {
	raw, err := json.Marshal(map[string]any{
		"summary":            "Synthetic agent calibration origin",
		"description":        syntheticAgentCalibrationDescription,
		"goal":               syntheticAgentCalibrationGoal,
		"calibration_marker": syntheticAgentCalibrationMarker,
		"classification":     "synthetic",
		"execution_policy":   SyntheticAgentCalibrationExecutionPolicy,
		"target_present":     false,
		"private_input":      false,
	})
	if err != nil {
		return "", fmt.Errorf("marshal synthetic agent calibration origin: %w", err)
	}
	return string(raw), nil
}

func syntheticAgentCalibrationIntentPayload() (string, error) {
	raw, err := json.Marshal(map[string]any{
		"summary":            syntheticAgentCalibrationIntent,
		"calibration_marker": syntheticAgentCalibrationMarker,
		"classification":     "synthetic",
		"execution_policy":   SyntheticAgentCalibrationExecutionPolicy,
		"target_present":     false,
		"private_input":      false,
	})
	if err != nil {
		return "", fmt.Errorf("marshal synthetic agent calibration intent: %w", err)
	}
	return string(raw), nil
}

func syntheticAgentCalibrationExisting(tx *sql.Tx, llmProfileID int64) (*syntheticAgentCalibrationStored, bool, error) {
	// An archived policy task remains a historical capability record. Do not
	// create a second lane while it exists; restore or explicitly delete the old
	// record instead of allowing an archive/restore uniqueness collision.
	rows, err := tx.Query(`
SELECT task.id, task.exploration_id,
       (
         task.name=$2
         AND task.description=$3
         AND task.goal=$4
         AND task.category_id IS NULL
         AND task.company_id IS NULL
         AND task.parent_ref IS NULL
	         AND task.paused=false
	         AND task.queued=false
	         AND task.queued_at IS NULL
	         AND COALESCE(task.queue_mode,'')=''
	         AND task.paused_at IS NULL
	         AND task.archived_at IS NULL
         AND task.timeout_seconds=$5
         AND task.plan_heartbeat_seconds=$6
         AND task.plan_quiet_mode=true
         AND task.coverage_enabled=false
         AND task.llm_rotate=false
	         AND task.llm_chain_revision=0
         AND task.target_gate=true
         AND COALESCE(task.system_kind,'')=''
         AND COALESCE(task.execution_policy,'')=$1
         AND task.llm_profile_id=$7
         AND task.active_llm_profile_id=$7
         AND NOT EXISTS (SELECT 1 FROM task_llm_profiles profile WHERE profile.task_id=task.id AND profile.profile_id<>$7)
	         AND NOT EXISTS (SELECT 1 FROM task_llm_profiles profile WHERE profile.task_id=task.id AND profile.position<>0)
         AND (SELECT count(*) FROM task_llm_profiles profile WHERE profile.task_id=task.id)=1
         AND NOT EXISTS (SELECT 1 FROM task_relations relation WHERE relation.task_id=task.id OR relation.source_task_id=task.id)
         AND NOT EXISTS (SELECT 1 FROM task_scope scope WHERE scope.task_id=task.id)
         AND NOT EXISTS (SELECT 1 FROM task_credentials credential WHERE credential.task_id=task.id)
         AND NOT EXISTS (SELECT 1 FROM task_intercept_rules rule WHERE rule.task_id=task.id)
         AND NOT EXISTS (SELECT 1 FROM target_pins pin WHERE pin.task_id=task.id)
       )
FROM tasks task
WHERE task.execution_policy=$1
FOR UPDATE OF task`,
		SyntheticAgentCalibrationExecutionPolicy,
		SyntheticAgentCalibrationTaskName,
		syntheticAgentCalibrationDescription,
		syntheticAgentCalibrationGoal,
		SyntheticAgentCalibrationTaskTimeoutSeconds,
		MinPlanHeartbeatSeconds,
		llmProfileID,
	)
	if err != nil {
		return nil, false, fmt.Errorf("lookup synthetic agent calibration task: %w", err)
	}
	defer rows.Close()

	type candidate struct {
		taskID, explorationID int64
		canonical             bool
	}
	var candidates []candidate
	for rows.Next() {
		var candidate candidate
		if err := rows.Scan(&candidate.taskID, &candidate.explorationID, &candidate.canonical); err != nil {
			return nil, false, err
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(candidates) == 0 {
		return nil, false, nil
	}
	if len(candidates) != 1 || !candidates[0].canonical {
		return nil, true, fmt.Errorf("%w: fixed task fields or policy bindings differ", ErrSyntheticAgentCalibrationConflict)
	}
	c := candidates[0]
	intentID, err := verifySyntheticAgentCalibrationGraph(tx, c.explorationID)
	if err != nil {
		return nil, true, err
	}
	return &syntheticAgentCalibrationStored{taskID: c.taskID, intentID: intentID, alreadyPresent: true}, true, nil
}

func verifySyntheticAgentCalibrationGraph(tx *sql.Tx, explorationID int64) (int64, error) {
	intent, err := inspectSyntheticAgentCalibrationGraph(tx, explorationID)
	if err != nil {
		return 0, err
	}
	return intent.id, nil
}

// inspectSyntheticAgentCalibrationGraph locks and validates the complete fixed
// graph. The exploration parent is locked first, then the two exact nodes. This
// prevents a generic graph writer from adding or changing a frontier between a
// successful policy check and the dedicated claim transition.
func inspectSyntheticAgentCalibrationGraph(tx *sql.Tx, explorationID int64) (syntheticAgentCalibrationIntentState, error) {
	return inspectSyntheticAgentCalibrationGraphWithOpenRollback(tx, explorationID, false)
}

// inspectSyntheticAgentCalibrationGraphWithOpenRollback is the same closed-graph
// verifier used by the policy lifecycle, with one deliberately narrow recovery
// exception.  A generic worker cancellation historically settles a claimed
// intent to open while retaining the policy owner and heartbeat fields.  That is
// not a valid steady state and must never be accepted by normal start/finish
// paths.  The dedicated abort/restart-recovery transitions may recognise it only
// so they can atomically terminalise the attempt instead of leaving a
// provider-backed task stuck running forever.
func inspectSyntheticAgentCalibrationGraphWithOpenRollback(tx *sql.Tx, explorationID int64, allowOpenRollback bool) (syntheticAgentCalibrationIntentState, error) {
	var lockedID int64
	if err := tx.QueryRow(`
SELECT id
FROM explorations
WHERE id=$1 AND description=$2 AND goal=$3 AND status='open'
FOR UPDATE`, explorationID, syntheticAgentCalibrationDescription, syntheticAgentCalibrationGoal).Scan(&lockedID); err != nil {
		if err == sql.ErrNoRows {
			return syntheticAgentCalibrationIntentState{}, fmt.Errorf("%w: fixed exploration fields differ", ErrSyntheticAgentCalibrationConflict)
		}
		return syntheticAgentCalibrationIntentState{}, fmt.Errorf("lock synthetic agent calibration exploration: %w", err)
	}

	// The product-owned graph is deliberately closed: one origin fact, one
	// receipt intent and one edge between them. A normal ARTEX graph may grow
	// freely, but this lane must never select an operator- or model-authored
	// frontier node merely because it happens to be open first.
	var nodeCount, edgeCount, anchorCount int
	if err := tx.QueryRow(`SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1`, explorationID).Scan(&nodeCount); err != nil {
		return syntheticAgentCalibrationIntentState{}, fmt.Errorf("count synthetic agent calibration nodes: %w", err)
	}
	if err := tx.QueryRow(`SELECT count(*) FROM exploration_edges WHERE exploration_id=$1`, explorationID).Scan(&edgeCount); err != nil {
		return syntheticAgentCalibrationIntentState{}, fmt.Errorf("count synthetic agent calibration edges: %w", err)
	}
	if err := tx.QueryRow(`
SELECT count(*)
FROM exploration_anchors anchor
JOIN exploration_nodes node ON node.id=anchor.node_id
WHERE node.exploration_id=$1`, explorationID).Scan(&anchorCount); err != nil {
		return syntheticAgentCalibrationIntentState{}, fmt.Errorf("count synthetic agent calibration anchors: %w", err)
	}
	if nodeCount != 2 || edgeCount != 1 || anchorCount != 0 {
		return syntheticAgentCalibrationIntentState{}, fmt.Errorf("%w: graph topology nodes=%d edges=%d anchors=%d", ErrSyntheticAgentCalibrationConflict, nodeCount, edgeCount, anchorCount)
	}

	originPayload, err := syntheticAgentCalibrationOriginPayload()
	if err != nil {
		return syntheticAgentCalibrationIntentState{}, err
	}
	intentPayload, err := syntheticAgentCalibrationIntentPayload()
	if err != nil {
		return syntheticAgentCalibrationIntentState{}, err
	}

	var originID int64
	if err := tx.QueryRow(`
SELECT node.id
FROM exploration_nodes node
WHERE node.exploration_id=$1
  AND node.kind=$2 AND node.state=$3 AND node.origin=$4
  AND node.priority=0 AND node.owner IS NULL AND node.blocked_reason IS NULL
  AND node.delete_reason IS NULL AND node.completed_at IS NULL
  AND node.unclaim_token IS NULL AND node.last_heartbeat_at IS NULL
  AND node.payload=$5::jsonb
FOR UPDATE`,
		explorationID, KindFact, StateOrigin, syntheticAgentCalibrationSource, originPayload,
	).Scan(&originID); err != nil {
		if err == sql.ErrNoRows {
			return syntheticAgentCalibrationIntentState{}, fmt.Errorf("%w: fixed synthetic origin is missing or mutated", ErrSyntheticAgentCalibrationConflict)
		}
		return syntheticAgentCalibrationIntentState{}, fmt.Errorf("verify synthetic agent calibration origin: %w", err)
	}

	var intent syntheticAgentCalibrationIntentState
	if err := tx.QueryRow(`
SELECT node.id, node.state, node.owner, node.unclaim_token,
       node.blocked_reason, node.completed_at, node.last_heartbeat_at
FROM exploration_nodes node
JOIN exploration_edges edge
  ON edge.exploration_id=node.exploration_id AND edge.src_id=$2 AND edge.rel=$3 AND edge.dst_id=node.id
WHERE node.exploration_id=$1
  AND node.kind=$4 AND node.origin=$5 AND node.priority=0
  AND node.delete_reason IS NULL AND node.payload=$6::jsonb
FOR UPDATE OF node`,
		explorationID, originID, RelDerivedFrom, KindIntent, syntheticAgentCalibrationSource, intentPayload,
	).Scan(&intent.id, &intent.state, &intent.owner, &intent.token, &intent.blockedReason, &intent.completedAt, &intent.heartbeatAt); err != nil {
		if err == sql.ErrNoRows {
			return syntheticAgentCalibrationIntentState{}, fmt.Errorf("%w: fixed synthetic receipt intent is missing or mutated", ErrSyntheticAgentCalibrationConflict)
		}
		return syntheticAgentCalibrationIntentState{}, fmt.Errorf("verify synthetic agent calibration intent: %w", err)
	}

	// ClaimIntentToken retains the owner as an audit label after settlement and
	// leaves the final heartbeat in place. Accept exactly those combinations;
	// anything else is a policy conflict, not a reason to fall back to a generic
	// graph or lifecycle path.
	switch intent.state {
	case "open":
		if intent.owner.Valid || intent.token.Valid || intent.blockedReason.Valid || intent.completedAt.Valid || intent.heartbeatAt.Valid {
			if allowOpenRollback && syntheticAgentCalibrationOpenRollback(intent) {
				break
			}
			return syntheticAgentCalibrationIntentState{}, fmt.Errorf("%w: open receipt intent has stale lifecycle fields", ErrSyntheticAgentCalibrationConflict)
		}
	case "running":
		if intent.owner.String != syntheticAgentCalibrationSource || !intent.token.Valid || intent.token.String == "" ||
			intent.blockedReason.Valid || intent.completedAt.Valid || !intent.heartbeatAt.Valid {
			return syntheticAgentCalibrationIntentState{}, fmt.Errorf("%w: running receipt intent does not have the calibration lease", ErrSyntheticAgentCalibrationConflict)
		}
	case "done", "blocked", "exhausted", "stopped":
		if intent.owner.String != syntheticAgentCalibrationSource || intent.token.Valid || intent.blockedReason.Valid || !intent.completedAt.Valid {
			return syntheticAgentCalibrationIntentState{}, fmt.Errorf("%w: settled receipt intent has invalid lifecycle fields", ErrSyntheticAgentCalibrationConflict)
		}
	default:
		return syntheticAgentCalibrationIntentState{}, fmt.Errorf("%w: receipt intent has unexpected state %q", ErrSyntheticAgentCalibrationConflict, intent.state)
	}
	return intent, nil
}

// syntheticAgentCalibrationOpenRollback identifies only the residue left by
// ExplorationStore.SettleIntentClaim(..., "open") for this fixed policy lease.
// The store intentionally retains owner/heartbeat for ordinary provenance, but
// this one-shot lane cannot resume that state: it must become a failed attempt.
func syntheticAgentCalibrationOpenRollback(intent syntheticAgentCalibrationIntentState) bool {
	return intent.state == "open" &&
		intent.owner.Valid && intent.owner.String == syntheticAgentCalibrationSource &&
		!intent.token.Valid && !intent.blockedReason.Valid && !intent.completedAt.Valid && intent.heartbeatAt.Valid
}

// settleSyntheticAgentCalibrationOpenRollback converts the narrowly recognised
// cancellation residue into a normal terminal blocked intent.  It is used only
// while the task/exploration/node are locked by the policy transaction.
func settleSyntheticAgentCalibrationOpenRollback(tx *sql.Tx, task syntheticAgentCalibrationLockedTask, intent syntheticAgentCalibrationIntentState) error {
	if !syntheticAgentCalibrationOpenRollback(intent) {
		return fmt.Errorf("%w: fixed receipt intent is not an open cancellation rollback", ErrSyntheticAgentCalibrationConflict)
	}
	res, err := tx.Exec(`
UPDATE exploration_nodes
SET state='blocked', blocked_reason=NULL, completed_at=now(),
    unclaim_token=NULL, content_version=content_version+1
WHERE id=$1 AND exploration_id=$2 AND kind='intent' AND state='open'
  AND owner=$3 AND unclaim_token IS NULL`, intent.id, task.explorationID, syntheticAgentCalibrationSource)
	if err != nil {
		return fmt.Errorf("settle open synthetic calibration rollback: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("%w: open cancellation rollback changed before terminal settlement", ErrSyntheticAgentCalibrationConflict)
	}
	return nil
}

type syntheticAgentCalibrationLockedTask struct {
	id            int64
	explorationID int64
	profileID     int64
	status        string
	paused        bool
	queued        bool
	queuedAt      sql.NullTime
	queueMode     string
	pausedAt      sql.NullTime
	archivedAt    sql.NullTime
	completedAt   sql.NullTime
	firstRunAt    sql.NullTime
	deadlineAt    sql.NullTime
	endReason     string
}

// lockSyntheticAgentCalibrationTask locks the policy record before its
// exploration and claim rows. expectedProfileID=0 means that lifecycle code is
// only reading the already-bound profile; start/retry always pass the fresh
// server-reviewed profile ID and therefore cannot silently rebind the task.
func lockSyntheticAgentCalibrationTask(tx *sql.Tx, taskID, expectedProfileID int64) (syntheticAgentCalibrationLockedTask, error) {
	if taskID <= 0 {
		return syntheticAgentCalibrationLockedTask{}, fmt.Errorf("%w: invalid task id", ErrSyntheticAgentCalibrationConflict)
	}
	var task syntheticAgentCalibrationLockedTask
	err := tx.QueryRow(`
SELECT task.id, task.exploration_id, task.llm_profile_id,
       task.status, task.paused, task.queued, task.queued_at,
       COALESCE(task.queue_mode,''), task.paused_at, task.archived_at,
       task.completed_at, task.first_run_at, task.deadline_at,
       COALESCE(task.end_reason,'')
FROM tasks task
WHERE task.id=$1
  AND task.deleted_at IS NULL
  AND COALESCE(task.execution_policy,'')=$2
  AND ($3::bigint=0 OR task.llm_profile_id=$3)
  AND task.name=$4
  AND task.description=$5
  AND task.goal=$6
  AND task.category_id IS NULL
  AND task.company_id IS NULL
  AND task.parent_ref IS NULL
  AND task.archived_at IS NULL
  AND task.timeout_seconds=$7
  AND task.plan_heartbeat_seconds=$8
  AND task.plan_quiet_mode=true
  AND task.coverage_enabled=false
  AND task.llm_rotate=false
  AND task.llm_chain_revision=0
  AND task.target_gate=true
  AND COALESCE(task.system_kind,'')=''
  AND task.active_llm_profile_id=task.llm_profile_id
  AND task.llm_profile_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM task_llm_profiles profile WHERE profile.task_id=task.id AND profile.profile_id<>task.llm_profile_id)
  AND NOT EXISTS (SELECT 1 FROM task_llm_profiles profile WHERE profile.task_id=task.id AND profile.position<>0)
  AND (SELECT count(*) FROM task_llm_profiles profile WHERE profile.task_id=task.id)=1
  AND NOT EXISTS (SELECT 1 FROM task_relations relation WHERE relation.task_id=task.id OR relation.source_task_id=task.id)
  AND NOT EXISTS (SELECT 1 FROM task_scope scope WHERE scope.task_id=task.id)
  AND NOT EXISTS (SELECT 1 FROM task_credentials credential WHERE credential.task_id=task.id)
  AND NOT EXISTS (SELECT 1 FROM task_intercept_rules rule WHERE rule.task_id=task.id)
  AND NOT EXISTS (SELECT 1 FROM target_pins pin WHERE pin.task_id=task.id)
FOR UPDATE OF task`,
		taskID,
		SyntheticAgentCalibrationExecutionPolicy,
		expectedProfileID,
		SyntheticAgentCalibrationTaskName,
		syntheticAgentCalibrationDescription,
		syntheticAgentCalibrationGoal,
		SyntheticAgentCalibrationTaskTimeoutSeconds,
		MinPlanHeartbeatSeconds,
	).Scan(
		&task.id, &task.explorationID, &task.profileID,
		&task.status, &task.paused, &task.queued, &task.queuedAt,
		&task.queueMode, &task.pausedAt, &task.archivedAt,
		&task.completedAt, &task.firstRunAt, &task.deadlineAt, &task.endReason,
	)
	if err == sql.ErrNoRows {
		return syntheticAgentCalibrationLockedTask{}, fmt.Errorf("%w: fixed task fields, profile binding, or archive state differ", ErrSyntheticAgentCalibrationConflict)
	}
	if err != nil {
		return syntheticAgentCalibrationLockedTask{}, fmt.Errorf("lock synthetic agent calibration task: %w", err)
	}
	profileID := task.profileID
	if err := lockLLMProfileForReference(tx, &profileID); err != nil {
		return syntheticAgentCalibrationLockedTask{}, fmt.Errorf("lock synthetic agent calibration profile: %w", err)
	}
	return task, nil
}

func validateSyntheticAgentCalibrationLifecycle(task syntheticAgentCalibrationLockedTask, intent syntheticAgentCalibrationIntentState) error {
	if task.paused || task.queued || task.queuedAt.Valid || task.queueMode != "" || task.pausedAt.Valid || task.archivedAt.Valid {
		return fmt.Errorf("%w: policy task has queue, pause, or archive state", ErrSyntheticAgentCalibrationConflict)
	}
	switch task.status {
	case "created":
		if task.completedAt.Valid || task.firstRunAt.Valid || task.deadlineAt.Valid || task.endReason != "" || intent.state != "open" {
			return fmt.Errorf("%w: created task and fixed intent lifecycle differ", ErrSyntheticAgentCalibrationConflict)
		}
	case "running":
		if task.completedAt.Valid || task.endReason != "" || intent.state != "running" {
			return fmt.Errorf("%w: running task and fixed intent lifecycle differ", ErrSyntheticAgentCalibrationConflict)
		}
	case "done":
		if !task.completedAt.Valid || task.endReason != "" || intent.state != "done" {
			return fmt.Errorf("%w: completed task and fixed intent lifecycle differ", ErrSyntheticAgentCalibrationConflict)
		}
	case "failed", "timeout":
		if !task.completedAt.Valid || task.endReason != "" {
			return fmt.Errorf("%w: failed task lifecycle fields differ", ErrSyntheticAgentCalibrationConflict)
		}
		switch intent.state {
		case "open", "running", "done", "blocked", "exhausted", "stopped":
		default:
			return fmt.Errorf("%w: failed task has unexpected fixed intent state %q", ErrSyntheticAgentCalibrationConflict, intent.state)
		}
	default:
		return fmt.Errorf("%w: task has unexpected lifecycle state %q", ErrSyntheticAgentCalibrationConflict, task.status)
	}
	return nil
}

func syntheticAgentCalibrationProfileReceipt(tx *sql.Tx, profileID int64) (map[string]any, error) {
	var format, model, baseURL string
	if err := tx.QueryRow(`
SELECT format, model, COALESCE(base_url,'')
FROM llm_profiles
WHERE id=$1
FOR KEY SHARE`, profileID).Scan(&format, &model, &baseURL); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrLLMProfileNotFound
		}
		return nil, err
	}
	return map[string]any{
		"profile_id":       profileID,
		"profile_format":   format,
		"profile_model":    model,
		"profile_base_url": baseURL,
	}, nil
}

func syntheticAgentCalibrationLastAttempt(tx *sql.Tx, explorationID int64) (int, error) {
	var last int
	if err := tx.QueryRow(`
SELECT COALESCE(MAX(
  CASE WHEN COALESCE(metadata->>'attempt','') ~ '^[0-9]+$'
       THEN (metadata->>'attempt')::integer
       ELSE 0
  END
), 0)
FROM activity
WHERE exploration_id=$1
  AND worker='synthetic-calibration'
  AND kind IN ('attempt_started','retry_reset','attempt_finished','restart_interrupted')`, explorationID).Scan(&last); err != nil {
		return 0, err
	}
	return last, nil
}

func appendSyntheticAgentCalibrationActivity(tx *sql.Tx, explorationID, intentID int64, kind, summary string, metadata map[string]any) error {
	raw, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`
INSERT INTO activity(exploration_id, node_id, worker, kind, is_error, summary, metadata)
VALUES ($1,$2,'synthetic-calibration',$3,false,$4,$5)`, explorationID, intentID, kind, summary, jsonbClean(raw))
	return err
}

func syntheticAgentCalibrationRunningLeaseIsStale(intent syntheticAgentCalibrationIntentState, now time.Time) bool {
	return intent.state == "running" && intent.heartbeatAt.Valid && !intent.heartbeatAt.Time.After(now.Add(-IntentClaimTTL))
}

// PrepareSyntheticAgentCalibrationRun is the only transition that grants a
// provider-backed lease. It atomically validates the exact task/graph, handles
// an explicit failed-attempt retry, changes task state to running, claims the
// one fixed intent, and records the immutable start receipt.
func (d *DB) PrepareSyntheticAgentCalibrationRun(taskID, expectedProfileID int64, allowRetry bool) (*SyntheticAgentCalibrationRun, error) {
	if expectedProfileID <= 0 {
		return nil, fmt.Errorf("%w: LLM profile id is required", ErrSyntheticAgentCalibrationConflict)
	}
	var lastErr error
	for attempt := 0; attempt < syntheticCalibrationSerializationMaxAttempts; attempt++ {
		run, err := d.prepareSyntheticAgentCalibrationRunOnce(taskID, expectedProfileID, allowRetry)
		if err == nil {
			task, getErr := d.GetTask(taskID)
			if getErr != nil {
				return nil, fmt.Errorf("read prepared synthetic agent calibration task: %w", getErr)
			}
			if task == nil || !task.IsSyntheticAgentCalibration() {
				return nil, ErrSyntheticAgentCalibrationImmutable
			}
			run.Task = task
			return run, nil
		}
		lastErr = err
		if !isSyntheticCalibrationSerializationFailure(err) {
			return nil, err
		}
	}
	return nil, lastErr
}

func (d *DB) prepareSyntheticAgentCalibrationRunOnce(taskID, expectedProfileID int64, allowRetry bool) (*SyntheticAgentCalibrationRun, error) {
	tx, err := d.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // successful commit makes this a no-op
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext($1)::bigint)`, "artex-calibration:synthetic-agent:v1"); err != nil {
		return nil, fmt.Errorf("lock synthetic agent calibration: %w", err)
	}
	task, err := lockSyntheticAgentCalibrationTask(tx, taskID, expectedProfileID)
	if err != nil {
		return nil, err
	}
	intent, err := inspectSyntheticAgentCalibrationGraph(tx, task.explorationID)
	if err != nil {
		return nil, err
	}
	if err := validateSyntheticAgentCalibrationLifecycle(task, intent); err != nil {
		return nil, err
	}
	lastAttempt, err := syntheticAgentCalibrationLastAttempt(tx, task.explorationID)
	if err != nil {
		return nil, fmt.Errorf("read synthetic agent calibration attempt: %w", err)
	}
	currentAttempt := lastAttempt
	if currentAttempt == 0 {
		currentAttempt = 1
	}

	switch task.status {
	case "running", "done":
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return &SyntheticAgentCalibrationRun{IntentID: intent.id, Attempt: currentAttempt}, nil
	case "failed", "timeout":
		if !allowRetry {
			if err := tx.Commit(); err != nil {
				return nil, err
			}
			return &SyntheticAgentCalibrationRun{IntentID: intent.id, Attempt: currentAttempt}, nil
		}
		if intent.state == "running" && !syntheticAgentCalibrationRunningLeaseIsStale(intent, time.Now().UTC()) {
			return nil, fmt.Errorf("%w: prior fixed intent still has a live lease", ErrSyntheticAgentCalibrationConflict)
		}
		if intent.state == "running" && !intent.heartbeatAt.Valid {
			return nil, fmt.Errorf("%w: prior running fixed intent has no heartbeat", ErrSyntheticAgentCalibrationConflict)
		}
		profileReceipt, err := syntheticAgentCalibrationProfileReceipt(tx, task.profileID)
		if err != nil {
			return nil, fmt.Errorf("read synthetic agent calibration profile receipt: %w", err)
		}
		currentAttempt = lastAttempt + 1
		if currentAttempt < 2 {
			currentAttempt = 2
		}
		metadata := map[string]any{
			"event":              "retry_reset",
			"attempt":            currentAttempt,
			"execution_policy":   SyntheticAgentCalibrationExecutionPolicy,
			"intent_id":          intent.id,
			"prior_task_status":  task.status,
			"prior_intent_state": intent.state,
		}
		for key, value := range profileReceipt {
			metadata[key] = value
		}
		if _, err := tx.Exec(`
UPDATE exploration_nodes
SET state='open', owner=NULL, blocked_reason=NULL, completed_at=NULL,
    unclaim_token=NULL, last_heartbeat_at=NULL, delete_reason=NULL,
    content_version=content_version+1
WHERE id=$1 AND exploration_id=$2 AND kind='intent'`, intent.id, task.explorationID); err != nil {
			return nil, fmt.Errorf("reset fixed synthetic receipt intent: %w", err)
		}
		if err := appendSyntheticAgentCalibrationActivity(tx, task.explorationID, intent.id, "retry_reset", "Synthetic calibration retry reset the one fixed receipt intent; prior activity and LLM records were retained.", metadata); err != nil {
			return nil, fmt.Errorf("record synthetic agent calibration retry: %w", err)
		}
		intent = syntheticAgentCalibrationIntentState{id: intent.id, state: "open"}
	case "created":
		// The initial, never-run lane is already in the exact state required
		// below. Only this case can start without an explicit retry action.
	default:
		return nil, fmt.Errorf("%w: task state %q cannot start calibration", ErrSyntheticAgentCalibrationConflict, task.status)
	}

	profileReceipt, err := syntheticAgentCalibrationProfileReceipt(tx, task.profileID)
	if err != nil {
		return nil, fmt.Errorf("read synthetic agent calibration profile receipt: %w", err)
	}
	token, err := newUnclaimToken()
	if err != nil {
		return nil, fmt.Errorf("create synthetic agent calibration claim token: %w", err)
	}
	res, err := tx.Exec(`
UPDATE tasks
SET status='running', paused=false, paused_at=NULL,
    queued=false, queued_at=NULL, queue_mode='',
    completed_at=NULL, first_run_at=NULL, deadline_at=NULL, end_reason=''
WHERE id=$1 AND deleted_at IS NULL AND execution_policy=$2
  AND status IN ('created','failed','timeout')`, task.id, SyntheticAgentCalibrationExecutionPolicy)
	if err != nil {
		return nil, fmt.Errorf("start synthetic agent calibration task: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, fmt.Errorf("%w: task start compare-and-set lost", ErrSyntheticAgentCalibrationConflict)
	}
	res, err = tx.Exec(`
UPDATE exploration_nodes
SET state='running', owner=$1, blocked_reason=NULL, completed_at=NULL,
    unclaim_token=$2, last_heartbeat_at=now(), content_version=content_version+1
WHERE id=$3 AND exploration_id=$4 AND kind='intent' AND state='open'
  AND owner IS NULL AND unclaim_token IS NULL`, syntheticAgentCalibrationSource, token, intent.id, task.explorationID)
	if err != nil {
		return nil, fmt.Errorf("claim fixed synthetic receipt intent: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, fmt.Errorf("%w: fixed receipt intent claim compare-and-set lost", ErrSyntheticAgentCalibrationConflict)
	}
	metadata := map[string]any{
		"event":            "attempt_started",
		"attempt":          currentAttempt,
		"execution_policy": SyntheticAgentCalibrationExecutionPolicy,
		"intent_id":        intent.id,
	}
	for key, value := range profileReceipt {
		metadata[key] = value
	}
	if err := appendSyntheticAgentCalibrationActivity(tx, task.explorationID, intent.id, "attempt_started", "Bounded synthetic agent calibration started: one fixed Worker receipt intent and no target or generic tool lane.", metadata); err != nil {
		return nil, fmt.Errorf("record synthetic agent calibration start: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &SyntheticAgentCalibrationRun{
		IntentID:   intent.id,
		ClaimToken: token,
		Attempt:    currentAttempt,
		Retried:    task.status == "failed" || task.status == "timeout",
		Started:    true,
	}, nil
}

// FinishSyntheticAgentCalibration records one terminal state and its structured
// local receipt in one transaction. A stale/lost runner cannot add a misleading
// completion row because terminal writes are accepted only from task=running.
func (d *DB) FinishSyntheticAgentCalibration(taskID int64, status string, receiptCalls, modelCalls uint32) (bool, error) {
	if status != "done" && status != "failed" {
		return false, fmt.Errorf("synthetic agent calibration terminal status %q is invalid", status)
	}
	tx, err := d.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck // successful commit makes this a no-op
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext($1)::bigint)`, "artex-calibration:synthetic-agent:v1"); err != nil {
		return false, fmt.Errorf("lock synthetic agent calibration: %w", err)
	}
	task, err := lockSyntheticAgentCalibrationTask(tx, taskID, 0)
	if err != nil {
		return false, err
	}
	intent, err := inspectSyntheticAgentCalibrationGraph(tx, task.explorationID)
	if err != nil {
		return false, err
	}
	if task.status != "running" {
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return false, nil
	}
	if task.paused || task.queued || task.queuedAt.Valid || task.queueMode != "" || task.pausedAt.Valid || task.archivedAt.Valid ||
		task.completedAt.Valid || task.endReason != "" {
		return false, fmt.Errorf("%w: running policy task has invalid lifecycle fields", ErrSyntheticAgentCalibrationConflict)
	}
	if status == "done" {
		if intent.state != "done" || receiptCalls != 1 || modelCalls != 2 {
			return false, fmt.Errorf("%w: successful calibration did not complete the exact two-call fixed receipt", ErrSyntheticAgentCalibrationConflict)
		}
	}
	if status == "failed" {
		switch intent.state {
		case "done", "blocked", "exhausted", "stopped":
		default:
			return false, fmt.Errorf("%w: failed calibration left fixed intent %q", ErrSyntheticAgentCalibrationConflict, intent.state)
		}
	}
	attempt, err := syntheticAgentCalibrationLastAttempt(tx, task.explorationID)
	if err != nil {
		return false, err
	}
	if attempt == 0 {
		return false, fmt.Errorf("%w: no recorded attempt start", ErrSyntheticAgentCalibrationConflict)
	}
	res, err := tx.Exec(`
UPDATE tasks
SET status=$1, completed_at=now()
WHERE id=$2 AND deleted_at IS NULL AND execution_policy=$3 AND status='running'`, status, task.id, SyntheticAgentCalibrationExecutionPolicy)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false, fmt.Errorf("%w: terminal compare-and-set lost", ErrSyntheticAgentCalibrationConflict)
	}
	metadata := map[string]any{
		"event":            "attempt_finished",
		"attempt":          attempt,
		"execution_policy": SyntheticAgentCalibrationExecutionPolicy,
		"intent_id":        intent.id,
		"task_status":      status,
		"intent_state":     intent.state,
		"receipt_calls":    receiptCalls,
		"model_calls":      modelCalls,
	}
	summary := "Synthetic calibration attempt ended without satisfying the fixed receipt condition. No generic retry, fallback, target, or tool path was used."
	if status == "done" {
		summary = "Synthetic calibration receipt completed with the fixed local capability. No target, filesystem, process, network, credential, proxy, browser, scanner, skill, MCP, traffic, or graph tool was available."
	}
	if err := appendSyntheticAgentCalibrationActivity(tx, task.explorationID, intent.id, "attempt_finished", summary, metadata); err != nil {
		return false, fmt.Errorf("record synthetic agent calibration completion: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// AbortPreparedSyntheticAgentCalibration fails one already-admitted synthetic
// attempt without opening a generic execution or retry path.  The live-claim
// case is fenced by the exact token returned by Prepare; a terminal intent was
// already fenced by its own token-bearing settlement.  The only tokenless open
// case accepted here is the explicitly recognised generic-cancellation residue,
// which is terminalised in this same transaction.
//
// won=false means the task is no longer this runner's live attempt (for example
// another terminal writer already won, or its claim token was lost).  Callers
// must treat that as stale and must not issue another provider request.
func (d *DB) AbortPreparedSyntheticAgentCalibration(taskID, intentID int64, claimToken string) (bool, error) {
	if taskID <= 0 || intentID <= 0 || claimToken == "" {
		return false, fmt.Errorf("%w: task, intent, and claim token are required to abort a prepared calibration", ErrSyntheticAgentCalibrationConflict)
	}
	tx, err := d.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck // successful commit makes this a no-op
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext($1)::bigint)`, "artex-calibration:synthetic-agent:v1"); err != nil {
		return false, fmt.Errorf("lock synthetic agent calibration: %w", err)
	}
	task, err := lockSyntheticAgentCalibrationTask(tx, taskID, 0)
	if err != nil {
		return false, err
	}
	intent, err := inspectSyntheticAgentCalibrationGraphWithOpenRollback(tx, task.explorationID, true)
	if err != nil {
		return false, err
	}
	if intent.id != intentID {
		return false, fmt.Errorf("%w: prepared intent identity differs", ErrSyntheticAgentCalibrationConflict)
	}
	if task.status != "running" {
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return false, nil
	}
	if task.paused || task.queued || task.queuedAt.Valid || task.queueMode != "" || task.pausedAt.Valid || task.archivedAt.Valid ||
		task.completedAt.Valid || task.endReason != "" {
		return false, fmt.Errorf("%w: running policy task has invalid lifecycle fields", ErrSyntheticAgentCalibrationConflict)
	}

	priorIntentState := intent.state
	switch intent.state {
	case "running":
		// Do not let a stale local runner terminalise a newer lease.  The
		// conditional update repeats the exact-token fence even though the row is
		// locked, because this helper is the final lifecycle authority.
		if !intent.token.Valid || intent.token.String != claimToken {
			if err := tx.Commit(); err != nil {
				return false, err
			}
			return false, nil
		}
		res, err := tx.Exec(`
UPDATE exploration_nodes
SET state='blocked', blocked_reason=NULL, completed_at=now(),
    unclaim_token=NULL, content_version=content_version+1
WHERE id=$1 AND exploration_id=$2 AND kind='intent' AND state='running'
  AND unclaim_token=$3`, intent.id, task.explorationID, claimToken)
		if err != nil {
			return false, fmt.Errorf("abort prepared synthetic calibration claim: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return false, fmt.Errorf("%w: prepared claim changed before abort", ErrSyntheticAgentCalibrationConflict)
		}
	case "open":
		// A cancellation can pass through generic runIntent before the dedicated
		// engine regains control.  It clears the token but leaves the precisely
		// recognised owner/heartbeat residue.  Convert it to blocked atomically;
		// normal open intents never take this branch.
		if err := settleSyntheticAgentCalibrationOpenRollback(tx, task, intent); err != nil {
			return false, err
		}
	case "done", "blocked", "exhausted", "stopped":
		// The terminal settlement had to pass ExplorationStore's exact-token
		// fence.  A retry cannot have started while this task remains running, so
		// this is still the prepared attempt and may receive its failure receipt.
	default:
		return false, fmt.Errorf("%w: prepared calibration has unexpected intent state %q", ErrSyntheticAgentCalibrationConflict, intent.state)
	}

	attempt, err := syntheticAgentCalibrationLastAttempt(tx, task.explorationID)
	if err != nil {
		return false, err
	}
	if attempt == 0 {
		return false, fmt.Errorf("%w: no recorded attempt start", ErrSyntheticAgentCalibrationConflict)
	}
	res, err := tx.Exec(`
UPDATE tasks
SET status='failed', completed_at=now()
WHERE id=$1 AND deleted_at IS NULL AND execution_policy=$2 AND status='running'`, task.id, SyntheticAgentCalibrationExecutionPolicy)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false, fmt.Errorf("%w: prepared abort terminal compare-and-set lost", ErrSyntheticAgentCalibrationConflict)
	}
	metadata := map[string]any{
		"event":              "attempt_aborted",
		"attempt":            attempt,
		"execution_policy":   SyntheticAgentCalibrationExecutionPolicy,
		"intent_id":          intent.id,
		"prior_intent_state": priorIntentState,
		"task_status":        "failed",
	}
	if err := appendSyntheticAgentCalibrationActivity(tx, task.explorationID, intent.id, "attempt_aborted", "Bounded synthetic calibration execution did not retain its prepared local lease. The attempt was failed without a generic retry, fallback, target, or tool path.", metadata); err != nil {
		return false, fmt.Errorf("record synthetic agent calibration abort: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// MarkSyntheticAgentCalibrationInterrupted is used only by restart recovery.
// It never resumes or clears a running lease; retry later requires the normal
// stale-lease check in PrepareSyntheticAgentCalibrationRun.
func (d *DB) MarkSyntheticAgentCalibrationInterrupted(taskID int64) (bool, error) {
	tx, err := d.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck // successful commit makes this a no-op
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext($1)::bigint)`, "artex-calibration:synthetic-agent:v1"); err != nil {
		return false, fmt.Errorf("lock synthetic agent calibration: %w", err)
	}
	task, err := lockSyntheticAgentCalibrationTask(tx, taskID, 0)
	if err != nil {
		return false, err
	}
	intent, err := inspectSyntheticAgentCalibrationGraphWithOpenRollback(tx, task.explorationID, true)
	if err != nil {
		return false, err
	}
	if task.status != "created" && task.status != "running" {
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return false, nil
	}
	priorIntentState := intent.state
	if task.status == "created" {
		if err := validateSyntheticAgentCalibrationLifecycle(task, intent); err != nil {
			return false, err
		}
	} else {
		// A process can crash after runIntent fenced/settled the fixed intent
		// but before FinishSyntheticAgentCalibration commits the task receipt.
		// Keep that terminal node untouched, mark the task failed, and let the
		// explicit retry transaction preserve the evidence and reset it later.
		if task.paused || task.queued || task.queuedAt.Valid || task.queueMode != "" || task.pausedAt.Valid || task.archivedAt.Valid ||
			task.completedAt.Valid || task.endReason != "" {
			return false, fmt.Errorf("%w: interrupted running task has invalid lifecycle fields", ErrSyntheticAgentCalibrationConflict)
		}
		switch intent.state {
		case "open":
			// This is only the recognised result of a cancellation racing the
			// dedicated engine.  Normal open state with a running policy task is
			// still rejected by the helper above.  Normalise it before committing
			// the failed task receipt so a later explicit retry has a canonical
			// terminal intent to reset.
			if err := settleSyntheticAgentCalibrationOpenRollback(tx, task, intent); err != nil {
				return false, err
			}
			intent.state = "blocked"
		case "running", "done", "blocked", "exhausted", "stopped":
		default:
			return false, fmt.Errorf("%w: interrupted running task has fixed intent %q", ErrSyntheticAgentCalibrationConflict, intent.state)
		}
	}
	attempt, err := syntheticAgentCalibrationLastAttempt(tx, task.explorationID)
	if err != nil {
		return false, err
	}
	if attempt == 0 {
		attempt = 1
	}
	res, err := tx.Exec(`
UPDATE tasks
SET status='failed', completed_at=now()
WHERE id=$1 AND deleted_at IS NULL AND execution_policy=$2
  AND status IN ('created','running')`, task.id, SyntheticAgentCalibrationExecutionPolicy)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false, fmt.Errorf("%w: restart interruption compare-and-set lost", ErrSyntheticAgentCalibrationConflict)
	}
	metadata := map[string]any{
		"event":              "restart_interrupted",
		"attempt":            attempt,
		"execution_policy":   SyntheticAgentCalibrationExecutionPolicy,
		"intent_id":          intent.id,
		"prior_task_status":  task.status,
		"prior_intent_state": priorIntentState,
	}
	if err := appendSyntheticAgentCalibrationActivity(tx, task.explorationID, intent.id, "restart_interrupted", "ARTEX restarted before this bounded synthetic calibration completed. The provider-backed lease was not resumed.", metadata); err != nil {
		return false, fmt.Errorf("record synthetic agent calibration restart interruption: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// SyntheticAgentCalibrationIntentID remains a narrow read/verification helper
// for code that needs the fixed intent identity after a run. It is not a start
// primitive; only PrepareSyntheticAgentCalibrationRun may claim the intent.
func (d *DB) SyntheticAgentCalibrationIntentID(taskID int64) (int64, error) {
	tx, err := d.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // successful commit makes this a no-op
	task, err := lockSyntheticAgentCalibrationTask(tx, taskID, 0)
	if err != nil {
		return 0, err
	}
	intent, err := inspectSyntheticAgentCalibrationGraph(tx, task.explorationID)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return intent.id, nil
}
