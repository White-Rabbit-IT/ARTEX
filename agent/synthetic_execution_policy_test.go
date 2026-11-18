package agent

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

func policyProbe(name string, calls *atomic.Uint32) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name:        name,
		Description: "test-only handler",
		Schema:      map[string]any{"type": "object"},
		Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
			calls.Add(1)
			return actool.Text("unexpected handler execution"), nil
		},
	})
}

func toolNames(tools []actool.CoreTool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name())
	}
	return names
}

func TestSyntheticCalibrationFinaliseToolsDropsEverySuppliedTool(t *testing.T) {
	var calls atomic.Uint32
	input := []actool.CoreTool{
		policyProbe("Bash", &calls),
		policyProbe("Read", &calls),
		policyProbe("Write", &calls),
		policyProbe("Edit", &calls),
		policyProbe("LS", &calls),
		policyProbe("Sleep", &calls),
		policyProbe("WebFetch", &calls),
		policyProbe("Skill", &calls),
		policyProbe("mcp__test__network", &calls),
		policyProbe("custom_command", &calls),
	}
	ctx, state := NewSyntheticCalibrationContext(t.Context())
	got, def := FinaliseTools(ctx, "worker", input, DeferredInfo{
		Deferred:    []string{"mcp__test__network"},
		GlobalNames: []string{"mcp__test__network"},
		Unlock:      actool.NewUnlockSet("mcp__test__network"),
		UnlockSkill: func(string) {},
	})

	if names := toolNames(got); len(names) != 1 || names[0] != SyntheticReceiptToolName {
		t.Fatalf("synthetic worker tools = %v, want only %q", names, SyntheticReceiptToolName)
	}
	if len(def.Deferred) != 0 || len(def.GlobalNames) != 0 || def.Unlock != nil || def.UnlockSkill != nil || def.FindingGuidance != "" {
		t.Fatalf("synthetic deferred metadata = %#v, want empty", def)
	}
	res, err := got[0].Call(ctx, json.RawMessage(`{}`), nil)
	if err != nil || res.IsError {
		t.Fatalf("synthetic receipt failed: result=%+v err=%v", res, err)
	}
	if !strings.Contains(res.Flatten(), "no target") {
		t.Fatalf("receipt should be static safety text, got %q", res.Flatten())
	}
	if calls.Load() != 0 {
		t.Fatalf("discarded handler calls=%d, want 0", calls.Load())
	}
	if !state.ReceiptCalled() || state.ReceiptCallCount() != 1 {
		t.Fatalf("receipt state = called=%v count=%d, want true/1", state.ReceiptCalled(), state.ReceiptCallCount())
	}
}

func TestSyntheticCalibrationFinaliseToolsRejectsReceiptReuseAndArguments(t *testing.T) {
	ctx, state := NewSyntheticCalibrationContext(t.Context())
	tools, _ := FinaliseTools(ctx, "worker", nil, DeferredInfo{})
	if len(tools) != 1 {
		t.Fatalf("tools=%d, want one", len(tools))
	}
	res, err := tools[0].Call(ctx, json.RawMessage(`{"untrusted":"payload"}`), nil)
	if err != nil || !res.IsError || !strings.Contains(res.Flatten(), "empty JSON object") {
		t.Fatalf("non-empty receipt input = result=%+v err=%v", res, err)
	}
	if state.ReceiptCalled() {
		t.Fatal("invalid receipt input must not record a receipt")
	}

	otherCtx, _ := NewSyntheticCalibrationContext(t.Context())
	res, err = tools[0].Call(otherCtx, json.RawMessage(`{}`), nil)
	if err != nil || !res.IsError || !strings.Contains(res.Flatten(), "unavailable outside") {
		t.Fatalf("cross-context receipt reuse = result=%+v err=%v", res, err)
	}
	if state.ReceiptCalled() {
		t.Fatal("cross-context receipt use must not record a receipt")
	}

	res, err = tools[0].Call(ctx, json.RawMessage(`{}`), nil)
	if err != nil || res.IsError || !state.ReceiptCalled() {
		t.Fatalf("first valid receipt = result=%+v err=%v called=%v", res, err, state.ReceiptCalled())
	}
	res, err = tools[0].Call(ctx, json.RawMessage(`{}`), nil)
	if err != nil || !res.IsError || !strings.Contains(res.Flatten(), "already recorded") {
		t.Fatalf("second receipt = result=%+v err=%v", res, err)
	}
	if state.ReceiptCallCount() != 1 {
		t.Fatalf("receipt count=%d, want 1", state.ReceiptCallCount())
	}
}

