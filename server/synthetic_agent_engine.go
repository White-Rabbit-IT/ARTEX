package server

import (
	"context"
	"log"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// RunPreparedSyntheticAgentCalibration accepts only the fenced lease returned
// by Manager.PrepareSyntheticAgentCalibrationRun. It is deliberately separate
// from Engine.Run: no generic route may validate a graph and later claim a
// different frontier, or transition a task to running without that transaction.
func (e *Engine) RunPreparedSyntheticAgentCalibration(parent context.Context, run *syntheticAgentCalibrationRun) bool {
	if e == nil || e.m == nil || run == nil || run.Task == nil || !run.Task.IsSyntheticAgentCalibration() ||
		!run.Started || run.IntentID <= 0 || run.ClaimToken == "" {
		return false
	}
	t := run.Task
	if run.Retried {
		// The DB transaction already cleared persisted timing/end state. Clear
		// only in-memory per-run bookkeeping before the next fixed attempt; this
		// does not touch an ordinary task or generic execution route.
		e.resetSyntheticAgentCalibrationAttempt(t.ID)
	}
	e.deleteMu.RLock()
	if e.IsDeleting(t.ID) {
		e.deleteMu.RUnlock()
		return false
	}
	if _, loaded := e.started.LoadOrStore(t.ID, true); loaded {
		e.deleteMu.RUnlock()
		return false
	}
	rt := e.registerTaskRoutines(parent, t.ID, 1)
	e.deleteMu.RUnlock()
	runTaskRoutine(rt, func(ctx context.Context) {
		e.executePreparedSyntheticAgentCalibration(ctx, t, run.IntentID, run.ClaimToken)
	})
	return true
}

func (e *Engine) resetSyntheticAgentCalibrationAttempt(taskID string) {
	if e == nil {
		return
	}
	e.settling.Delete(taskID)
	e.deadline.Delete(taskID)
	e.stamped.Delete(taskID)
	e.coordStarted.Delete(taskID)
	e.budgets.Delete(taskID)
	e.paused.Delete(taskID)
	e.lastAct.Delete(taskID)
	e.llmCalls.Delete(taskID)
	e.goalFP.Delete(taskID)
	e.dropCnt.Delete(taskID)
	e.plannerRound.Delete(taskID)
}

// syntheticAgentCalibrationAttemptContext supplies the lifecycle-level wall
// clock for the whole prepared run.  It deliberately sits outside agentcore's
// turn accounting so a transport that never returns from a provider request
// still observes cancellation and releases the fenced lease.
func syntheticAgentCalibrationAttemptContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, time.Duration(db.SyntheticAgentCalibrationTaskTimeoutSeconds)*time.Second)
}

// abortPreparedSyntheticAgentCalibration closes a lease that was already
// admitted by the database but cannot safely proceed in this process.  It is
// intentionally separate from generic task cancellation: the database helper
// requires the prepared intent identity and exact claim token, so a stale local
// goroutine cannot terminalise a later attempt.
func (e *Engine) abortPreparedSyntheticAgentCalibration(t *Task, intentID int64, token, phase string) bool {
	if e == nil || e.m == nil || t == nil || !t.IsSyntheticAgentCalibration() || intentID <= 0 || token == "" {
		return false
	}
	won, _, err := e.m.AbortPreparedSyntheticAgentCalibration(t.ID, intentID, token)
	if err != nil {
		log.Printf("[synthetic-agent-calibration] task %s cannot abort prepared lease during %s: %v", t.ID, phase, err)
		return false
	}
	if !won {
		log.Printf("[synthetic-agent-calibration] task %s prepared lease was already settled during %s", t.ID, phase)
		return false
	}
	log.Printf("[synthetic-agent-calibration] task %s failed prepared lease during %s", t.ID, phase)
	return true
}

