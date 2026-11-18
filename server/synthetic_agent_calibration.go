package server

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// HermesSyntheticAgentCalibrationExecutionEnv is deliberately distinct from
// ARTEX_HERMES_SUBSCRIPTION_ALLOW_AGENT_EXECUTION.  The latter opens ordinary
// task/chat agent execution through Hermes; this one permits only the fixed,
// code-owned synthetic calibration worker when its persisted policy is present.
const HermesSyntheticAgentCalibrationExecutionEnv = "ARTEX_HERMES_SYNTHETIC_CALIBRATION_ALLOW"

const hermesSyntheticAgentCalibrationDisabledMessage = "synthetic Hermes calibration is disabled; set ARTEX_HERMES_SYNTHETIC_CALIBRATION_ALLOW=1 to run the fixed bounded calibration"

// syntheticAgentCalibrationMaxReplyTokens bounds each of the two possible
// worker turns. Together with the fixed two-turn cap and fixed tiny prompt, it
// is a task-scoped provider request cap rather than a mutable profile setting.
const syntheticAgentCalibrationMaxReplyTokens = 128

func hermesSyntheticAgentCalibrationAllowed() bool {
	return os.Getenv(HermesSyntheticAgentCalibrationExecutionEnv) == "1"
}

// createSyntheticAgentCalibration starts the one product-owned synthetic
// calibration task. A repeated start observes an existing running/terminal
// attempt; it cannot silently spend a new provider budget. Failed-attempt retry
// is a separately named empty-body action below.
func (s *Server) createSyntheticAgentCalibration(w http.ResponseWriter, r *http.Request) {
	s.startSyntheticAgentCalibration(w, r, false)
}

// retrySyntheticAgentCalibration is the only deliberate retry path. It accepts
// no task text or execution options; the DB transaction retains prior evidence,
// resets only the verified fixed intent, and records the retry receipt before it
// grants a new lease.
func (s *Server) retrySyntheticAgentCalibration(w http.ResponseWriter, r *http.Request) {
	s.startSyntheticAgentCalibration(w, r, true)
}

func (s *Server) startSyntheticAgentCalibration(w http.ResponseWriter, r *http.Request, allowRetry bool) {
	label := "synthetic agent calibration"
	if allowRetry {
		label = "synthetic agent calibration retry"
	}
	if !emptySyntheticCalibrationRequest(w, r, label) {
		return
	}
	if !hermesSyntheticAgentCalibrationAllowed() {
		writeErr(w, http.StatusConflict, hermesSyntheticAgentCalibrationDisabledMessage)
		return
	}
	profileID, err := s.syntheticAgentCalibrationProfileID()
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	task, alreadyPresent, err := s.m.EnsureSyntheticAgentCalibrationTask(profileID)
	if err != nil {
		log.Printf("[synthetic-agent-calibration] ensure task: %v", err)
		writeErr(w, http.StatusConflict, "could not create bounded synthetic agent calibration")
		return
	}
	if !task.IsSyntheticAgentCalibration() {
		// The helper's type is part of the security contract.  Do not start a
		// generic task if a future database regression returned another row.
		writeErr(w, http.StatusConflict, "synthetic agent calibration policy verification failed")
		return
	}
	status := task.lifecycleSnapshot().Status
	if allowRetry && status != "failed" && status != "timeout" {
		writeErr(w, http.StatusConflict, "only a failed bounded synthetic calibration may be retried")
		return
	}
	// A terminal DB status is written just before the special goroutine returns.
	// Do not reset a fresh failed task while that same process still owns its
	// in-memory routine; the explicit retry can be repeated after it releases.
	if allowRetry && s.engine.Started(task.ID) {
		writeErr(w, http.StatusConflict, "the previous bounded synthetic calibration is still finalizing; retry again once it has stopped")
		return
	}
	run, err := s.m.PrepareSyntheticAgentCalibrationRun(task.ID, profileID, allowRetry)
	if err != nil {
		log.Printf("[synthetic-agent-calibration] prepare task %s: %v", task.ID, err)
		writeErr(w, http.StatusConflict, "could not prepare bounded synthetic agent calibration")
		return
	}
	if run == nil || run.Task == nil || !run.Task.IsSyntheticAgentCalibration() {
		writeErr(w, http.StatusConflict, "synthetic agent calibration preparation verification failed")
		return
	}
	if run.Started && !s.engine.RunPreparedSyntheticAgentCalibration(s.ctx, run) {
		// This is a local lifecycle race, not a reason to route through the
		// generic engine.  Close the exact prepared DB lease now; leaving the
		// task running would make the explicit retry control unreachable until a
		// process restart.
		if won, _, abortErr := s.m.AbortPreparedSyntheticAgentCalibration(run.Task.ID, run.IntentID, run.ClaimToken); abortErr != nil {
			log.Printf("[synthetic-agent-calibration] abort local admission task %s: %v", run.Task.ID, abortErr)
			writeErr(w, http.StatusInternalServerError, "could not record bounded synthetic calibration admission failure")
			return
		} else if !won {
			log.Printf("[synthetic-agent-calibration] local admission task %s was already settled", run.Task.ID)
		}
		writeErr(w, http.StatusConflict, "bounded synthetic calibration could not acquire its local execution slot")
		return
	}
	code := http.StatusCreated
	if alreadyPresent || allowRetry || !run.Started {
		code = http.StatusOK
	}
	writeJSON(w, code, taskDTO(run.Task, s.resolvedTaskStatus(run.Task)))
}

