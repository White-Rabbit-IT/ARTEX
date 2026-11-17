package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

func hermesExecutionGuardConfig() agent.Config {
	return agent.ConfigFrom("hermes-subscription", "openai/gpt-test", "http://127.0.0.1:8645/v1", "", "")
}

func TestHermesSubscriptionAgentExecutionRequiresExplicitOptIn(t *testing.T) {
	cfg := hermesExecutionGuardConfig()

	for _, value := range []string{"", "true", "yes", "0", " 1", "1 ", "1\n"} {
		t.Setenv(HermesSubscriptionAgentExecutionEnv, value)
		if err := hermesSubscriptionAgentExecutionError(cfg); err == nil {
			t.Fatalf("%s=%q unexpectedly enabled Hermes agent execution", HermesSubscriptionAgentExecutionEnv, value)
		} else if !strings.Contains(err.Error(), HermesSubscriptionAgentExecutionEnv) {
			t.Fatalf("denial=%q does not name the required opt-in", err)
		}
	}

	t.Setenv(HermesSubscriptionAgentExecutionEnv, "1")
	if err := hermesSubscriptionAgentExecutionError(cfg); err != nil {
		t.Fatalf("explicit opt-in was rejected: %v", err)
	}
	if err := hermesSubscriptionAgentExecutionError(agent.ConfigFrom("openai", "m", "", "test-key", "")); err != nil {
		t.Fatalf("non-Hermes profile must retain existing behavior: %v", err)
	}
}

