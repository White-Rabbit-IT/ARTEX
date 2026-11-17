package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5/pgconn"
)

// SyntheticCalibrationTaskName is the fixed, terminal CAL-00 task shown in
// the UI.  It is deliberately not an operator-provided task title: CAL-00 is
// an auditable product calibration record, not an engagement or a test target.
const SyntheticCalibrationTaskName = "ARTEX CAL-00 — synthetic UI calibration"

const (
	syntheticCalibrationMarker        = "artex-cal-00-synthetic-ui-v1"
	syntheticCalibrationBundleID      = "artex:calibration:synthetic-ui:v1"
	syntheticCalibrationSource        = "synthetic_calibration"
	syntheticCalibrationDescription   = "Fixed synthetic provenance calibration record. It does not assess a target, invoke a model, execute a tool, capture traffic, or create a finding."
	syntheticCalibrationGoal          = "Show a terminal, non-runnable synthetic provenance graph in the ARTEX UI."
	syntheticCalibrationAssetName     = "ARTEX CAL-00 synthetic fixture (not a target)"
	syntheticCalibrationAssetCategory = "synthetic-calibration"
	syntheticCalibrationAssetDetail   = "Fixed local fixture used only to anchor the CAL-00 provenance graph. It is not a target or scope entry."
)

var (
	// ErrSyntheticCalibrationConflict means an existing CAL-00 receipt no
	// longer matches its fixed topology or metadata. It is deliberately
	// fail-closed: ARTEX will not silently replace a record that operators may
	// already have inspected.
	ErrSyntheticCalibrationConflict = errors.New("synthetic calibration record is not canonical")
	// ErrSyntheticCalibrationAssetImmutable prevents a general asset operation
	// from changing or deleting CAL-00's fixture asset.
	ErrSyntheticCalibrationAssetImmutable = errors.New("synthetic calibration fixture asset is immutable")
)

// IsSyntheticCalibrationFixtureBundleID reports whether bundleID is reserved
// for CAL-00's one fixed, local graph anchor. General asset-import paths must
// never use this bundle ID; the calibration constructor is the sole producer.
func IsSyntheticCalibrationFixtureBundleID(bundleID string) bool {
	return bundleID == syntheticCalibrationBundleID
}

// SyntheticCalibrationResult is the immutable database receipt returned by
// EnsureSyntheticCalibrationTask.  The record exists to prove UI/provenance
// wiring only.  It must never be interpreted as a model, tool, target, or
// vulnerability run.
type SyntheticCalibrationResult struct {
	Task           *Task
	AssetID        int64
	OriginFactID   int64
	IntentID       int64
	FactIDs        map[string]int64
	AlreadyPresent bool
}

type syntheticCalibrationFact struct {
	ID      string
	Summary string
	Detail  string
}

// syntheticCalibrationSpec is intentionally internal.  The exported helper
// takes no request data, so callers cannot turn CAL-00 into a task with an
// arbitrary prompt, host, URL, credential, or target.
type syntheticCalibrationSpec struct {
	Marker           string
	BundleID         string
	SystemKind       string
	Name             string
	Description      string
	Goal             string
	AssetName        string
	AssetCategory    string
	AssetDescription string
	Facts            []syntheticCalibrationFact
}

var defaultSyntheticCalibrationSpec = syntheticCalibrationSpec{
	Marker:           syntheticCalibrationMarker,
	BundleID:         syntheticCalibrationBundleID,
	SystemKind:       SyntheticCalibrationSystemKind,
	Name:             SyntheticCalibrationTaskName,
	Description:      syntheticCalibrationDescription,
	Goal:             syntheticCalibrationGoal,
	AssetName:        syntheticCalibrationAssetName,
	AssetCategory:    syntheticCalibrationAssetCategory,
	AssetDescription: syntheticCalibrationAssetDetail,
	Facts: []syntheticCalibrationFact{
		{
			ID:      "fixture",
			Summary: "CAL-00 synthetic fixture recorded",
			Detail:  "This is a fixed local calibration fixture. It is not a host, endpoint, account, source tree, target, or third-party instruction.",
		},
		{
			ID:      "limitation",
			Summary: "CAL-00 did not execute an assessment",
			Detail:  "No LLM, model provider, tool, browser, shell, target, scope, traffic capture, credential, approval, or finding was created by this record.",
		},
	},
}

