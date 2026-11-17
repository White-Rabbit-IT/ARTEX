package server

import (
	"io"
	"log"
	"net/http"
)

// createSyntheticCalibration creates the one fixed CAL-00 UI/provenance
// fixture. It intentionally accepts no request data: arbitrary task text,
// target URLs, credentials, provider configuration, and tools belong to normal
// task admission, never to this safe calibration path.
func (s *Server) createSyntheticCalibration(w http.ResponseWriter, r *http.Request) {
	// Read at most one byte solely to reject a chunked body whose Content-Length
	// is unknown or dishonest. Do not decode, retain, or log caller data.
	if r.Body != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "could not read calibration request")
			return
		}
		if r.ContentLength > 0 || len(body) != 0 {
			writeErr(w, http.StatusBadRequest, "synthetic calibration accepts an empty request body")
			return
		}
	}

	task, alreadyPresent, err := s.m.EnsureSyntheticCalibrationTask()
	if err != nil {
		log.Printf("[calibration] create CAL-00: %v", err)
		writeErr(w, http.StatusInternalServerError, "could not create synthetic calibration")
		return
	}
	code := http.StatusCreated
	if alreadyPresent {
		code = http.StatusOK
	}
	writeJSON(w, code, map[string]any{
		"kind":            "synthetic_calibration",
		"runnable":        false,
		"provider_called": false,
		"already_present": alreadyPresent,
		"task":            taskDTO(task, s.resolvedTaskStatus(task)),
	})
}