// spawn_task resolves a profile independently from HTTP task creation. It must
// enforce the calibration-only policy before it writes a child task, both for a
// model-supplied explicit profile id and for the optional parent pin.
func TestHermesSubscriptionSpawnTaskRejectsExplicitAndInheritedProfiles(t *testing.T) {
	t.Setenv(HermesSubscriptionAgentExecutionEnv, "")
	g := newGateServer(t)
	profileID, err := g.m.pg.SaveProfile(&db.LLMProfile{
		Name:   "hermes-spawn-guard-" + time.Now().UTC().Format("20060102150405.000000000"),
		Format: "hermes-subscription", Model: "openai/gpt-test", BaseURL: "http://127.0.0.1:8645/v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.m.pg.DeleteProfile(profileID) })

	call := func(t *testing.T, args map[string]any) {
		t.Helper()
		before := len(g.m.List())
		in, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		res, err := g.s.toolSpawnTask().Call(context.Background(), in, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := res.Flatten(); !strings.Contains(got, HermesSubscriptionAgentExecutionEnv) {
			t.Fatalf("spawn_task denial=%q, want explicit Hermes opt-in guidance", got)
		}
		if after := len(g.m.List()); after != before {
			t.Fatalf("blocked spawn_task changed task count: before=%d after=%d", before, after)
		}
	}

	call(t, map[string]any{
		"description": "blocked explicit Hermes child", "goal": "must not create", "llm_profile_id": profileID,
	})

	parent := g.newTask(t, db.TaskCreateOptions{LLMProfileIDs: []int64{profileID}})
	call(t, map[string]any{
		"description": "blocked inherited Hermes child", "goal": "must not create", "parent_ref": parent.ID,
	})
}

func TestHermesSubscriptionGuardBlocksCachedTaskAndChatPaths(t *testing.T) {
	t.Setenv(HermesSubscriptionAgentExecutionEnv, "")
	cfg := hermesExecutionGuardConfig()
	provider := &scriptedLLMProvider{}
	profileID := int64(17)
	cachedChat := &agent.ChatAgent{}
	s := &Server{
		m:             &Manager{},
		llmOn:         true,
		llmCfg:        cfg,
		llmProv:       provider,
		llmDirect:     provider,
		chatAgent:     &agent.ChatAgent{},
		provByProfile: map[int64]*provEntry{profileID: {prov: provider, cfg: cfg}},
		profChatAgents: map[int64]*agent.ChatAgent{
			profileID: cachedChat,
		},
	}

	if _, _, ok := s.globalProvider(); ok {
		t.Fatal("blocked Hermes global provider was available to a task")
	}
	if _, _, ok := s.providerForProfile(profileID); ok {
		t.Fatal("blocked Hermes profile cache bypassed the execution guard")
	}
	if got := s.chatAgentForProfile(profileID); got != nil {
		t.Fatalf("blocked Hermes profile ChatAgent=%p, want nil", got)
	}
	if got := s.resolveChatAgent(&db.Conversation{LLMProfileID: &profileID}); got != nil {
		t.Fatalf("blocked Hermes conversation fell back to ChatAgent %p", got)
	}
	if reason := s.chatUnavailableReasonFor(&db.Conversation{LLMProfileID: &profileID}); !strings.Contains(reason, HermesSubscriptionAgentExecutionEnv) {
		t.Fatalf("chat denial reason=%q, want explicit opt-in guidance", reason)
	}

	task := &Task{ID: "7"}
	task.setLLMState(&profileID, &profileID, []int64{profileID}, 1, "", "")
	if s.taskRuntimeAvailable(task, "worker") {
		t.Fatal("blocked Hermes task profile was runnable")
	}

	t.Setenv(HermesSubscriptionAgentExecutionEnv, "1")
	if _, _, ok := s.globalProvider(); !ok {
		t.Fatal("explicit opt-in did not restore the global provider")
	}
	if _, _, ok := s.providerForProfile(profileID); !ok {
		t.Fatal("explicit opt-in did not restore the cached profile provider")
	}
	if got := s.chatAgentForProfile(profileID); got != cachedChat {
		t.Fatalf("explicit opt-in ChatAgent=%p, want cached %p", got, cachedChat)
	}
	if !s.taskRuntimeAvailable(task, "worker") {
		t.Fatal("explicit opt-in did not restore task admission")
	}
}

func TestApplyLLMDisablesHermesAgentRuntimeButConnectionTestStillWorks(t *testing.T) {
	t.Setenv(HermesSubscriptionAgentExecutionEnv, "")
	cfg := hermesExecutionGuardConfig()
	s := &Server{
		llmOn:     true,
		llmCfg:    agent.ConfigFrom("openai", "old", "", "test-key", ""),
		llmProv:   &scriptedLLMProvider{},
		llmDirect: &scriptedLLMProvider{},
		chatAgent: &agent.ChatAgent{},
	}
	if err := s.applyLLM(cfg); err == nil || !strings.Contains(err.Error(), HermesSubscriptionAgentExecutionEnv) {
		t.Fatalf("applyLLM error=%v, want explicit Hermes execution denial", err)
	}
	if s.llmOn || s.llmProv != nil || s.llmDirect != nil || s.chatAgent != nil {
		t.Fatalf("blocked Hermes activation left a runnable global state: on=%v prov=%v direct=%v chat=%v", s.llmOn, s.llmProv != nil, s.llmDirect != nil, s.chatAgent != nil)
	}

	var calls atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path=%q, want /v1/chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"calibration","object":"chat.completion","model":"openai/gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"ARTEX_HERMES_CALIBRATION_OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	t.Cleanup(fake.Close)

	payload, err := json.Marshal(map[string]any{
		"provider":  "hermes-subscription",
		"model":     "openai/gpt-test",
		"base_url":  fake.URL,
		"streaming": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/llm/test", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	(&Server{}).testLLM(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("connection test status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result struct {
		OK    bool   `json:"ok"`
		Reply string `json:"reply"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || !strings.Contains(result.Reply, "ARTEX_HERMES_CALIBRATION_OK") || calls.Load() != 1 {
		t.Fatalf("connection test result=%+v calls=%d", result, calls.Load())
	}
}

func TestHermesSubscriptionConnectionTestRejectsOutboundProxy(t *testing.T) {
	var targetHits, proxyHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"calibration","object":"chat.completion","model":"openai/gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"ARTEX_HERMES_CALIBRATION_OK"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(target.Close)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)

	payload, err := json.Marshal(map[string]any{
		"provider": "hermes-subscription", "model": "openai/gpt-test", "base_url": target.URL,
		"proxy": proxy.URL, "streaming": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/llm/test", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	(&Server{}).testLLM(rec, req)
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || result.OK || !strings.Contains(result.Error, "does not permit") {
		t.Fatalf("proxy-bearing Hermes test status=%d result=%+v", rec.Code, result)
	}
	if targetHits.Load() != 0 || proxyHits.Load() != 0 {
		t.Fatalf("Hermes proxy rejection made network calls: target=%d proxy=%d", targetHits.Load(), proxyHits.Load())
	}
}

func TestGenericProviderAtDocumentedHermesRouteIsRejectedBeforeAnyRequest(t *testing.T) {
	for _, route := range []struct {
		name string
		url  string
	}{
		{name: "legacy port", url: "http://host.docker.internal:8645/v1"},
		{name: "literal bridge port", url: "http://172.18.0.1:864/v1"},
	} {
		t.Run(route.name, func(t *testing.T) {
			for _, endpoint := range []struct {
				name string
				h    http.HandlerFunc
				path string
			}{
				{name: "connection test", h: (&Server{}).testLLM, path: "/api/llm/test"},
				{name: "model listing", h: (&Server{}).pgListModels, path: "/api/llm/models"},
			} {
				t.Run(endpoint.name, func(t *testing.T) {
					payload, err := json.Marshal(map[string]any{
						"provider": "openai", "model": "gpt-test", "base_url": route.url,
						"api_key": "cloud-key-that-must-not-leave",
					})
					if err != nil {
						t.Fatal(err)
					}
					req := httptest.NewRequest(http.MethodPost, endpoint.path, bytes.NewReader(payload))
					rec := httptest.NewRecorder()
					endpoint.h(rec, req)
					var result struct {
						OK    bool   `json:"ok"`
						Error string `json:"error"`
					}
					if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					if rec.Code != http.StatusOK || result.OK || !strings.Contains(result.Error, "hermes-subscription") {
						t.Fatalf("generic endpoint response status=%d result=%+v", rec.Code, result)
					}
				})
			}
		})
	}
}

func TestGenericProviderAtReservedHermesPortCannotBeSavedOrInstalled(t *testing.T) {
	for _, route := range []struct {
		name string
		url  string
	}{
		{name: "legacy port", url: "http://host.docker.internal:8645/v1/chat/completions#fragment"},
		{name: "literal bridge port", url: "http://172.18.0.1:864/v1/chat/completions#fragment"},
	} {
		t.Run(route.name, func(t *testing.T) {
			g := newGateServer(t)
			payload, err := json.Marshal(map[string]any{
				"name": "must-not-save-generic-hermes-port", "format": "openai", "model": "gpt-test",
				"base_url": route.url, "api_key": "cloud-key-that-must-not-leave", "streaming": false,
			})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/llm/profiles", bytes.NewReader(payload))
			rec := httptest.NewRecorder()
			g.s.pgSaveProfile(rec, req)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "hermes-subscription") {
				t.Fatalf("generic reserved-port profile save status=%d body=%s", rec.Code, rec.Body.String())
			}

			// The legacy global configuration endpoint must make the same decision before
			// it persists/replaces a runnable provider configuration.
			payload, err = json.Marshal(map[string]any{
				"provider": "openai", "model": "gpt-test", "base_url": route.url, "api_key": "cloud-key-that-must-not-leave",
			})
			if err != nil {
				t.Fatal(err)
			}
			req = httptest.NewRequest(http.MethodPost, "/api/llm", bytes.NewReader(payload))
			rec = httptest.NewRecorder()
			g.s.setLLM(rec, req)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "hermes-subscription") {
				t.Fatalf("generic reserved-port legacy install status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHermesSubscriptionModelListingRejectsOutboundProxy(t *testing.T) {
	var targetHits, proxyHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"id":"openai/gpt-test"}]}`))
	}))
	t.Cleanup(target.Close)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)

	payload, err := json.Marshal(map[string]any{
		"provider": "hermes-subscription", "base_url": target.URL, "proxy": proxy.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/llm/models", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	(&Server{}).pgListModels(rec, req)
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || result.OK || !strings.Contains(result.Error, "does not permit") {
		t.Fatalf("proxy-bearing Hermes model listing status=%d result=%+v", rec.Code, result)
	}
	if targetHits.Load() != 0 || proxyHits.Load() != 0 {
		t.Fatalf("Hermes model listing proxy rejection made network calls: target=%d proxy=%d", targetHits.Load(), proxyHits.Load())
	}
}

func TestHermesSubscriptionModelListingNeverFollowsRedirects(t *testing.T) {
	var originHits, sinkHits atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sinkHits.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"id":"outside-model"}]}`))
	}))
	t.Cleanup(sink.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHits.Add(1)
		http.Redirect(w, r, sink.URL+"/models", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(origin.Close)

	payload, err := json.Marshal(map[string]any{
		"provider": "hermes-subscription", "base_url": origin.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/llm/models", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	(&Server{}).pgListModels(rec, req)
	var result struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || result.OK {
		t.Fatalf("redirecting Hermes model-list response status=%d result=%+v", rec.Code, result)
	}
	if originHits.Load() != 1 || sinkHits.Load() != 0 {
		t.Fatalf("model-list redirect origin=%d sink=%d, want 1/0", originHits.Load(), sinkHits.Load())
	}
}

func TestHermesSubscriptionGuardBlocksSideQuestionInference(t *testing.T) {
	t.Setenv(HermesSubscriptionAgentExecutionEnv, "")
	cfg := hermesExecutionGuardConfig()
	provider := &scriptedLLMProvider{}
	model := sideModel(cfg, 0, "")
	s := &Server{llmOn: true, llmCfg: cfg, llmDirect: provider}

	if got, err := s.sideProvider(model); err == nil || got != nil || !strings.Contains(err.Error(), HermesSubscriptionAgentExecutionEnv) {
		t.Fatalf("blocked side provider=(%T, %v), want explicit Hermes execution denial", got, err)
	}

	t.Setenv(HermesSubscriptionAgentExecutionEnv, "1")
	if got, err := s.sideProvider(model); err != nil || got != provider {
		t.Fatalf("explicit opt-in side provider=(%T, %v), want cached provider", got, err)
	}
}

// A calibration-only profile must not be able to displace an already working
// active profile through either UI mutation route. The runtime guard catches a
// resulting configuration too, but that is too late: the operator would have
// lost the active row and ARTEX would be left idle. These handler checks keep
// the database selection and the live runtime coherent.
func TestHermesSubscriptionBlockedProfileCannotReplaceActiveProfile(t *testing.T) {
	skipWithoutDSN(t)
	t.Setenv(HermesSubscriptionAgentExecutionEnv, "")

	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if m.pg == nil {
		t.Skip("no postgres for this run")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := New(ctx, m, t.TempDir(), t.TempDir(), t.TempDir())

	prior, err := m.pg.ActiveProfile()
	if err != nil {
		t.Fatal(err)
	}
	nameSuffix := time.Now().UTC().Format("20060102150405.000000000")
	normalID, err := m.pg.SaveProfile(&db.LLMProfile{
		Name: "hermes-guard-normal-" + nameSuffix, Format: "openai", Model: "gpt-test",
		BaseURL: "http://127.0.0.1:9/v1", APIKey: "local-test-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	hermesID, err := m.pg.SaveProfile(&db.LLMProfile{
		Name: "hermes-guard-calibration-" + nameSuffix, Format: "hermes-subscription", Model: "openai/gpt-test",
		BaseURL: "http://127.0.0.1:8645/v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if prior != nil {
			_ = m.pg.SetActiveProfile(prior.ID)
		} else {
			_, _ = m.pg.Exec(`UPDATE llm_profiles SET is_default=false WHERE id=$1 OR id=$2`, normalID, hermesID)
		}
		_ = m.pg.DeleteProfile(hermesID)
		_ = m.pg.DeleteProfile(normalID)
	})
	if err := m.pg.SetActiveProfile(normalID); err != nil {
		t.Fatal(err)
	}
	normalCfg, ok := s.loadProfileConfig(normalID)
	if !ok {
		t.Fatal("normal profile is unexpectedly unusable")
	}
	if err := s.applyLLM(normalCfg); err != nil {
		t.Fatalf("apply normal profile: %v", err)
	}

	post := func(handler http.HandlerFunc, body any) *httptest.ResponseRecorder {
		t.Helper()
		blob, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/llm/profiles", bytes.NewReader(blob))
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}
	assertNormalStillActive := func(stage string) {
		t.Helper()
		active, err := m.pg.ActiveProfile()
		if err != nil || active == nil || active.ID != normalID {
			t.Fatalf("%s active profile=%+v err=%v, want normal #%d", stage, active, err, normalID)
		}
		s.cfgMu.Lock()
		on, cfg := s.llmOn, s.llmCfg
		s.cfgMu.Unlock()
		if !on || cfg.IsHermesSubscription() {
			t.Fatalf("%s live runtime changed to blocked Hermes: on=%v cfg=%+v", stage, on, cfg)
		}
	}

	// Direct activation must reject before SetActiveProfile changes the global row.
	activation := post(s.pgActivateProfile, map[string]any{"id": hermesID})
	if activation.Code != http.StatusConflict || !strings.Contains(activation.Body.String(), HermesSubscriptionAgentExecutionEnv) {
		t.Fatalf("blocked activation status=%d body=%s", activation.Code, activation.Body.String())
	}
	assertNormalStillActive("blocked activation")

	// The profile save endpoint must also reject converting the active row into
	// a calibration-only Hermes profile before it updates PostgreSQL.
	edit := post(s.pgSaveProfile, map[string]any{
		"id": normalID, "name": fmt.Sprintf("hermes-guard-normal-%s", nameSuffix),
		"format": "hermes-subscription", "model": "openai/gpt-test",
		"base_url": "http://127.0.0.1:8645/v1", "streaming": false,
	})
	if edit.Code != http.StatusConflict || !strings.Contains(edit.Body.String(), HermesSubscriptionAgentExecutionEnv) {
		t.Fatalf("blocked active-profile edit status=%d body=%s", edit.Code, edit.Body.String())
	}
	stored, err := m.pg.ProfileByID(normalID)
	if err != nil || stored == nil || stored.Format != "openai" {
		t.Fatalf("blocked edit persisted a profile change: stored=%+v err=%v", stored, err)
	}
	assertNormalStillActive("blocked active-profile edit")

	// The explicit opt-in permits the normal profile activation operation; this
	// proves the denial is a policy gate rather than a permanently dead profile.
	t.Setenv(HermesSubscriptionAgentExecutionEnv, "1")
	allowed := post(s.pgActivateProfile, map[string]any{"id": hermesID})
	if allowed.Code != http.StatusOK {
		t.Fatalf("opted-in activation status=%d body=%s", allowed.Code, allowed.Body.String())
	}
	active, err := m.pg.ActiveProfile()
	if err != nil || active == nil || active.ID != hermesID {
		t.Fatalf("opted-in active profile=%+v err=%v, want Hermes #%d", active, err, hermesID)
	}
}
