package server

import (
	"log"
	"net/http"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/egress"
)

// operatorReadinessResponse is an authenticated, non-secret deployment summary
// for the dashboard. It must stay observational: no field asserts target
// authorization, provider privacy, listener ownership, connection success, or
// agent approval, and it never carries task/profile/endpoint/proxy content.
type operatorReadinessResponse struct {
	Policy    operatorReadinessPolicy    `json:"policy"`
	Settings  operatorReadinessSettings  `json:"settings"`
	Bootstrap operatorReadinessBootstrap `json:"bootstrap"`
	Hermes    operatorReadinessHermes    `json:"hermes"`
}

type operatorReadinessPolicy struct {
	TargetGateRequired       bool                    `json:"target_gate_required"`
	UntrustedOutputBoundary  bool                    `json:"untrusted_output_boundary"`
	TaskBudget               operatorReadinessBudget `json:"task_budget"`
	IntentBudget             operatorReadinessBudget `json:"intent_budget"`
}

// nil denotes ARTEX's normal unlimited dimension. It is intentionally not the
// raw environment value: malformed, unset, and zero values all enforce as
// unlimited through agent.BudgetsFromEnv and should look the same here.
type operatorReadinessBudget struct {
	Tokens *int64 `json:"tokens"`
	Turns  *int64 `json:"turns"`
}

type operatorReadinessSettings struct {
	NoiseProfile              string `json:"noise_profile"`
	Workers                   int    `json:"workers"`
	TaskConcurrencyEnabled    bool   `json:"task_concurrency_enabled"`
	TaskConcurrencyLimit      int    `json:"task_concurrency_limit"`
	ConstraintsInjectPlanner  bool   `json:"constraints_inject_planner"`
	ConstraintsInjectWorker   bool   `json:"constraints_inject_worker"`
	TrafficCapture            bool   `json:"traffic_capture"`
	AgentTrafficBinding       bool   `json:"agent_traffic_binding"`
	LLMRecord                 bool   `json:"llm_record"`
	WebSearchEnabled          bool   `json:"web_search_enabled"`
	GlobalProxySet            bool   `json:"global_proxy_set"`
	WebSearchProxySet         bool   `json:"web_search_proxy_set"`
	LLMPoolEnabled            bool   `json:"llm_pool_enabled"`
	LLMPoolBindFallback       bool   `json:"llm_pool_bind_fallback"`
}

type operatorReadinessBootstrap struct {
	SyntheticCalibration string                                  `json:"synthetic_calibration"`
	WorkflowTemplates    db.OperatorReadinessTemplateStatus      `json:"workflow_templates"`
}

type operatorReadinessHermes struct {
	SubscriptionProfilePresent bool `json:"subscription_profile_present"`
	AgentExecutionEnabled      bool `json:"agent_execution_enabled"`
}

func operatorReadinessBudgetCap(value int64) *int64 {
	if value == agent.Unlimited {
		return nil
	}
	copy := value
	return &copy
}

// operatorReadiness returns a small, read-only summary intended for the
// authenticated dashboard. Do not expand it into a general configuration dump:
// profile endpoints/models, proxy values, task content, targets, traffic, and
// credentials belong to their existing, deliberately scoped views.
func (s *Server) operatorReadiness(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}

	calibration, err := pg.OperatorReadinessSyntheticCalibrationState()
	if err != nil {
		log.Printf("[operator-readiness] read CAL-00 state: %v", err)
		writeErr(w, http.StatusInternalServerError, "could not load operator readiness")
		return
	}
	templates, err := pg.OperatorReadinessWorkflowTemplateStatus(researchWorkflowTemplateInputs)
	if err != nil {
		log.Printf("[operator-readiness] read workflow-template state: %v", err)
		writeErr(w, http.StatusInternalServerError, "could not load operator readiness")
		return
	}
	hermesProfile, err := pg.OperatorReadinessHasProfileFormat("hermes-subscription")
	if err != nil {
		log.Printf("[operator-readiness] read Hermes profile presence: %v", err)
		writeErr(w, http.StatusInternalServerError, "could not load operator readiness")
		return
	}

	taskBudget, intentBudget := agent.BudgetsFromEnv()
	webSearchEnabled, _, _, _, webSearchProxy := s.m.WebSearch()
	concurrencyEnabled, concurrencyLimit := s.m.ConcurrencyLimit()
	response := operatorReadinessResponse{
		Policy: operatorReadinessPolicy{
			TargetGateRequired:      s.m.RequireTargetGate(),
			UntrustedOutputBoundary: agent.UntrustedOutputBoundaryEnabled(),
			TaskBudget: operatorReadinessBudget{
				Tokens: operatorReadinessBudgetCap(taskBudget.Tokens),
				Turns:  operatorReadinessBudgetCap(taskBudget.Turns),
			},
			IntentBudget: operatorReadinessBudget{
				Tokens: operatorReadinessBudgetCap(intentBudget.Tokens),
				Turns:  operatorReadinessBudgetCap(intentBudget.Turns),
			},
		},
		Settings: operatorReadinessSettings{
			NoiseProfile:             egress.Current().Profile().Name,
			Workers:                  s.m.Workers(),
			TaskConcurrencyEnabled:   concurrencyEnabled,
			TaskConcurrencyLimit:     concurrencyLimit,
			ConstraintsInjectPlanner: s.constraintInjectPlanner(),
			ConstraintsInjectWorker:  s.constraintInjectWorker(),
			TrafficCapture:           s.m.TrafficEnabled(),
			AgentTrafficBinding:      s.m.pg.GetBool(settingAgentTrafficBinding, false),
			LLMRecord:                s.m.LLMRecordEnabled(),
			WebSearchEnabled:         webSearchEnabled,
			GlobalProxySet:           strings.TrimSpace(s.m.GlobalProxy()) != "",
			WebSearchProxySet:        strings.TrimSpace(webSearchProxy) != "",
			LLMPoolEnabled:           s.m.LLMPoolEnabled(),
			LLMPoolBindFallback:      s.m.LLMPoolBindFallback(),
		},
		Bootstrap: operatorReadinessBootstrap{
			SyntheticCalibration: calibration,
			WorkflowTemplates:    templates,
		},
		Hermes: operatorReadinessHermes{
			SubscriptionProfilePresent: hermesProfile,
			AgentExecutionEnabled:      hermesSubscriptionAgentExecutionAllowed(),
		},
	}
	writeJSON(w, http.StatusOK, response)
}
