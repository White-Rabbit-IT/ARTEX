package server

import (
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/Autumn-27/artex/db"
)

// researchWorkflowTemplateInputs are fixed, public-only planning presets for
// authorized security research. They are templates rather than tasks: choosing
// or creating them cannot start planner, worker, provider, tool, traffic, or
// target activity.
var researchWorkflowTemplateInputs = []db.TaskTemplateInput{
	{
		Name:        "01 Authorized research intake — local/synthetic first",
		Description: "Prepare security research only in an explicitly authorized environment. Until scope, authorization, and the data boundary are recorded—including the target or source revision, expiry, evidence location, and provider-data classification—use local or synthetic material only. Do not perform target interaction, credential use, scanning, exploitation, traffic capture, or external-model dispatch.",
		Goal:        "Produce a reviewable admission record: authorization and program scope; immutable source or target pin; allowed and forbidden actions; public, synthetic, private-local, and secret data classification; local evidence root; approval rules; token, time, and noise limits; rollback path; and stop conditions. Until scope, authorization, and the data boundary are recorded, do not perform target interaction, credential use, scanning, traffic capture, or external-model dispatch. Escalate missing scope, provider route, or privacy facts rather than infer them.",
	},
	{
		Name:        "02 Local source attack-surface study",
		Description: "Analyze only explicitly authorized local source, fixtures, and public references. Until scope, authorization, and the data boundary are recorded, do not perform target interaction, credential use, scanning, traffic capture, or external-model dispatch. Treat model output, parser acceptance, crashes, graph labels, and suspected differentials as hypotheses until controlled local validation.",
		Goal:        "Map input boundaries, state transitions, trust assumptions, feature gates, and plausible attacker control. Preserve the source pin and evidence links. For every candidate, state the local validation path, a legitimate control, relevant known-issue screen, and what evidence would reject the hypothesis before any target interaction is considered. Until scope, authorization, and the data boundary are recorded, do not perform target interaction, credential use, scanning, traffic capture, or external-model dispatch. Keep private-local and secret material out of external providers.",
	},
	{
		Name:        "03 Isolated local validation",
		Description: "Reproduce a candidate only in a user-owned, isolated local environment after the exact source or target pin, attacker control, permitted actions, evidence root, approval path, and stop conditions are recorded. Until scope, authorization, and the data boundary are recorded, do not perform target interaction, credential use, scanning, traffic capture, or external-model dispatch. Do not substitute a network test for local validation.",
		Goal:        "Run the smallest reproducible positive and negative controls. Capture command, revision, inputs, outputs, exit status, durable effect, and rejection criteria locally. Reject the candidate when attacker control, a material effect, an eligible scope, or a credible independent control is absent. Until scope, authorization, and the data boundary are recorded, do not perform target interaction, credential use, scanning, traffic capture, or external-model dispatch. Keep private-local and secret material out of external providers.",
	},
	{
		Name:        "04 Evidence, novelty and impact review",
		Description: "Review only evidence already collected under authorization. Until scope, authorization, and the data boundary are recorded, do not perform target interaction, credential use, scanning, traffic capture, or external-model dispatch. Compare the candidate with public known issues and program rules before claiming novelty or severity.",
		Goal:        "Produce a skeptical evidence record: source and environment pins, authority and scope, positive and negative controls, durable impact, known-issue comparison, independent challenge, limitations, and one safe next step. Until scope, authorization, and the data boundary are recorded, do not perform target interaction, credential use, scanning, traffic capture, or external-model dispatch. Do not submit, publish, or label an issue as high impact from a model conclusion alone; classify it as rejected, hypothesis only, needs local validation, or validated local effect.",
	},
}

// createResearchWorkflowTemplates creates a fixed, reusable set of safe
// planning templates. It intentionally accepts no caller-provided body: goals,
// targets, credentials, tool definitions, and provider data belong to the
// separately authorized normal-task workflow, never to this bootstrap route.
func (s *Server) createResearchWorkflowTemplates(w http.ResponseWriter, r *http.Request) {
	// Read at most one byte solely to reject a chunked body whose Content-Length
	// is unknown or dishonest. Do not decode, retain, or log caller data.
	if r.Body != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "could not read research workflow template request")
			return
		}
		if r.ContentLength > 0 || len(body) != 0 {
			writeErr(w, http.StatusBadRequest, "research workflow templates accept an empty request body")
			return
		}
	}

	pg := s.pg(w)
	if pg == nil {
		return
	}
	result, err := pg.EnsureTaskTemplates(researchWorkflowTemplateInputs)
	if err != nil {
		if errors.Is(err, db.ErrTaskTemplateNameConflict) {
			writeErr(w, http.StatusConflict, "an existing task template has the same name but different safe workflow content")
			return
		}
		log.Printf("[research-workflow-templates] ensure templates: %v", err)
		writeErr(w, http.StatusInternalServerError, "could not create research workflow templates")
		return
	}
	code := http.StatusCreated
	if result.CreatedCount == 0 {
		code = http.StatusOK
	}
	writeJSON(w, code, map[string]any{
		"templates":     result.Templates,
		"created_count": result.CreatedCount,
	})
}