// emptySyntheticCalibrationRequest rejects request content before any profile,
// database, provider, or task state is touched.  It mirrors the CAL-00 body
// rule but stays separate so the runnable path cannot accidentally inherit a
// future CAL-00 payload surface.
func emptySyntheticCalibrationRequest(w http.ResponseWriter, r *http.Request, label string) bool {
	if r.Body == nil {
		return true
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not read "+label+" request")
		return false
	}
	if r.ContentLength > 0 || len(body) != 0 {
		writeErr(w, http.StatusBadRequest, label+" accepts an empty request body")
		return false
	}
	return true
}

// syntheticAgentCalibrationProfileID chooses exactly one already-reviewed
// Hermes profile.  Selecting one through the usual profile UI lets ARTEX keep
// account edits, route validation and secret handling separate from this
// task-admission endpoint.  Ambiguity fails closed rather than silently picking
// a different model/provider.
func (s *Server) syntheticAgentCalibrationProfileID() (int64, error) {
	if s == nil || s.m == nil || s.m.pg == nil {
		return 0, fmt.Errorf("synthetic agent calibration is unavailable")
	}
	profiles, err := s.m.pg.ListProfiles()
	if err != nil {
		return 0, fmt.Errorf("could not inspect saved LLM profiles")
	}
	var matches []int64
	for _, profile := range profiles {
		if profile == nil {
			continue
		}
		cfg, ok := s.loadProfileConfig(profile.ID)
		if !ok || !syntheticAgentCalibrationProfileAllowed(profile, cfg) {
			continue
		}
		matches = append(matches, profile.ID)
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return 0, fmt.Errorf("configure exactly one pool-excluded, non-default Hermes Subscription profile before starting synthetic agent calibration")
	default:
		return 0, fmt.Errorf("synthetic agent calibration requires exactly one eligible Hermes Subscription profile; narrow the saved profiles before starting")
	}
}

// syntheticAgentCalibrationProfileAllowed is deliberately stricter than a
// normal Hermes profile: it requires an exact local /v1 route, no ARTEX proxy,
// no default-role routing and no participation in the fallback pool.
func syntheticAgentCalibrationProfileAllowed(profile *db.LLMProfile, cfg agent.Config) bool {
	if profile == nil || !cfg.IsHermesSubscription() || profile.Format != "hermes-subscription" {
		return false
	}
	// ConfigFrom intentionally discards an ARTEX proxy for Hermes as a
	// last-resort transport defence. Inspect the persisted row as well: using
	// cfg.Proxy alone would hide a legacy/sideloaded proxy value and incorrectly
	// classify that profile as eligible for this narrower lane.
	if profile.IsDefault || !profile.PoolExclude ||
		strings.TrimSpace(profile.Proxy) != "" || strings.TrimSpace(cfg.Proxy) != "" {
		return false
	}
	canonical, err := agent.ValidateHermesSubscriptionBaseURL(cfg.BaseURL)
	return err == nil && canonical == cfg.BaseURL
}
