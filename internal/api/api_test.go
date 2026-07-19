package api

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/auth"
	"github.com/kodestar/audiosilo-sidecars/internal/config"
	"github.com/kodestar/audiosilo-sidecars/internal/events"
	"github.com/kodestar/audiosilo-sidecars/internal/secrets"
)

// testEnv wires an API over in-memory stores and returns a running test server
// plus the one-time admin password.
type testEnv struct {
	api      *API
	srv      *httptest.Server
	password string
	saved    *config.Config
	restart  chan struct{}
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	store := auth.NewMemStore()
	mgr := auth.New(store)
	pw, err := mgr.EnsureAdmin()
	if err != nil {
		t.Fatalf("EnsureAdmin: %v", err)
	}
	env := &testEnv{password: pw, restart: make(chan struct{}, 1)}
	env.api = New(Deps{
		Auth:    mgr,
		Limiter: auth.NewRateLimiter(100, 100),
		Secrets: secrets.NewMemStore(),
		Events:  events.NewHub(0),
		Version: "test",
		DataDir: "/tmp/data",
		Config:  config.Default(),
		ASR:     ASRInfo{Backend: "mlx-whisper", Available: true, Device: "metal", Version: "Python 3.13"},
		AgentStatus: func() AgentInfo {
			return AgentInfo{Backend: "claude", Available: true, Version: "2.1.211"}
		},
		Save: func(c config.Config) error {
			cp := c
			env.saved = &cp
			return nil
		},
		Restart: func() { env.restart <- struct{}{} },
	})
	env.srv = httptest.NewServer(env.api.Handler())
	t.Cleanup(env.srv.Close)
	return env
}

func TestRestartRequiresAuthAndSignalsDaemon(t *testing.T) {
	env := newTestEnv(t)
	resp := env.do(t, http.MethodPost, "/api/v1/system/restart", "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated restart = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	token := env.login(t)
	resp = env.do(t, http.MethodPost, "/api/v1/system/restart", token, "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("restart = %d, want 202", resp.StatusCode)
	}
	resp.Body.Close()
	select {
	case <-env.restart:
	case <-time.After(time.Second):
		t.Fatal("restart callback was not signalled")
	}
}

func (e *testEnv) login(t *testing.T) string {
	t.Helper()
	body := `{"password":"` + e.password + `"}`
	resp, err := http.Post(e.srv.URL+"/api/v1/auth/login", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("login post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", resp.StatusCode)
	}
	var lr loginResponse
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	return lr.Token
}

func (e *testEnv) do(t *testing.T, method, path, token, body string) *http.Response {
	t.Helper()
	var r *http.Request
	var err error
	if body != "" {
		r, err = http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	} else {
		r, err = http.NewRequest(method, e.srv.URL+path, nil)
	}
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, path, err)
	}
	return resp
}