func TestSyntheticCalibrationAugmentSkipsVisibilityAndCatalogHooks(t *testing.T) {
	oldAugment, oldResolve := ToolAugment(), ToolResolve()
	t.Cleanup(func() {
		SetToolAugment(oldAugment)
		SetToolResolve(oldResolve)
	})
	var augmentCalls, resolveCalls atomic.Uint32
	SetToolAugment(func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func()) {
		augmentCalls.Add(1)
		return []actool.CoreTool{policyProbe("mcp__test__late", &augmentCalls)}, DeferredInfo{Deferred: []string{"mcp__test__late"}}, func() {}
	})
	SetToolResolve(func(_ context.Context, _ string, tools []actool.CoreTool) []actool.CoreTool {
		resolveCalls.Add(1)
		return tools
	})

	ctx, _ := NewSyntheticCalibrationContext(t.Context())
	tools, def, cleanup := AugmentTools(ctx, "worker", []actool.CoreTool{policyProbe("Bash", &resolveCalls)})
	cleanup()
	if augmentCalls.Load() != 0 || resolveCalls.Load() != 0 {
		t.Fatalf("synthetic assembly called augment=%d resolve=%d, want 0/0", augmentCalls.Load(), resolveCalls.Load())
	}
	if names := toolNames(tools); len(names) != 1 || names[0] != SyntheticReceiptToolName {
		t.Fatalf("synthetic AugmentTools names=%v, want only receipt", names)
	}
	if len(def.Deferred) != 0 || def.Unlock != nil {
		t.Fatalf("synthetic AugmentTools deferred=%#v, want empty", def)
	}
}

func TestSyntheticCalibrationFinaliseToolsLeavesPlannerAndMainToolless(t *testing.T) {
	ctx, _ := NewSyntheticCalibrationContext(t.Context())
	for _, agentKey := range []string{"planner", "mainagent"} {
		tools, def := FinaliseTools(ctx, agentKey, []actool.CoreTool{policyProbe("WebFetch", &atomic.Uint32{})}, DeferredInfo{Deferred: []string{"mcp__test__network"}})
		if tools == nil {
			t.Fatalf("%s synthetic tools must be a non-nil empty list; agentcore treats nil as DefaultTools", agentKey)
		}
		if len(tools) != 0 {
			t.Fatalf("%s synthetic tools=%v, want none", agentKey, toolNames(tools))
		}
		if len(def.Deferred) != 0 || def.Unlock != nil || def.UnlockSkill != nil {
			t.Fatalf("%s synthetic deferred=%#v, want empty", agentKey, def)
		}
	}
}

func TestFinaliseToolsPreservesOrdinaryRuns(t *testing.T) {
	var calls atomic.Uint32
	base := policyProbe("Bash", &calls)
	def := DeferredInfo{Deferred: []string{"mcp__test__deferred"}, GlobalNames: []string{"mcp__test__deferred"}}
	tools, gotDef := FinaliseTools(t.Context(), "worker", []actool.CoreTool{base}, def)
	if names := toolNames(tools); len(names) != 1 || names[0] != "Bash" {
		t.Fatalf("ordinary tools=%v, want Bash unchanged", names)
	}
	if len(gotDef.Deferred) != 1 || gotDef.Deferred[0] != def.Deferred[0] || len(gotDef.GlobalNames) != 1 || gotDef.GlobalNames[0] != def.GlobalNames[0] {
		t.Fatalf("ordinary deferred metadata changed: %#v", gotDef)
	}
	if _, err := tools[0].Call(t.Context(), json.RawMessage(`{}`), nil); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("ordinary handler calls=%d, want 1", calls.Load())
	}
}

func TestSyntheticWorkerTurnCapIsFiniteAndDoesNotChangeOrdinaryRuns(t *testing.T) {
	if got := syntheticWorkerTurnCap(true, 0); got != SyntheticCalibrationMaxTurns {
		t.Fatalf("synthetic cap with unlimited profile = %d, want %d", got, SyntheticCalibrationMaxTurns)
	}
	if got := syntheticWorkerTurnCap(true, 99); got != SyntheticCalibrationMaxTurns {
		t.Fatalf("synthetic cap with broad profile = %d, want %d", got, SyntheticCalibrationMaxTurns)
	}
	if got := syntheticWorkerTurnCap(false, 0); got != 0 {
		t.Fatalf("ordinary unlimited profile cap = %d, want 0", got)
	}
	if got := syntheticWorkerTurnCap(false, 7); got != 7 {
		t.Fatalf("ordinary configured cap = %d, want 7", got)
	}
}

