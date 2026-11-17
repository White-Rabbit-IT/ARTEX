package server

import (
	"errors"
	"os"

	"github.com/Autumn-27/artex/agent"
)

// HermesSubscriptionAgentExecutionEnv is an explicit, process-local opt-in for
// routing ARTEX agents through a Hermes Subscription Proxy. The proxy's login
// belongs to Hermes rather than ARTEX, so a reachable endpoint or a successful
// calibration request is not enough to authorize autonomous agent execution.
const HermesSubscriptionAgentExecutionEnv = "ARTEX_HERMES_SUBSCRIPTION_ALLOW_AGENT_EXECUTION"

const hermesSubscriptionAgentExecutionDisabledMessage = "Hermes Subscription Proxy is calibration-only; set ARTEX_HERMES_SUBSCRIPTION_ALLOW_AGENT_EXECUTION=1 to enable task and chat agent execution"

// hermesSubscriptionAgentExecutionAllowed intentionally accepts only the
// literal value 1. A typo, an unset variable, or an inherited boolean-looking
// value must leave the external-agent lane disabled.
func hermesSubscriptionAgentExecutionAllowed() bool {
	return os.Getenv(HermesSubscriptionAgentExecutionEnv) == "1"
}

// hermesSubscriptionAgentExecutionError is the policy decision at the config
// boundary. It is deliberately independent of connectivity: a connection test
// may use a Hermes profile while agent/task execution remains disabled.
func hermesSubscriptionAgentExecutionError(cfg agent.Config) error {
	if !cfg.IsHermesSubscription() || hermesSubscriptionAgentExecutionAllowed() {
		return nil
	}
	return errors.New(hermesSubscriptionAgentExecutionDisabledMessage)
}

// profileAgentExecutionError resolves just enough state to tell whether an
// existing profile is blocked for agent execution. A cached provider is checked
// first so a runtime environment change cannot be bypassed by an already-built
// profile agent. Missing or otherwise unusable profiles retain ARTEX's existing
// fallback/error behavior; this guard changes only Hermes Subscription Proxy.
func (s *Server) profileAgentExecutionError(id int64) error {
	if s == nil {
		return nil
	}
	s.provCacheMu.Lock()
	entry := s.provByProfile[id]
	s.provCacheMu.Unlock()
	if entry != nil {
		return hermesSubscriptionAgentExecutionError(entry.cfg)
	}
	if s.m == nil || s.m.pg == nil {
		return nil
	}
	cfg, ok := s.loadProfileConfig(id)
	if !ok {
		return nil
	}
	return hermesSubscriptionAgentExecutionError(cfg)
}

// agentBindingExecutionError is separate from a task's profile-chain fallback:
// a deliberately bound, disabled Hermes profile must not silently send that
// agent's task context to a different global or chain provider.
func (s *Server) agentBindingExecutionError(agentKey string) error {
	id := s.effectiveProfileForAgent(agentKey, nil)
	if id == nil {
		return nil
	}
	return s.profileAgentExecutionError(*id)
}

// globalAgentExecutionError inspects the installed global configuration without
// assuming it has a live provider. applyLLM keeps the blocked configuration for
// status/error reporting but clears its runtime providers.
func (s *Server) globalAgentExecutionError() error {
	if s == nil {
		return nil
	}
	s.cfgMu.Lock()
	cfg := s.llmCfg
	s.cfgMu.Unlock()
	return hermesSubscriptionAgentExecutionError(cfg)
}

// disableHermesSubscriptionRuntime removes any previously installed global
// provider/ChatAgent before returning the policy error. This matters when an
// operator activates a blocked Hermes profile after a different profile: stale
// global agents must not remain usable as an implicit fallback.
func (s *Server) disableHermesSubscriptionRuntime(cfg agent.Config) {
	if s == nil {
		return
	}
	s.cfgMu.Lock()
	s.chatAgent = nil
	s.llmProv = nil
	s.llmDirect = nil
	s.llmCfg = cfg
	s.llmOn = false
	s.cfgMu.Unlock()
	s.invalidateTaskAgents()
}
