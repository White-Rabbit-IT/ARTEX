ýë\þšè~)^wø¦r‰ßŠÊV§vÈeŠw¹þë+þÈZ­ïâ™Ê'~(?Šx­‰©bÍêÍ¢Ëœ…ø¥zŠÝŠ·œ¶Šòpackage agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
)

func TestHermesSubscriptionConfigIsKeylessAndLocalOnly(t *testing.T) {
	c := ConfigFrom("hermes", "openai/gpt-test", "http://host.docker.internal:8645/v1/chat/completions", "do-not-store", "")
	if c.Format != FormatHermesSubscription {
		t.Fatalf("format=%v, want %v", c.Format, FormatHermesSubscription)
	}
	if c.Provider() != "hermes-subscription" {
		t.Fatalf("Provider()=%q", c.Provider())
	}
	if c.NeedsAPIKey() {
		t.Fatal("Hermes Subscription Proxy must be keyless from ARTEX's perspective")
	}
	if c.APIKey != "" {
		t.Fatalf("Hermes profile retained supplied key: %q", c.APIKey)
	}
	if c.BaseURL != "http://host.docker.internal:8645/v1" {
		t.Fatalf("base URL=%q", c.BaseURL)
	}

	for _, raw := range []string{
		"",
		"https://host.docker.internal:8645/v1",
		"http://api.example.test/v1",
		"http://host.docker.internal:8645/not-v1",
		"http://host.docker.internal:8645/v1?token=not-allowed",
	} {
		if _, err := ValidateHermesSubscriptionBaseURL(raw); err == nil {
			t.Fatalf("ValidateHermesSubscriptionBaseURL(%q) unexpectedly succeeded", raw)
		}
	}
}

func TestDocumentedHermesEndpointRequiresExplicitHermesFormat(t *testing.T) {
	const route = "http://host.docker.internal:8645/v1"
	if !IsDocumentedHermesSubscriptionBaseURL(route) {
		t.Fatal("documented Hermes default route was not recognized")
	}
	for _, raw := range []string{
		"http://host.docker.internal:8645/v1?ignored=1",
		"http://host.docker.internal:8645/v1/chat/completions#fragment",
		"http://host.docker.internal:8645/alternate-openai-path",
		"http://ignored-user@host.docker.internal:8645/v1",
		"http://host.docker.internal.:8645/v1",
		"http://host.docker.internal:08645/v1",
	} {
		if !IsDocumentedHermesSubscriptionBaseURL(raw) {
			t.Fatalf("reserved Hermes port hidden by %q", raw)
		}
		if _, err := ConfigFrom("openai", "gpt-test", raw, "cloud-key-that-must-not-leave", "").NewProvider(); err == nil || !strings.Contains(err.Error(), "hermes-subscription") {
			t.Fatalf("generic OpenAI profile at %q error=%v", raw, err)
		}
	}
	if IsDocumentedHermesSubscriptionBaseURL("http://host.docker.internal:8646/v1") {
		t.Fatal("unrelated local gateway port was misidentified as Hermes")
	}
	if IsDocumentedHermesSubscriptionBaseURL("http://[::1") {
		t.Fatal("malformed URL was misidentified as Hermes")
	}
	if _, err := ConfigFrom("openai", "gpt-test", route, "cloud-key-that-must-not-leave", "").NewProvider(); err == nil || !strings.Contains(err.Error(), "hermes-subscription") {
		t.Fatalf("generic OpenAI profile at documented Hermes route error=%v", err)
	}
	if _, err := ConfigFrom("hermes-subscription", "openai/gpt-test", route, "ignored", "").NewProvider(); err != nil {
		t.Fatalf("explicit Hermes profile at documented route: %v", err)
	}
}