func TestSyntheticCalibrationProviderCallReservationsAreContextPrivateAndBounded(t *testing.T) {
	if ReserveSyntheticCalibrationModelCall(t.Context()) {
		t.Fatal("ordinary context must not reserve a synthetic provider call")
	}
	ctx, state := NewSyntheticCalibrationContext(t.Context())
	for call := uint32(1); call <= SyntheticCalibrationMaxModelCalls; call++ {
		if !ReserveSyntheticCalibrationModelCall(ctx) {
			t.Fatalf("reservation %d unexpectedly denied", call)
		}
	}
	if ReserveSyntheticCalibrationModelCall(ctx) {
		t.Fatalf("reservation beyond fixed limit %d unexpectedly succeeded", SyntheticCalibrationMaxModelCalls)
	}
	if state.ModelCallCount() != SyntheticCalibrationMaxModelCalls {
		t.Fatalf("reserved model calls=%d, want %d", state.ModelCallCount(), SyntheticCalibrationMaxModelCalls)
	}
}

func TestSyntheticCalibrationPlannerFailsClosedBeforeAnyTaskDataOrProviderCall(t *testing.T) {
	provider := &syntheticRequestProvider{}
	planner := NewPlanner(provider, "PRIVATE_MODEL_SENTINEL", "/private/workdir-sentinel", nil, 9000, 0)
	ctx, _ := NewSyntheticCalibrationContext(t.Context())

	met, reason, err := planner.Plan(ctx, 987654, nil, nil, "PRIVATE_GOAL_SENTINEL", nil, nil, nil)
	if err != nil {
		t.Fatalf("synthetic planner rejection: %v", err)
	}
	if met {
		t.Fatal("synthetic planner must not report a goal met")
	}
	if reason != syntheticPlannerUnavailableReason {
		t.Fatalf("synthetic planner reason=%q, want fixed %q", reason, syntheticPlannerUnavailableReason)
	}
	if got := provider.snapshot(); len(got) != 0 {
		t.Fatalf("synthetic planner made %d provider calls, want 0", len(got))
	}
}

func TestSyntheticCalibrationMainAgentFailsClosedBeforeAnyTaskDataOrProviderCall(t *testing.T) {
	provider := &syntheticRequestProvider{}
	main := NewMainAgent(provider, "PRIVATE_MODEL_SENTINEL", "/private/workdir-sentinel", nil, 9000, 0)
	ctx, _ := NewSyntheticCalibrationContext(t.Context())

	reply, err := main.Chat(ctx, 987654, 3, nil, nil, "PRIVATE_GOAL_SENTINEL", "PRIVATE_MESSAGE_SENTINEL", nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("synthetic main-agent rejection: %v", err)
	}
	if reply != syntheticMainAgentUnavailableReply {
		t.Fatalf("synthetic main-agent reply=%q, want fixed %q", reply, syntheticMainAgentUnavailableReply)
	}
	if got := provider.snapshot(); len(got) != 0 {
		t.Fatalf("synthetic main-agent made %d provider calls, want 0", len(got))
	}
}

// syntheticRequestProvider records exactly the provider-visible request packet
// for the bounded worker path. It emits one receipt tool call, then a fixed
// completion, making the test independent of a network/provider account.
type syntheticRequestProvider struct {
	mu            sync.Mutex
	requests      []llm.CompletionRequest
	contextValues []any
	turn          atomic.Uint32
}

type syntheticPrivateContextKey struct{}

func (p *syntheticRequestProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.contextValues = append(p.contextValues, ctx.Value(syntheticPrivateContextKey{}))
	p.mu.Unlock()
	turn := p.turn.Add(1)
	return func(yield func(llm.StreamEvent, error) bool) {
		if turn == 1 {
			events := []llm.StreamEvent{
				{Type: llm.SEToolUseStart, ToolID: "receipt-1", ToolName: SyntheticReceiptToolName},
				{Type: llm.SEToolInputJSON, Text: `{}`},
				{Type: llm.SEMessageDelta, StopReason: "tool_use"},
			}
			for _, event := range events {
				if !yield(event, nil) {
					return
				}
			}
			return
		}
		events := []llm.StreamEvent{
			{Type: llm.SETextDelta, Text: "Synthetic calibration complete."},
			{Type: llm.SEMessageDelta, StopReason: "end_turn"},
		}
		for _, event := range events {
			if !yield(event, nil) {
				return
			}
		}
	}
}

