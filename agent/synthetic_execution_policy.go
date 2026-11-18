package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"sync/atomic"

	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// ExecutionPolicySyntheticCalibrationV1 is the persisted task policy name for
// the first runnable, Hermes-backed calibration lane. It is intentionally a
// product-owned capability, not a prompt convention or a user-selectable tool
// profile. The server must check the persisted task policy before it creates
// the context with NewSyntheticCalibrationContext.
const ExecutionPolicySyntheticCalibrationV1 = "synthetic_calibration_v1"

// SyntheticReceiptToolName is the sole model-visible tool in a synthetic
// calibration worker. It has no inputs and performs no I/O. Its successful call
// is an auditable local receipt that the server can require before it marks a
// calibration successful.
const SyntheticReceiptToolName = "synthetic_receipt"

// SyntheticCalibrationMaxTurns is the hard model-turn ceiling for the worker
// receipt exchange: one tool-use turn plus one completion turn. It deliberately
// ignores an otherwise-unlimited persisted worker profile for this policy.
const SyntheticCalibrationMaxTurns = 2

// SyntheticCalibrationMaxModelCalls is an independent hard stop for provider
// requests. MaxTurns counts tool turns, so a repeated receipt call could
// otherwise cause an extra model continuation before the harness stops. The
// server calls ReserveSyntheticCalibrationModelCall immediately before each
// provider request for this policy.
const SyntheticCalibrationMaxModelCalls = 2

// syntheticPlannerUnavailableReason and syntheticMainAgentUnavailableReply are
// deliberately fixed local results. They keep an accidentally misrouted
// planner or human-chat invocation from constructing a prompt, observing a
// graph, or contacting a provider under the synthetic calibration capability.
const syntheticPlannerUnavailableReason = "synthetic calibration only runs the fixed receipt worker"

const syntheticMainAgentUnavailableReply = "Synthetic calibration only runs the fixed receipt worker; interactive main-agent chat is unavailable."

// SyntheticCalibrationState is the server-observable state for one calibration
// attempt. Its fields are deliberately private: task text, a model response, and
// package consumers cannot forge a receipt. A state is created only together
// with the private context capability by NewSyntheticCalibrationContext.
type SyntheticCalibrationState struct {
	receiptCalls atomic.Uint32
	modelCalls   atomic.Uint32
}

// ReceiptCalled reports whether the inert calibration receipt completed once.
func (s *SyntheticCalibrationState) ReceiptCalled() bool {
	return s != nil && s.receiptCalls.Load() > 0
}

// ReceiptCallCount returns the number of successful receipt calls. Version 1
// permits exactly one successful call, so a value other than zero or one is a
// programming error rather than a useful runtime state.
func (s *SyntheticCalibrationState) ReceiptCallCount() uint32 {
	if s == nil {
		return 0
	}
	return s.receiptCalls.Load()
}

// ModelCallCount returns the number of provider-call slots consumed by this
// calibration. It is state only; it does not identify a task, a provider, or a
// model route.
func (s *SyntheticCalibrationState) ModelCallCount() uint32 {
	if s == nil {
		return 0
	}
	return s.modelCalls.Load()
}

type syntheticCalibrationContextKey struct{}

// syntheticCalibrationCapability is private so only this package can construct
// the marker that activates the policy. In particular, an LLM prompt, a task
// description, a tool argument, or another package cannot manufacture it.
type syntheticCalibrationCapability struct {
	state *SyntheticCalibrationState
}

// NewSyntheticCalibrationContext adds the unforgeable, in-process capability
// for a server-validated synthetic calibration task. It does not itself approve
// a target, enable a generic Hermes agent, or change provider routing.
func NewSyntheticCalibrationContext(ctx context.Context) (context.Context, *SyntheticCalibrationState) {
	state := &SyntheticCalibrationState{}
	return context.WithValue(ctx, syntheticCalibrationContextKey{}, syntheticCalibrationCapability{state: state}), state
}

func syntheticCalibrationState(ctx context.Context) *SyntheticCalibrationState {
	capability, ok := ctx.Value(syntheticCalibrationContextKey{}).(syntheticCalibrationCapability)
	if !ok || capability.state == nil {
		return nil
	}
	return capability.state
}

// cleanSyntheticCalibrationContext removes every caller-supplied context value
// before a synthetic request reaches a provider. It preserves only the private
// receipt capability and relays cancellation from the caller. This keeps task
// ids, target scopes, review backgrounds, transcripts, run metadata, and other
// application values out of this deliberately static provider lane.
func cleanSyntheticCalibrationContext(parent context.Context) (context.Context, context.CancelFunc) {
	state := syntheticCalibrationState(parent)
	if state == nil {
		return parent, func() {}
	}
	base, cancel := context.WithCancel(context.Background())
	clean := context.WithValue(base, syntheticCalibrationContextKey{}, syntheticCalibrationCapability{state: state})
	go func() {
		select {
		case <-parent.Done():
			cancel()
		case <-base.Done():
		}
	}()
	return clean, cancel
}