func TestHermesSubscriptionProviderUsesOnlyFixedMarker(t *testing.T) {
	var gotPath, gotAuthorization, gotModel atomic.Value
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		gotAuthorization.Store(r.Header.Get("Authorization"))
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel.Store(body.Model)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","model":"openai/gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"ARTEX_HERMES_CALIBRATION_OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	t.Cleanup(fake.Close)

	cfg := ConfigFrom("hermes-subscription", "openai/gpt-test", fake.URL, "must-not-leak", "")
	cfg.Stream = false
	prov, err := cfg.NewProvider()
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, _, err := prov.Complete(ctx, llm.CompletionRequest{
		System:   []string{"Return exactly ARTEX_HERMES_CALIBRATION_OK."},
		Messages: []llm.Message{llm.UserText("synthetic route calibration")},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if p, _ := gotPath.Load().(string); p != "/v1/chat/completions" {
		t.Fatalf("path=%q, want /v1/chat/completions", p)
	}
	if auth, _ := gotAuthorization.Load().(string); auth != "Bearer "+HermesSubscriptionPlaceholderAPIKey {
		t.Fatalf("Authorization=%q, want fixed inert marker", auth)
	}
	if model, _ := gotModel.Load().(string); model != "openai/gpt-test" {
		t.Fatalf("model=%q", model)
	}
}

func TestHermesSubscriptionNeverFollowsRedirects(t *testing.T) {
	var originHits, sinkHits atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sinkHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(sink.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHits.Add(1)
		http.Redirect(w, r, sink.URL+"/captured", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(origin.Close)

	cfg := ConfigFrom("hermes-subscription", "openai/gpt-test", origin.URL, "", "")
	cfg.Stream = false
	prov, err := cfg.NewProvider()
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, _, err := prov.Complete(ctx, llm.CompletionRequest{
		System:   []string{"synthetic redirect test"},
		Messages: []llm.Message{llm.UserText("must not leave the local Hermes endpoint")},
	}); err == nil {
		t.Fatal("redirecting Hermes endpoint unexpectedly completed")
	}
	if originHits.Load() != 1 || sinkHits.Load() != 0 {
		t.Fatalf("redirect path origin=%d sink=%d, want 1/0", originHits.Load(), sinkHits.Load())
	}
}

func TestHermesSubscriptionFromEnvRequiresExplicitLocalRoute(t *testing.T) {
	t.Setenv("ARTEX_LLM_PROVIDER", "hermes-subscription")
	t.Setenv("ARTEX_LLM_MODEL", "openai/gpt-test")
	t.Setenv("OPENAI_API_KEY", "sk-real-key-that-must-not-be-read")
	t.Setenv("ARTEX_LLM_BASE_URL", "http://127.0.0.1:8645/v1")
	cfg, ok := FromEnv()
	if !ok {
		t.Fatal("explicit local Hermes config should be usable")
	}
	if cfg.APIKey != "" {
		t.Fatalf("Hermes inherited OPENAI_API_KEY: %q", cfg.APIKey)
	}
	if cfg.BaseURL != "http://127.0.0.1:8645/v1" {
		t.Fatalf("base URL=%q", cfg.BaseURL)
	}

	t.Setenv("ARTEX_LLM_BASE_URL", "https://api.example.test/v1")
	if _, ok := FromEnv(); ok {
		t.Fatal("a public Hermes route must not be accepted from the environment")
	}
}

func TestHermesSubscriptionFromEnvDoesNotTreatGenericOpenAIAsHermes(t *testing.T) {
	t.Setenv("ARTEX_LLM_PROVIDER", "openai")
	t.Setenv("ARTEX_LLM_MODEL", "gpt-test")
	t.Setenv("ARTEX_LLM_BASE_URL", "http://host.docker.internal:8645/v1/chat/completions#fragment")
	t.Setenv("OPENAI_API_KEY", "cloud-key-that-must-not-leave")
	if _, ok := FromEnv(); ok {
		t.Fatal("generic OpenAI environment config at documented Hermes route was accepted")
	}
}

func TestHermesSubscriptionForbidsARTEXOutboundProxy(t *testing.T) {
	fromForm := ConfigFrom("hermes-subscription", "openai/gpt-test", "http://127.0.0.1:8645/v1", "", "http://127.0.0.1:8080")
	if fromForm.Proxy != "" {
		t.Fatalf("ConfigFrom retained Hermes proxy %q", fromForm.Proxy)
	}

	t.Setenv("ARTEX_LLM_PROVIDER", "hermes-subscription")
	t.Setenv("ARTEX_LLM_MODEL", "openai/gpt-test")
	t.Setenv("ARTEX_LLM_BASE_URL", "http://127.0.0.1:8645/v1")
	t.Setenv("ARTEX_LLM_PROXY", "http://127.0.0.1:8080")
	if _, ok := FromEnv(); ok {
		t.Fatal("environment Hermes config with ARTEX_LLM_PROXY must fail closed")
	}

	// Direct construction protects legacy database rows and callers that bypass
	// ConfigFrom. It must return before an HTTP client can use the proxy.
	raw := Config{
		Format:  FormatHermesSubscription,
		Model:   "openai/gpt-test",
		BaseURL: "http://127.0.0.1:8645/v1",
		Proxy:   "http://127.0.0.1:8080",
	}
	if _, err := raw.NewProvider(); err == nil || !strings.Contains(err.Error(), "does not permit") {
		t.Fatalf("legacy Hermes proxy error=%v, want direct-only rejection", err)
	}
}