const syntheticCalibrationSerializationMaxAttempts = 3

// EnsureSyntheticCalibrationTask creates CAL-00 exactly once.  It never calls
// CreateTaskWithOptions, launches no engine, and accepts no external input.
// The task is terminal at INSERT time, so neither a manager reload nor a
// restart can ever observe it as runnable.
func (d *DB) EnsureSyntheticCalibrationTask() (*SyntheticCalibrationResult, error) {
	return d.ensureSyntheticCalibrationTask(defaultSyntheticCalibrationSpec)
}

func (d *DB) ensureSyntheticCalibrationTask(spec syntheticCalibrationSpec) (*SyntheticCalibrationResult, error) {
	if err := validateSyntheticCalibrationSpec(spec); err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 0; attempt < syntheticCalibrationSerializationMaxAttempts; attempt++ {
		stored, err := d.ensureSyntheticCalibrationTaskOnce(spec)
		if err == nil {
			task, err := d.GetTask(stored.taskID)
			if err != nil {
				return nil, fmt.Errorf("read synthetic calibration task: %w", err)
			}
			if task == nil {
				return nil, fmt.Errorf("%w: task %d disappeared after commit", ErrSyntheticCalibrationConflict, stored.taskID)
			}
			return &SyntheticCalibrationResult{
				Task:           task,
				AssetID:        stored.assetID,
				OriginFactID:   stored.originFactID,
				IntentID:       stored.intentID,
				FactIDs:        stored.factIDs,
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

type syntheticCalibrationStored struct {
	taskID, assetID, originFactID, intentID int64
	factIDs                                 map[string]int64
	alreadyPresent                          bool
}

func (d *DB) ensureSyntheticCalibrationTaskOnce(spec syntheticCalibrationSpec) (*syntheticCalibrationStored, error) {
	tx, err := d.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // a successful commit makes this a no-op

	// The asset bundle has a uniqueness constraint, and this advisory lock keeps
	// two first callers from taking incompatible snapshots before that constraint
	// can converge them.  The key is fixed/internal, never user supplied.
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext($1)::bigint)`, "artex-calibration:"+spec.BundleID); err != nil {
		return nil, fmt.Errorf("lock synthetic calibration: %w", err)
	}

	if existing, found, err := syntheticCalibrationExisting(tx, spec); err != nil {
		return nil, err
	} else if found {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return existing, nil
	}

	var explorationID int64
	if err := tx.QueryRow(`
INSERT INTO explorations(description, goal, status)
VALUES ($1, $2, 'achieved')
RETURNING id`, spec.Description, spec.Goal).Scan(&explorationID); err != nil {
		return nil, fmt.Errorf("create synthetic calibration exploration: %w", err)
	}

	originPayload, err := syntheticCalibrationOriginPayload(spec)
	if err != nil {
		return nil, err
	}
	var originFactID int64
	if err := tx.QueryRow(`
INSERT INTO exploration_nodes(exploration_id, kind, payload, priority, state, origin)
VALUES ($1, $2, $3, 0, $4, $5)
RETURNING id`, explorationID, KindFact, originPayload, StateOrigin, syntheticCalibrationSource).Scan(&originFactID); err != nil {
		return nil, fmt.Errorf("create synthetic calibration origin: %w", err)
	}

	// This record is terminal from its first visible state.  In particular, it
	// never has status=created/running and no code path here invokes engine.Run.
	var taskID int64
	if err := tx.QueryRow(`
INSERT INTO tasks(
    name, description, goal, exploration_id,
    status, paused, queued,
    timeout_seconds, plan_heartbeat_seconds, plan_quiet_mode,
    coverage_enabled, llm_rotate, target_gate, system_kind, completed_at
)
VALUES ($1, $2, $3, $4, 'done', false, false, 0, $5, true, false, false, true, $6, now())
RETURNING id`,
		spec.Name, spec.Description, spec.Goal, explorationID, MinPlanHeartbeatSeconds, spec.SystemKind,
	).Scan(&taskID); err != nil {
		return nil, fmt.Errorf("create terminal synthetic calibration task: %w", err)
	}

	// Serialize with company-scope changes before the app upsert.  The fixture
	// itself uses no ICP/company data; the lock protects that invariant from an
	// unrelated scope mutation during this transaction.
	if err := lockCompanyScopeMutation(tx); err != nil {
		return nil, fmt.Errorf("lock synthetic calibration asset scope: %w", err)
	}
	assets := &AssetStore{db: d, company: d.Companies(), tx: tx}
	assetID, err := assets.UpsertApp(UpsertAppReq{
		Name:        spec.AssetName,
		BundleID:    spec.BundleID,
		Category:    spec.AssetCategory,
		Description: spec.AssetDescription,
		TaskID:      taskID,
		// This unexported switch is intentionally available only within db. A
		// normal API/import caller cannot create or update the reserved fixture.
		allowSyntheticCalibrationFixture: true,
	})
	if err != nil {
		return nil, fmt.Errorf("create synthetic calibration fixture asset: %w", err)
	}
	if err := syntheticCalibrationAnchor(tx, originFactID, assetID); err != nil {
		return nil, err
	}
	if err := assets.SetTaskAssetSource(taskID, assetID, syntheticCalibrationSource, spec.Marker, &originFactID); err != nil {
		return nil, fmt.Errorf("set synthetic calibration asset provenance: %w", err)
	}

	intentPayload, err := syntheticCalibrationIntentPayload(spec)
	if err != nil {
		return nil, err
	}
	var intentID int64
	if err := tx.QueryRow(`
INSERT INTO exploration_nodes(exploration_id, kind, payload, priority, state, origin, completed_at)
VALUES ($1, $2, $3, 0, 'done', $4, now())
RETURNING id`, explorationID, KindIntent, intentPayload, syntheticCalibrationSource).Scan(&intentID); err != nil {
		return nil, fmt.Errorf("create synthetic calibration terminal intent: %w", err)
	}
	if err := syntheticCalibrationAnchor(tx, intentID, assetID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`
INSERT INTO exploration_edges(exploration_id, src_id, rel, dst_id)
VALUES ($1, $2, $3, $4)`, explorationID, originFactID, RelDerivedFrom, intentID); err != nil {
		return nil, fmt.Errorf("link synthetic calibration origin to intent: %w", err)
	}

	factIDs := make(map[string]int64, len(spec.Facts))
	for _, fact := range spec.Facts {
		payload, err := syntheticCalibrationFactPayload(spec, fact)
		if err != nil {
			return nil, err
		}
		var factID int64
		if err := tx.QueryRow(`
INSERT INTO exploration_nodes(exploration_id, kind, payload, priority, state, origin, completed_at)
VALUES ($1, $2, $3, 0, 'confirmed', $4, now())
RETURNING id`, explorationID, KindFact, payload, syntheticCalibrationSource).Scan(&factID); err != nil {
			return nil, fmt.Errorf("create synthetic calibration fact %q: %w", fact.ID, err)
		}
		if err := syntheticCalibrationAnchor(tx, factID, assetID); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`
INSERT INTO exploration_edges(exploration_id, src_id, rel, dst_id)
VALUES ($1, $2, $3, $4)`, explorationID, intentID, RelYields, factID); err != nil {
			return nil, fmt.Errorf("link synthetic calibration fact %q: %w", fact.ID, err)
		}
		factIDs[fact.ID] = factID
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &syntheticCalibrationStored{
		taskID: taskID, assetID: assetID, originFactID: originFactID, intentID: intentID, factIDs: factIDs,
	}, nil
}

func validateSyntheticCalibrationSpec(spec syntheticCalibrationSpec) error {
	if spec.Marker == "" || spec.BundleID == "" || spec.SystemKind != SyntheticCalibrationSystemKind || spec.Name == "" || spec.Description == "" || spec.Goal == "" || spec.AssetName == "" || spec.AssetCategory == "" || spec.AssetDescription == "" {
		return fmt.Errorf("%w: incomplete fixed specification", ErrSyntheticCalibrationConflict)
	}
	if len(spec.Facts) == 0 {
		return fmt.Errorf("%w: fixed specification has no facts", ErrSyntheticCalibrationConflict)
	}
	seen := make(map[string]struct{}, len(spec.Facts))
	for _, fact := range spec.Facts {
		if fact.ID == "" || fact.Summary == "" || fact.Detail == "" {
			return fmt.Errorf("%w: fixed specification has an incomplete fact", ErrSyntheticCalibrationConflict)
		}
		if _, exists := seen[fact.ID]; exists {
			return fmt.Errorf("%w: fixed specification repeats fact %q", ErrSyntheticCalibrationConflict, fact.ID)
		}
		seen[fact.ID] = struct{}{}
	}
	return nil
}

func isSyntheticCalibrationSerializationFailure(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40001"
}

func syntheticCalibrationOriginPayload(spec syntheticCalibrationSpec) (string, error) {
	raw, err := json.Marshal(map[string]any{
		"summary":               "Synthetic calibration origin",
		"description":           spec.Description,
		"goal":                  spec.Goal,
		"calibration_marker":    spec.Marker,
		"classification":        "synthetic-calibration",
		"runnable":              false,
		"provider_called":       false,
		"target_assessment_run": false,
	})
	if err != nil {
		return "", fmt.Errorf("marshal synthetic calibration origin: %w", err)
	}
	return string(raw), nil
}

func syntheticCalibrationIntentPayload(spec syntheticCalibrationSpec) (string, error) {
	raw, err := json.Marshal(map[string]any{
		"summary":            "Terminal synthetic calibration provenance",
		"calibration_marker": spec.Marker,
		"classification":     "synthetic-calibration",
		"runnable":           false,
		"provider_called":    false,
	})
	if err != nil {
		return "", fmt.Errorf("marshal synthetic calibration intent: %w", err)
	}
	return string(raw), nil
}

func syntheticCalibrationFactPayload(spec syntheticCalibrationSpec, fact syntheticCalibrationFact) (string, error) {
	raw, err := json.Marshal(map[string]any{
		"calibration_id":     fact.ID,
		"summary":            fact.Summary,
		"detail":             fact.Detail,
		"calibration_marker": spec.Marker,
		"classification":     "synthetic-calibration",
		"runnable":           false,
		"provider_called":    false,
	})
	if err != nil {
		return "", fmt.Errorf("marshal synthetic calibration fact %q: %w", fact.ID, err)
	}
	return string(raw), nil
}

func syntheticCalibrationAnchor(tx *sql.Tx, nodeID, assetID int64) error {
	if _, err := tx.Exec(`
INSERT INTO exploration_anchors(node_id, asset_id)
VALUES ($1, $2)
ON CONFLICT (node_id, asset_id) DO NOTHING`, nodeID, assetID); err != nil {
		return fmt.Errorf("anchor synthetic calibration node %d: %w", nodeID, err)
	}
	return nil
}

// syntheticCalibrationExisting accepts a replay only when every mutable field
// and every graph/ledger relation still matches the fixed inert receipt.  A
// modified row is a conflict, never a prompt to repair or overwrite history.
func syntheticCalibrationExisting(tx *sql.Tx, spec syntheticCalibrationSpec) (*syntheticCalibrationStored, bool, error) {
	var assetID int64
	var assetCanonical bool
	err := tx.QueryRow(`
SELECT id,
       type='app'
       AND COALESCE(app_name,'')=$2
       AND COALESCE(category,'')=$3
       AND COALESCE(app_description,'')=$4
       AND company_id IS NULL
       AND COALESCE(app_icp,'')=''
       AND company_source='scope'
       AND cardinality(task_ids)=1
FROM assets
WHERE type='app' AND bundle_id=$1
FOR UPDATE`, spec.BundleID, spec.AssetName, spec.AssetCategory, spec.AssetDescription).Scan(&assetID, &assetCanonical)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("lookup synthetic calibration fixture asset: %w", err)
	}
	if !assetCanonical {
		return nil, true, fmt.Errorf("%w: synthetic fixture asset differs from CAL-00", ErrSyntheticCalibrationConflict)
	}

	var assetLinks int
	if err := tx.QueryRow(`SELECT count(*) FROM task_asset_links WHERE asset_id=$1`, assetID).Scan(&assetLinks); err != nil {
		return nil, true, fmt.Errorf("count synthetic calibration asset links: %w", err)
	}
	if assetLinks != 1 {
		return nil, true, fmt.Errorf("%w: synthetic fixture asset has %d task links, want 1", ErrSyntheticCalibrationConflict, assetLinks)
	}

	rows, err := tx.Query(`
SELECT task.id, task.exploration_id, COALESCE(link.source_node_id, 0),
       (
         task.name=$4
         AND task.description=$5
         AND task.goal=$6
         AND task.status='done'
         AND task.paused=false
         AND task.queued=false
         AND task.queued_at IS NULL
         AND task.queue_mode=''
         AND task.paused_at IS NULL
         AND task.completed_at IS NOT NULL
         AND task.first_run_at IS NULL
         AND task.deadline_at IS NULL
         AND task.parent_ref IS NULL
         AND task.category_id IS NULL
         AND task.company_id IS NULL
         AND task.pinned_at IS NULL
         AND task.archived_at IS NULL
         AND task.timeout_seconds=0
         AND task.plan_heartbeat_seconds=$7
         AND task.plan_quiet_mode=true
         AND task.coverage_enabled=false
         AND task.llm_rotate=false
         AND task.target_gate=true
         AND task.llm_profile_id IS NULL
         AND task.active_llm_profile_id IS NULL
         AND task.llm_chain_revision=0
		 AND task.system_kind=$8
         AND COALESCE(task.end_reason,'')=''
         AND NOT EXISTS (SELECT 1 FROM task_llm_profiles profile WHERE profile.task_id=task.id)
         AND (SELECT count(*) FROM task_asset_links task_link WHERE task_link.task_id=task.id)=1
         AND (SELECT count(*) FROM assets associated WHERE task.id=ANY(associated.task_ids))=1
       )
FROM task_asset_links link
JOIN tasks task ON task.id=link.task_id AND task.deleted_at IS NULL
JOIN assets asset ON asset.id=link.asset_id AND task.id=ANY(asset.task_ids)
WHERE link.asset_id=$1
  AND link.source=$2
  AND link.source_summary=$3
ORDER BY task.id
FOR UPDATE OF link, task`, assetID, syntheticCalibrationSource, spec.Marker, spec.Name, spec.Description, spec.Goal, MinPlanHeartbeatSeconds, spec.SystemKind)
	if err != nil {
		return nil, true, fmt.Errorf("lookup synthetic calibration task: %w", err)
	}
	defer rows.Close()

	type candidate struct {
		taskID, explorationID, originFactID int64
		canonical                           bool
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.taskID, &c.explorationID, &c.originFactID, &c.canonical); err != nil {
			return nil, true, err
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return nil, true, err
	}
	if len(candidates) != 1 {
		return nil, true, fmt.Errorf("%w: synthetic fixture has %d matching calibration tasks, want 1", ErrSyntheticCalibrationConflict, len(candidates))
	}
	c := candidates[0]
	if !c.canonical {
		return nil, true, fmt.Errorf("%w: synthetic calibration task is mutable or runnable", ErrSyntheticCalibrationConflict)
	}

	intentID, factIDs, err := verifySyntheticCalibrationGraph(tx, c.explorationID, c.originFactID, assetID, spec)
	if err != nil {
		return nil, true, err
	}
	if err := verifySyntheticCalibrationNoForbiddenRecords(tx, c.taskID, c.explorationID); err != nil {
		return nil, true, err
	}
	return &syntheticCalibrationStored{
		taskID: c.taskID, assetID: assetID, originFactID: c.originFactID, intentID: intentID, factIDs: factIDs, alreadyPresent: true,
	}, true, nil
}

func verifySyntheticCalibrationGraph(tx *sql.Tx, explorationID, originFactID, assetID int64, spec syntheticCalibrationSpec) (int64, map[string]int64, error) {
	if originFactID <= 0 {
		return 0, nil, fmt.Errorf("%w: synthetic calibration has no origin link", ErrSyntheticCalibrationConflict)
	}
	var explorationCanonical bool
	if err := tx.QueryRow(`
SELECT EXISTS (
  SELECT 1 FROM explorations
  WHERE id=$1 AND COALESCE(description,'')=$2 AND goal=$3 AND status='achieved' AND round_no=0
)`, explorationID, spec.Description, spec.Goal).Scan(&explorationCanonical); err != nil {
		return 0, nil, fmt.Errorf("verify synthetic calibration exploration: %w", err)
	}
	if !explorationCanonical {
		return 0, nil, fmt.Errorf("%w: synthetic calibration exploration differs from fixed receipt", ErrSyntheticCalibrationConflict)
	}

	originPayload, err := syntheticCalibrationOriginPayload(spec)
	if err != nil {
		return 0, nil, err
	}
	var originCanonical bool
	if err := tx.QueryRow(`
SELECT EXISTS (
  SELECT 1 FROM exploration_nodes node
  JOIN exploration_anchors anchor ON anchor.node_id=node.id AND anchor.asset_id=$3
  WHERE node.id=$1 AND node.exploration_id=$2
    AND node.kind=$4 AND node.state=$5 AND node.origin=$6
    AND node.owner IS NULL AND node.completed_at IS NULL AND node.payload=$7::jsonb
)`, originFactID, explorationID, assetID, KindFact, StateOrigin, syntheticCalibrationSource, originPayload).Scan(&originCanonical); err != nil {
		return 0, nil, fmt.Errorf("verify synthetic calibration origin: %w", err)
	}
	if !originCanonical {
		return 0, nil, fmt.Errorf("%w: synthetic calibration origin differs from fixed receipt", ErrSyntheticCalibrationConflict)
	}

	intentPayload, err := syntheticCalibrationIntentPayload(spec)
	if err != nil {
		return 0, nil, err
	}
	var intentID int64
	if err := tx.QueryRow(`
SELECT node.id
FROM exploration_nodes node
JOIN exploration_anchors anchor ON anchor.node_id=node.id AND anchor.asset_id=$2
JOIN exploration_edges edge
  ON edge.exploration_id=node.exploration_id AND edge.src_id=$3 AND edge.rel=$4 AND edge.dst_id=node.id
WHERE node.exploration_id=$1
  AND node.kind=$5 AND node.state='done' AND node.origin=$6
  AND node.owner IS NULL AND node.completed_at IS NOT NULL AND node.payload=$7::jsonb`,
		explorationID, assetID, originFactID, RelDerivedFrom, KindIntent, syntheticCalibrationSource, intentPayload).Scan(&intentID); err != nil {
		if err == sql.ErrNoRows {
			return 0, nil, fmt.Errorf("%w: synthetic calibration terminal intent is missing", ErrSyntheticCalibrationConflict)
		}
		return 0, nil, fmt.Errorf("verify synthetic calibration intent: %w", err)
	}

	factIDs := make(map[string]int64, len(spec.Facts))
	for _, fact := range spec.Facts {
		payload, err := syntheticCalibrationFactPayload(spec, fact)
		if err != nil {
			return 0, nil, err
		}
		var factID int64
		err = tx.QueryRow(`
SELECT node.id
FROM exploration_nodes node
JOIN exploration_anchors anchor ON anchor.node_id=node.id AND anchor.asset_id=$2
JOIN exploration_edges edge
  ON edge.exploration_id=node.exploration_id AND edge.src_id=$3 AND edge.rel=$4 AND edge.dst_id=node.id
WHERE node.exploration_id=$1
  AND node.kind=$5 AND node.state='confirmed' AND node.origin=$6
  AND node.owner IS NULL AND node.completed_at IS NOT NULL AND node.payload=$7::jsonb`,
			explorationID, assetID, intentID, RelYields, KindFact, syntheticCalibrationSource, payload).Scan(&factID)
		if err != nil {
			if err == sql.ErrNoRows {
				return 0, nil, fmt.Errorf("%w: synthetic calibration fact %q is missing", ErrSyntheticCalibrationConflict, fact.ID)
			}
			return 0, nil, fmt.Errorf("verify synthetic calibration fact %q: %w", fact.ID, err)
		}
		factIDs[fact.ID] = factID
	}

	expectedNodes := len(spec.Facts) + 2
	expectedEdges := len(spec.Facts) + 1
	var nodes, edges, anchors, assetAnchors int
	if err := tx.QueryRow(`
SELECT
  (SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1),
  (SELECT count(*) FROM exploration_edges WHERE exploration_id=$1),
  count(*),
  count(*) FILTER (WHERE anchor.asset_id=$2)
FROM exploration_anchors anchor
JOIN exploration_nodes node ON node.id=anchor.node_id
WHERE node.exploration_id=$1`, explorationID, assetID).Scan(&nodes, &edges, &anchors, &assetAnchors); err != nil {
		return 0, nil, fmt.Errorf("count synthetic calibration graph: %w", err)
	}
	if nodes != expectedNodes || edges != expectedEdges || anchors != expectedNodes || assetAnchors != expectedNodes {
		return 0, nil, fmt.Errorf("%w: synthetic calibration graph counts nodes=%d edges=%d anchors=%d asset_anchors=%d, want %d/%d/%d/%d", ErrSyntheticCalibrationConflict, nodes, edges, anchors, assetAnchors, expectedNodes, expectedEdges, expectedNodes, expectedNodes)
	}
	return intentID, factIDs, nil
}

func verifySyntheticCalibrationNoForbiddenRecords(tx *sql.Tx, taskID, explorationID int64) error {
	var count int64
	taskText := strconv.FormatInt(taskID, 10)
	if err := tx.QueryRow(`
SELECT
  (SELECT count(*) FROM task_constraints WHERE exploration_id=$2)
  + (SELECT count(*) FROM activity WHERE exploration_id=$2)
  + (SELECT count(*) FROM main_sessions WHERE exploration_id=$2)
  + (SELECT count(*) FROM task_archives WHERE task_id=$1)
  + (SELECT count(*) FROM task_relations WHERE task_id=$1 OR source_task_id=$1)
  + (SELECT count(*) FROM task_llm_profiles WHERE task_id=$1)
  + (SELECT count(*) FROM task_scope WHERE task_id=$1)
  + (SELECT count(*) FROM llm_records WHERE COALESCE(task_id,'')=$3)
  + (SELECT count(*) FROM llm_usage WHERE COALESCE(task_id,'')=$3 OR exploration_id=$2)
  + (SELECT count(*) FROM skill_usage WHERE task_id=$1 OR exploration_id=$2)
  + (SELECT count(*) FROM tool_usage WHERE task_id=$1 OR exploration_id=$2)
  + (SELECT count(*) FROM intercept_pending
       WHERE COALESCE(task_id,'')=$3
          OR (task_id ~ '^[+]?[0-9]+$'
              AND COALESCE(NULLIF(regexp_replace(task_id, '^[+]?0*', ''), ''), '0')=$3))
  + (SELECT count(*) FROM target_pins WHERE task_id=$1)
  + (SELECT count(*) FROM findings WHERE task_id=$1)
  + (SELECT count(*) FROM side_question_sessions WHERE task_id=$1 OR exploration_id=$2)
  + (SELECT count(*) FROM task_intercept_rules WHERE task_id=$1)
  + (SELECT count(*) FROM task_credentials WHERE task_id=$1)
  + (SELECT count(*) FROM rsi_lesson_usage WHERE task_id=$1)
  + (SELECT count(*) FROM coverage_ledger WHERE task_id=$1)
  + (SELECT count(*) FROM coverage_ledger_history WHERE task_id=$1)`, taskID, explorationID, taskText).Scan(&count); err != nil {
		return fmt.Errorf("count forbidden synthetic calibration records: %w", err)
	}
	if count != 0 {
		return fmt.Errorf("%w: synthetic calibration has %d execution, scope, target, or evidence records", ErrSyntheticCalibrationConflict, count)
	}
	return nil
}
