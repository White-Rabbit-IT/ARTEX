package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// ErrSystemTaskImmutable is returned when normal engagement controls attempt
// to mutate or admit an ARTEX-owned system fixture. System tasks are evidence
// fixtures, not paused work waiting for an operator to resume them.
var ErrSystemTaskImmutable = errors.New("system task is fixed and cannot be modified or executed")

const syntheticCalibrationImmutableMessage = "CAL-00 is a fixed synthetic calibration record and cannot be modified or executed"

// requireMutableTask is the server-side authorization boundary for a normal
// task write. The frontend may label CAL-00 as read-only, but callers can
// invoke HTTP directly, so this check deliberately lives on the server.
func (s *Server) requireMutableTask(w http.ResponseWriter, task *Task) bool {
	if task != nil && task.IsSyntheticCalibration() {
		writeErr(w, http.StatusConflict, syntheticCalibrationImmutableMessage)
		return false
	}
	return true
}

// isSyntheticCalibrationTaskID resolves the persistent system marker, using
// the live task when available and PostgreSQL as a cold-load fallback. The
// fallback matters for background paths and future callers that receive only a
// numeric task id rather than a live *Task.
func (s *Server) isSyntheticCalibrationTaskID(id int64) (bool, error) {
	if id <= 0 {
		return false, nil
	}
	if task, found := s.m.Task(strconv.FormatInt(id, 10)); found {
		return task.IsSyntheticCalibration(), nil
	}
	if s.m.pg == nil {
		return false, nil
	}
	stored, err := s.m.pg.GetTask(id)
	if err != nil || stored == nil {
		return false, err
	}
	return stored.IsSyntheticCalibration(), nil
}

// requireMutableTaskID is the equivalent guard for APIs whose task reference
// is carried in a body/query instead of the normal /api/tasks/{id} path.
func (s *Server) requireMutableTaskID(w http.ResponseWriter, id int64) bool {
	isSynthetic, err := s.isSyntheticCalibrationTaskID(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not resolve task classification")
		return false
	}
	if isSynthetic {
		writeErr(w, http.StatusConflict, syntheticCalibrationImmutableMessage)
		return false
	}
	return true
}

// systemTaskMutationGuard closes every ordinary, task-scoped write route in
// one place. It runs after JWT authentication and before a handler reads a
// body, creates files, updates a graph, or changes task lifecycle state.
//
// The routes whose task id is carried in a query/body/finding are guarded at
// their point of resolution instead; see chat, chatUpload, newMainSession and
// deepenFinding. GET/HEAD remain available so CAL-00 can be inspected in the
// ordinary task list and graph UI.
func (s *Server) systemTaskMutationGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if id, ok := taskIDFromTaskScopedPath(r.URL.Path); ok {
			numericID, err := strconv.ParseInt(id, 10, 64)
			if err == nil && !s.requireMutableTaskID(w, numericID) {
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func taskIDFromTaskScopedPath(path string) (string, bool) {
	const prefix = "/api/tasks/"
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	remainder := strings.TrimPrefix(path, prefix)
	id := remainder
	if slash := strings.IndexByte(remainder, '/'); slash >= 0 {
		id = remainder[:slash]
	}
	return canonicalTaskID(id)
}

func (s *Server) systemTaskInIDs(ids []string) *Task {
	for _, id := range ids {
		numericID, err := strconv.ParseInt(id, 10, 64)
		if err != nil || numericID <= 0 {
			continue
		}
		isSynthetic, err := s.isSyntheticCalibrationTaskID(numericID)
		if err != nil || !isSynthetic {
			continue
		}
		if task, ok := s.m.Task(id); ok {
			return task
		}
		return &Task{ID: id, systemKind: "synthetic_calibration"}
	}
	return nil
}