func (e *Engine) executePreparedSyntheticAgentCalibration(ctx context.Context, t *Task, intentID int64, token string) {
	if t == nil || !t.IsSyntheticAgentCalibration() {
		return
	}
	// A bounded attempt must never poison later retries. The started marker is
	// removed on every return, including a worker/provider/storage failure.
	defer e.started.Delete(t.ID)
	if e.IsDeleting(t.ID) {
		e.abortPreparedSyntheticAgentCalibration(t, intentID, token, "delete barrier before execution")
		return
	}
	if !e.beginTaskOperation(t.ID) {
		e.abortPreparedSyntheticAgentCalibration(t, intentID, token, "operation admission")
		return
	}
	defer e.decInflight(t.ID)

	intent, err := t.Store.GetNode(intentID)
	if err != nil || intent == nil || intent.Kind != db.KindIntent {
		log.Printf("[synthetic-agent-calibration] task %s cannot read its prepared fixed intent: %v", t.ID, err)
		e.abortPreparedSyntheticAgentCalibration(t, intentID, token, "prepared intent read")
		return
	}
	if e.IsPaused(t.ID) || e.IsDeleting(t.ID) {
		e.abortPreparedSyntheticAgentCalibration(t, intentID, token, "pause or delete barrier before worker")
		return
	}
	_, worker := e.snapshotFor(t)
	if worker == nil {
		e.abortPreparedSyntheticAgentCalibration(t, intentID, token, "worker unavailable")
		return
	}

	// The worker has its own SDK turn bound, but this outer cancellation is the
	// lifecycle backstop for a local Hermes transport that blackholes an HTTP
	// call and would otherwise keep renewing the prepared lease indefinitely.
	attemptCtx, cancelAttempt := syntheticAgentCalibrationAttemptContext(ctx)
	defer cancelAttempt()
	calibrationCtx, receipt := agent.NewSyntheticCalibrationContext(attemptCtx)
	e.runIntent(calibrationCtx, t, "synthetic-calibration", worker, intent, token, "", "")
	if e.IsDeleting(t.ID) {
		e.abortPreparedSyntheticAgentCalibration(t, intentID, token, "delete barrier after worker")
		return
	}
	completed := false
	if node, nodeErr := t.Store.GetNode(intent.ID); nodeErr == nil && node != nil {
		completed = node.State == "done"
	}
	// This is topology-only verification after the Worker finishes; terminal
	// state pairing and the structured attempt receipt are atomically checked by
	// FinishSyntheticAgentCalibration below.
	_, topologyErr := e.m.SyntheticAgentCalibrationIntentID(t.ID)
	success := receipt.ReceiptCallCount() == 1 &&
		receipt.ModelCallCount() == agent.SyntheticCalibrationMaxModelCalls &&
		completed && topologyErr == nil
	e.finishPreparedSyntheticAgentCalibration(t, intentID, token, success, receipt.ReceiptCallCount(), receipt.ModelCallCount())
}

func (e *Engine) finishPreparedSyntheticAgentCalibration(t *Task, intentID int64, token string, success bool, receiptCalls, modelCalls uint32) {
	if e == nil || e.m == nil || t == nil || !t.IsSyntheticAgentCalibration() {
		return
	}
	status := "failed"
	if success {
		status = "done"
	}
	won, _, err := e.m.FinishSyntheticAgentCalibration(t.ID, status, receiptCalls, modelCalls)
	if err != nil {
		log.Printf("[synthetic-agent-calibration] task %s cannot enter %s: %v", t.ID, status, err)
		// The generic worker can observe parent cancellation after it starts and
		// roll the claim back to open.  The dedicated abort transition knows how
		// to recognise and terminalise that residue; it is the only fallback here.
		e.abortPreparedSyntheticAgentCalibration(t, intentID, token, "terminal receipt recovery")
		return
	}
	if !won {
		log.Printf("[synthetic-agent-calibration] task %s terminal outcome lost to an earlier writer", t.ID)
		return
	}
	log.Printf("[synthetic-agent-calibration] task %s finished status=%s", t.ID, status)
}