func (p *syntheticRequestProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	acc := llm.NewAccumulator()
	for event, err := range p.Stream(ctx, req) {
		if err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		acc.Add(event)
	}
	return acc.Message(), acc.StopReason, acc.Usage, nil
}

func (p *syntheticRequestProvider) snapshot() []llm.CompletionRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]llm.CompletionRequest(nil), p.requests...)
}

func (p *syntheticRequestProvider) snapshotContextValues() []any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]any(nil), p.contextValues...)
}

func TestSyntheticWorkerSendsOnlyFixedProviderPacket(t *testing.T) {
	provider := &syntheticRequestProvider{}
	worker := NewWorker(provider, "model-private-sentinel", "/private/workdir-sentinel", nil, 9000, 0)
	worker.SetProxy("http://private-proxy-sentinel", "/private/ca-sentinel")
	worker.SetLessonsBlock("PRIVATE_LESSONS_SENTINEL")
	parent := context.WithValue(t.Context(), syntheticPrivateContextKey{}, "PRIVATE_CONTEXT_SENTINEL")
	ctx, state := NewSyntheticCalibrationContext(parent)
	intent := &db.Node{ID: 112233, Kind: "intent", Payload: json.RawMessage(`{"private":"PRIVATE_INTENT_PAYLOAD_SENTINEL"}`)}

	_, writes, err := worker.ExecuteWithMessage(ctx, "PRIVATE_WORKER_SENTINEL", 987654, nil, nil, intent, nil, nil, nil, nil, "PRIVATE_REQUEST_ID_SENTINEL", "PRIVATE_USER_MESSAGE_SENTINEL")
	if err != nil {
		t.Fatalf("synthetic worker execution: %v", err)
	}
	if writes.Total() != 0 {
		t.Fatalf("synthetic worker writes=%s, want no graph writes", writes)
	}
	if !state.ReceiptCalled() || state.ReceiptCallCount() != 1 {
		t.Fatalf("receipt state = called=%v count=%d, want true/1", state.ReceiptCalled(), state.ReceiptCallCount())
	}

	requests := provider.snapshot()
	if len(requests) != 2 {
		t.Fatalf("provider request count=%d, want tool call plus completion", len(requests))
	}
	for index, request := range requests {
		raw, marshalErr := json.Marshal(request)
		if marshalErr != nil {
			t.Fatalf("marshal provider request %d: %v", index, marshalErr)
		}
		packet := string(raw)
		for _, forbidden := range []string{
			"model-private-sentinel",
			"/private/workdir-sentinel",
			"private-proxy-sentinel",
			"/private/ca-sentinel",
			"PRIVATE_LESSONS_SENTINEL",
			"PRIVATE_WORKER_SENTINEL",
			"PRIVATE_INTENT_PAYLOAD_SENTINEL",
			"PRIVATE_REQUEST_ID_SENTINEL",
			"PRIVATE_USER_MESSAGE_SENTINEL",
			"987654",
			"112233",
		} {
			if strings.Contains(packet, forbidden) {
				t.Fatalf("provider request %d leaked forbidden sentinel %q: %s", index, forbidden, packet)
			}
		}
		if len(request.Tools) != 1 || request.Tools[0].Name != SyntheticReceiptToolName {
			t.Fatalf("provider request %d tools=%+v, want only %q", index, request.Tools, SyntheticReceiptToolName)
		}
		if request.MaxTokens != syntheticCalibrationMaxTokens {
			t.Fatalf("provider request %d max_tokens=%d, want fixed %d", index, request.MaxTokens, syntheticCalibrationMaxTokens)
		}
	}
	if !strings.Contains(strings.Join(requests[0].System, "\n"), "synthetic calibration version 1") {
		t.Fatalf("first system prompt does not contain the fixed calibration prompt: %q", requests[0].System)
	}
	if strings.Contains(strings.Join(requests[0].System, "\n"), "PRIVATE_") {
		t.Fatalf("first system prompt contains a private sentinel: %q", requests[0].System)
	}
	for index, value := range provider.snapshotContextValues() {
		if value != nil {
			t.Fatalf("provider request %d retained caller context value %q", index, value)
		}
	}
}