// IsSyntheticAgentCalibration reports whether ctx carries the private
// synthetic-calibration capability. It is used only to tighten execution; an
// absent capability preserves the existing agent behavior.
func IsSyntheticAgentCalibration(ctx context.Context) bool {
	return syntheticCalibrationState(ctx) != nil
}

// ReserveSyntheticCalibrationModelCall consumes one of the two provider-call
// slots for the private synthetic-calibration context. It returns false both
// when the context is not a synthetic calibration and when its fixed budget is
// exhausted. The compare-and-swap loop is safe when a provider tries to issue
// requests concurrently.
func ReserveSyntheticCalibrationModelCall(ctx context.Context) bool {
	state := syntheticCalibrationState(ctx)
	if state == nil {
		return false
	}
	for {
		current := state.modelCalls.Load()
		if current >= SyntheticCalibrationMaxModelCalls {
			return false
		}
		if state.modelCalls.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

// FinaliseTools is the last tool boundary before agentcore receives a tool
// list. For ordinary runs it preserves the caller's tool set and deferred-MCP
// metadata exactly. For a synthetic calibration it discards every supplied
// tool -- SDK defaults, domain/catalog/host/custom tools, Skills, MCP, and the
// late-added WebFetch tool -- then clears deferred/unlock metadata so no later
// MCP settlement or skill-unlock path can reintroduce a tool.
//
// Only the worker is given the inert receipt tool. Planner and main-agent runs
// get no model-visible tools, which means a generic execution loop cannot use a
// planner receipt as proof that a bounded worker ran. The server owns the
// narrow worker lifecycle for this calibration policy.
func FinaliseTools(ctx context.Context, agentKey string, tools []actool.CoreTool, def DeferredInfo) ([]actool.CoreTool, DeferredInfo) {
	state := syntheticCalibrationState(ctx)
	if state == nil {
		return tools, def
	}
	if agentKey != "worker" {
		// agentcore treats a nil list as "use DefaultTools". Keep this
		// deliberately non-nil to express a true zero-tool session.
		return []actool.CoreTool{}, DeferredInfo{}
	}
	return []actool.CoreTool{guardPanic(newSyntheticReceiptTool(state))}, DeferredInfo{}
}

// newSyntheticReceiptTool deliberately creates a fresh code-owned tool rather
// than retaining a caller-supplied tool with the same name. That prevents a
// custom/catalog/MCP tool from masquerading as the receipt handler.
func newSyntheticReceiptTool(state *SyntheticCalibrationState) actool.CoreTool {
	base := actool.Build(actool.Spec{
		Name:        SyntheticReceiptToolName,
		Description: "Record the fixed local synthetic-calibration receipt. It accepts no arguments and performs no target, filesystem, process, network, credential, proxy, browser, scanner, skill, MCP, traffic, or graph action.",
		Schema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
		},
		ReadOnly:   func(json.RawMessage) bool { return true },
		Concurrent: func(json.RawMessage) bool { return false },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(_ context.Context, input json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			if !emptyJSONObject(input) {
				return actool.Errorf("synthetic_receipt accepts only an empty JSON object"), nil
			}
			if !state.receiptCalls.CompareAndSwap(0, 1) {
				return actool.Errorf("synthetic_receipt was already recorded for this calibration"), nil
			}
			return actool.Text("synthetic calibration receipt recorded: no target, filesystem, process, network, credential, proxy, browser, scanner, skill, MCP, traffic, or graph action occurred"), nil
		},
	})
	return &syntheticReceiptGuard{CoreTool: base, state: state}
}

// syntheticReceiptGuard defends the retained handler at call time as well as at
// schema assembly time. A caller cannot keep a receipt tool from one context and
// invoke it under another context, and a normal task cannot invoke it at all.
type syntheticReceiptGuard struct {
	actool.CoreTool
	state *SyntheticCalibrationState
}

func (g *syntheticReceiptGuard) Call(ctx context.Context, input json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	if syntheticCalibrationState(ctx) != g.state {
		return actool.Errorf("synthetic_receipt is unavailable outside a bounded synthetic calibration"), nil
	}
	return g.CoreTool.Call(ctx, input, tc)
}

func emptyJSONObject(input json.RawMessage) bool {
	trimmed := bytes.TrimSpace(input)
	if len(trimmed) == 0 {
		return true
	}
	if trimmed[0] != '{' {
		return false
	}
	var values map[string]json.RawMessage
	return json.Unmarshal(trimmed, &values) == nil && len(values) == 0
}

// syntheticBashEnv avoids configuring proxy or CA variables for a policy that
// has no Bash tool. This is defence in depth against a future agentcore feature
// accidentally treating the environment as an executable capability.
func syntheticBashEnv(synthetic bool, proxy, caCert string) []string {
	if synthetic {
		return nil
	}
	return proxyEnv(proxy, caCert)
}

func syntheticWorkerTurnCap(synthetic bool, configured int) int {
	if synthetic {
		return SyntheticCalibrationMaxTurns
	}
	return configured
}