func TestLoginAllowedAndDenied(t *testing.T) {
	env := newTestEnv(t)

	// Denied: wrong password -> 401.
	resp := env.do(t, http.MethodPost, "/api/v1/auth/login", "", `{"password":"wrong"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong password status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Allowed: correct password -> token.
	token := env.login(t)
	if token == "" {
		t.Fatal("empty token")
	}
}

func TestSystemRequiresAuth(t *testing.T) {
	env := newTestEnv(t)

	// Denied: no token -> 401.
	resp := env.do(t, http.MethodGet, "/api/v1/system", "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-token /system = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Denied: bogus token -> 401.
	resp = env.do(t, http.MethodGet, "/api/v1/system", "not-a-real-token", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bogus-token /system = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Allowed.
	token := env.login(t)
	resp = env.do(t, http.MethodGet, "/api/v1/system", token, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/system = %d, want 200", resp.StatusCode)
	}
	var sys systemResponse
	if err := json.NewDecoder(resp.Body).Decode(&sys); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sys.Version != "test" {
		t.Errorf("version = %q", sys.Version)
	}
	if len(sys.Tabs) != 4 {
		t.Errorf("tabs = %d, want 4", len(sys.Tabs))
	}
	if sys.ASR.Backend != "mlx-whisper" || !sys.ASR.Available || sys.ASR.Device != "metal" {
		t.Errorf("asr info = %+v, want mlx-whisper/available/metal", sys.ASR)
	}
	if sys.Agent.Backend != "claude" || !sys.Agent.Available || sys.Agent.Version != "2.1.211" {
		t.Errorf("agent info = %+v, want claude/available/2.1.211", sys.Agent)
	}
}

// TestSystemAgentNilStatus asserts a nil AgentStatus provider yields a zero
// AgentInfo (no backend, unavailable) rather than panicking.
func TestSystemAgentNilStatus(t *testing.T) {
	mem := auth.NewMemStore()
	mgr := auth.New(mem)
	pw, err := mgr.EnsureAdmin()
	if err != nil {
		t.Fatalf("EnsureAdmin: %v", err)
	}
	a := New(Deps{
		Auth:    mgr,
		Limiter: auth.NewRateLimiter(100, 100),
		Secrets: secrets.NewMemStore(),
		Events:  events.NewHub(0),
		Version: "test",
		DataDir: "/tmp/data",
		Config:  config.Default(),
		// AgentStatus deliberately omitted (nil).
	})
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/api/v1/auth/login", "application/json", strings.NewReader(`{"password":"`+pw+`"}`))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	var lr loginResponse
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/system", nil)
	req.Header.Set("Authorization", "Bearer "+lr.Token)
	sysResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("system: %v", err)
	}
	defer sysResp.Body.Close()
	var sys systemResponse
	if err := json.NewDecoder(sysResp.Body).Decode(&sys); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sys.Agent.Backend != "" || sys.Agent.Available {
		t.Errorf("agent = %+v, want zero value for nil provider", sys.Agent)
	}
}

func TestLogoutRevokesToken(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t)

	resp := env.do(t, http.MethodPost, "/api/v1/auth/logout", token, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()

	// Token no longer works.
	resp = env.do(t, http.MethodGet, "/api/v1/system", token, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("post-logout /system = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestChangePassword(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t)

	// Too short -> 400.
	resp := env.do(t, http.MethodPost, "/api/v1/auth/password", token, `{"current":"`+env.password+`","new":"short"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("short password = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// Wrong current -> 401.
	resp = env.do(t, http.MethodPost, "/api/v1/auth/password", token, `{"current":"nope","new":"a-good-password"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong current = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Success -> 204.
	resp = env.do(t, http.MethodPost, "/api/v1/auth/password", token, `{"current":"`+env.password+`","new":"a-good-password"}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("change password = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSettingsNeverEchoesSecrets(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t)

	// PUT a secret value.
	resp := env.do(t, http.MethodPut, "/api/v1/settings", token, `{"secrets":{"anthropic_api_key":"sk-super-secret"}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put settings = %d, want 200", resp.StatusCode)
	}
	putBody := readAll(t, resp)
	if strings.Contains(putBody, "sk-super-secret") {
		t.Fatalf("PUT settings echoed the secret value: %s", putBody)
	}
	if !strings.Contains(putBody, `"anthropic_api_key":true`) {
		t.Errorf("presence not reflected: %s", putBody)
	}

	// GET settings must show presence boolean, never the value.
	resp = env.do(t, http.MethodGet, "/api/v1/settings", token, "")
	getBody := readAll(t, resp)
	if strings.Contains(getBody, "sk-super-secret") {
		t.Fatalf("GET settings echoed the secret value: %s", getBody)
	}
	var view settingsResponse
	if err := json.Unmarshal([]byte(getBody), &view); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if !view.Secrets[secrets.AnthropicAPIKey] {
		t.Error("anthropic presence = false after set")
	}

	// Clear it with an empty string.
	resp = env.do(t, http.MethodPut, "/api/v1/settings", token, `{"secrets":{"anthropic_api_key":""}}`)
	clearBody := readAll(t, resp)
	if strings.Contains(clearBody, `"anthropic_api_key":true`) {
		t.Errorf("secret not cleared: %s", clearBody)
	}
}

// TestSettingsAgentGET asserts the GET view carries the agent config: backend,
// concurrency, timeout, and both model maps (emitted as {} not null).
func TestSettingsAgentGET(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t)

	resp := env.do(t, http.MethodGet, "/api/v1/settings", token, "")
	body := readAll(t, resp)
	// Empty openai map must serialize as {} not null.
	if !strings.Contains(body, `"openai_models":{}`) {
		t.Errorf("openai_models not emitted as {}: %s", body)
	}
	var view settingsResponse
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.Agent.Concurrency != config.DefaultConcurrency {
		t.Errorf("concurrency = %d, want %d", view.Agent.Concurrency, config.DefaultConcurrency)
	}
	if view.Agent.QueueConcurrency != config.DefaultAgentQueueConcurrency || view.Agent.MaxAgentsPerBook != config.DefaultMaxAgentsPerBook || view.Agent.EffectiveGlobalInvocationLimit != 6 || view.Agent.LegacyConcurrency {
		t.Errorf("capacity view = %+v", view.Agent)
	}
	if view.Agent.TimeoutMinutes != config.DefaultTimeoutMinutes {
		t.Errorf("timeout_minutes = %d, want %d", view.Agent.TimeoutMinutes, config.DefaultTimeoutMinutes)
	}
	if view.Agent.ClaudeModels["synthesizing"] != "opus" {
		t.Errorf("claude_models[synthesizing] = %q, want opus (default seed)", view.Agent.ClaudeModels["synthesizing"])
	}
	if view.Agent.OpenAIModels == nil {
		t.Error("openai_models should be non-nil (empty map)")
	}
}

func TestSettingsAgentPUTModernCapacityAndPartialLegacyMigration(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t)
	resp := env.do(t, http.MethodPut, "/api/v1/settings", token, `{"agent":{"queue_concurrency":3,"max_agents_per_book":2}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put modern capacity = %d: %s", resp.StatusCode, readAll(t, resp))
	}
	resp.Body.Close()
	if env.saved.Agent.Concurrency != 0 || env.saved.Agent.QueueConcurrency != 3 || env.saved.Agent.MaxAgentsPerBook != 2 {
		t.Fatalf("saved modern capacity=%+v", env.saved.Agent)
	}

	// Old clients/configs can migrate one dimension at a time without producing a
	// transient zero dimension: the legacy effective value seeds the omitted field.
	env.api.mu.Lock()
	env.api.cfg.Agent.Concurrency, env.api.cfg.Agent.QueueConcurrency, env.api.cfg.Agent.MaxAgentsPerBook = 4, 0, 0
	env.api.mu.Unlock()
	resp = env.do(t, http.MethodPut, "/api/v1/settings", token, `{"agent":{"queue_concurrency":2}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("partial migration = %d: %s", resp.StatusCode, readAll(t, resp))
	}
	resp.Body.Close()
	if env.saved.Agent.Concurrency != 0 || env.saved.Agent.QueueConcurrency != 2 || env.saved.Agent.MaxAgentsPerBook != 4 {
		t.Fatalf("partial migrated capacity=%+v", env.saved.Agent)
	}
}

// TestSettingsAgentPUT updates the backend + model maps + timeout and asserts they
// validate, persist through Save, and read back on GET.
func TestSettingsAgentPUT(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t)

	body := `{"agent":{"backend":"codex","concurrency":3,"timeout_minutes":30,"book_budget_usd":120.5,` +
		`"claude_models":{"fact_pass":"haiku"},"openai_models":{"auditing":"gpt-x"}}}`
	resp := env.do(t, http.MethodPut, "/api/v1/settings", token, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put agent = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// Persisted through Save with the new values.
	if env.saved == nil {
		t.Fatal("config not persisted")
	}
	if env.saved.Agent.Backend != "codex" || env.saved.Agent.Concurrency != 3 ||
		env.saved.Agent.TimeoutMinutes != 30 || env.saved.Agent.BookBudgetUSD != 120.5 {
		t.Errorf("saved agent scalars = %+v", env.saved.Agent)
	}
	if env.saved.Agent.Claude["fact_pass"] != "haiku" || len(env.saved.Agent.Claude) != 1 {
		t.Errorf("saved claude map not replaced wholesale: %v", env.saved.Agent.Claude)
	}
	if env.saved.Agent.OpenAI["auditing"] != "gpt-x" {
		t.Errorf("saved openai map = %v", env.saved.Agent.OpenAI)
	}

	// GET reflects the update.
	resp = env.do(t, http.MethodGet, "/api/v1/settings", token, "")
	var view settingsResponse
	if err := json.Unmarshal([]byte(readAll(t, resp)), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.Agent.Backend != "codex" || view.Agent.ClaudeModels["fact_pass"] != "haiku" {
		t.Errorf("GET after PUT = %+v", view.Agent)
	}
}

// TestSettingsAgentPUTRejectsInvalid asserts a bad backend and a non-agent-stage
// model key are both rejected with 400 carrying the validation message.
func TestSettingsAgentPUTRejectsInvalid(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t)

	// Bad backend.
	resp := env.do(t, http.MethodPut, "/api/v1/settings", token, `{"agent":{"backend":"gemini"}}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad backend = %d, want 400", resp.StatusCode)
	}
	badBody := readAll(t, resp)
	if !strings.Contains(badBody, "agent.backend") {
		t.Errorf("400 body missing validation message: %s", badBody)
	}

	// Non-agent-stage model key.
	resp = env.do(t, http.MethodPut, "/api/v1/settings", token, `{"agent":{"claude_models":{"splitting":"sonnet"}}}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("non-agent key = %d, want 400", resp.StatusCode)
	}
	keyBody := readAll(t, resp)
	if !strings.Contains(keyBody, "splitting") {
		t.Errorf("400 body missing offending key: %s", keyBody)
	}

	// The rejected updates must not have persisted.
	if env.saved != nil {
		t.Errorf("invalid PUT persisted config: %+v", env.saved)
	}
}

// TestSettingsContributionGET asserts the GET view carries the contribution config
// with its defaults.
func TestSettingsContributionGET(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t)

	resp := env.do(t, http.MethodGet, "/api/v1/settings", token, "")
	var view settingsResponse
	if err := json.Unmarshal([]byte(readAll(t, resp)), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.Contribution.Mode != config.DefaultContributionMode ||
		view.Contribution.Repo != config.DefaultContributionRepo ||
		!view.Contribution.AutoPurge ||
		view.Contribution.PollMinutes != config.DefaultContributionPollMinutes {
		t.Errorf("contribution view = %+v", view.Contribution)
	}
}

// TestSettingsContributionPUT updates the contribution config and asserts it
// validates, persists through Save, and reads back on GET (auto_purge false included).
func TestSettingsContributionPUT(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t)

	body := `{"contribution":{"mode":"pr","repo":"acme/meta","auto_purge":false,"poll_minutes":20}}`
	resp := env.do(t, http.MethodPut, "/api/v1/settings", token, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put contribution = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	if env.saved == nil {
		t.Fatal("config not persisted")
	}
	if env.saved.Contribution.Mode != "pr" || env.saved.Contribution.Repo != "acme/meta" ||
		env.saved.Contribution.AutoPurge || env.saved.Contribution.PollMinutes != 20 {
		t.Errorf("saved contribution = %+v", env.saved.Contribution)
	}

	resp = env.do(t, http.MethodGet, "/api/v1/settings", token, "")
	var view settingsResponse
	if err := json.Unmarshal([]byte(readAll(t, resp)), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.Contribution.Mode != "pr" || view.Contribution.AutoPurge {
		t.Errorf("GET after PUT = %+v", view.Contribution)
	}
}

// TestSettingsContributionPUTRejectsInvalid asserts a bad mode and a malformed repo
// are both rejected 400 and do not persist.
func TestSettingsContributionPUTRejectsInvalid(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t)

	resp := env.do(t, http.MethodPut, "/api/v1/settings", token, `{"contribution":{"mode":"email"}}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad mode = %d, want 400", resp.StatusCode)
	}
	if body := readAll(t, resp); !strings.Contains(body, "contribution.mode") {
		t.Errorf("400 body missing validation message: %s", body)
	}

	resp = env.do(t, http.MethodPut, "/api/v1/settings", token, `{"contribution":{"repo":"noslash"}}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad repo = %d, want 400", resp.StatusCode)
	}
	if body := readAll(t, resp); !strings.Contains(body, "contribution.repo") {
		t.Errorf("400 body missing validation message: %s", body)
	}

	if env.saved != nil {
		t.Errorf("invalid contribution PUT persisted config: %+v", env.saved)
	}
}

func TestSettingsCORSValidationAndPersist(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t)

	// Invalid origin -> 400.
	resp := env.do(t, http.MethodPut, "/api/v1/settings", token, `{"cors_origins":["http://x/foo"]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad origin = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// Valid origin -> 200 and persisted via Save.
	resp = env.do(t, http.MethodPut, "/api/v1/settings", token, `{"cors_origins":["http://localhost:5173"]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("good origin = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
	if env.saved == nil || len(env.saved.CORSOrigins) != 1 || env.saved.CORSOrigins[0] != "http://localhost:5173" {
		t.Errorf("config not persisted: %+v", env.saved)
	}
}

func TestCORSAllowListedOrigin(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t)
	// Configure an allowed origin.
	resp := env.do(t, http.MethodPut, "/api/v1/settings", token, `{"cors_origins":["http://ui.example"]}`)
	resp.Body.Close()

	// A request from the allowed origin gets ACAO.
	req, _ := http.NewRequest(http.MethodGet, env.srv.URL+"/api/v1/system", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Origin", "http://ui.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://ui.example" {
		t.Errorf("ACAO = %q, want http://ui.example", got)
	}

	// A disallowed origin gets no ACAO.
	req2, _ := http.NewRequest(http.MethodGet, env.srv.URL+"/api/v1/system", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	req2.Header.Set("Origin", "http://evil.example")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("do2: %v", err)
	}
	defer resp2.Body.Close()
	if got := resp2.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("disallowed origin got ACAO = %q", got)
	}
}

func TestCORSPreflightAllowsDelete(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t)
	// Configure an allowed origin.
	env.do(t, http.MethodPut, "/api/v1/settings", token, `{"cors_origins":["http://ui.example"]}`).Body.Close()

	// A DELETE preflight from the allowed origin -> 204 with DELETE in Allow-Methods
	// (the web Running tab deletes books cross-origin in dev).
	req, _ := http.NewRequest(http.MethodOptions, env.srv.URL+"/api/v1/books/1", nil)
	req.Header.Set("Origin", "http://ui.example")
	req.Header.Set("Access-Control-Request-Method", "DELETE")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("preflight do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("allowed DELETE preflight = %d, want 204", resp.StatusCode)
	}
	if allow := resp.Header.Get("Access-Control-Allow-Methods"); !strings.Contains(allow, "DELETE") {
		t.Errorf("Allow-Methods = %q, must include DELETE", allow)
	}

	// A preflight from a disallowed origin -> 403 with no CORS headers.
	req2, _ := http.NewRequest(http.MethodOptions, env.srv.URL+"/api/v1/books/1", nil)
	req2.Header.Set("Origin", "http://evil.example")
	req2.Header.Set("Access-Control-Request-Method", "DELETE")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("denied preflight do: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Errorf("disallowed preflight = %d, want 403", resp2.StatusCode)
	}
	if got := resp2.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("disallowed preflight got ACAO = %q", got)
	}
}

func TestEventsStreamHeartbeatAndAuth(t *testing.T) {
	env := newTestEnv(t)

	// Denied: no token.
	resp := env.do(t, http.MethodGet, "/api/v1/events", "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("events no-token = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	token := env.login(t)

	// Allowed via query token (as EventSource connects). Read the immediate
	// heartbeat frame.
	req, _ := http.NewRequest(http.MethodGet, env.srv.URL+"/api/v1/events?token="+token, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("events do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content-type = %q", ct)
	}

	// Read until we see a heartbeat event or time out.
	done := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if ev, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
				done <- ev
				return
			}
		}
		done <- ""
	}()
	select {
	case ev := <-done:
		if ev != events.HeartbeatType {
			t.Errorf("first event = %q, want heartbeat", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for heartbeat frame")
	}
}

func TestEventsReplaysPublished(t *testing.T) {
	env := newTestEnv(t)
	token := env.login(t)
	// Publish a real event before connecting.
	_ = env.api.events.Publish("book.state", map[string]string{"state": "queued"})

	// Connect with Last-Event-ID: 0 -> should replay the published event.
	req, _ := http.NewRequest(http.MethodGet, env.srv.URL+"/api/v1/events?token="+token, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("events do: %v", err)
	}
	defer resp.Body.Close()

	found := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "book.state") {
				found <- true
				return
			}
		}
		found <- false
	}()
	select {
	case ok := <-found:
		if !ok {
			t.Error("published event not replayed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for replay")
	}
}

// TestSystemUsesLiveStatus asserts that when Deps.LiveStatus is set, /system reports
// the executor's LIVE ASR capability and resolved tool paths (which a stage may have
// re-detected after a retry) rather than the boot-time static values.
func TestSystemUsesLiveStatus(t *testing.T) {
	mem := auth.NewMemStore()
	mgr := auth.New(mem)
	pw, err := mgr.EnsureAdmin()
	if err != nil {
		t.Fatalf("EnsureAdmin: %v", err)
	}
	a := New(Deps{
		Auth:    mgr,
		Limiter: auth.NewRateLimiter(100, 100),
		Secrets: secrets.NewMemStore(),
		Events:  events.NewHub(0),
		Version: "test",
		DataDir: "/tmp/data",
		Config:  config.Default(),
		// Boot-time (static) values the live callback must override.
		ASR:         ASRInfo{Backend: "boot", Available: false, Device: "cpu"},
		FFmpegPath:  "/boot/ffmpeg",
		FFprobePath: "/boot/ffprobe",
		LiveStatus: func() (ASRInfo, string, string) {
			return ASRInfo{Backend: "live", Available: true, Device: "metal"}, "/live/ffmpeg", "/live/ffprobe"
		},
	})
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/api/v1/auth/login", "application/json", strings.NewReader(`{"password":"`+pw+`"}`))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	var lr loginResponse
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/system", nil)
	req.Header.Set("Authorization", "Bearer "+lr.Token)
	sysResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("system: %v", err)
	}
	defer sysResp.Body.Close()
	var sys systemResponse
	if err := json.NewDecoder(sysResp.Body).Decode(&sys); err != nil {
		t.Fatalf("decode system: %v", err)
	}
	if sys.ASR.Backend != "live" || !sys.ASR.Available || sys.ASR.Device != "metal" {
		t.Errorf("asr = %+v, want the live values (live/available/metal)", sys.ASR)
	}
	if sys.Tools.FFmpeg != "/live/ffmpeg" || sys.Tools.FFprobe != "/live/ffprobe" {
		t.Errorf("tools = %+v, want the live paths", sys.Tools)
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return b.String()
}
