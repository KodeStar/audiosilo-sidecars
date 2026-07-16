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
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	store := auth.NewMemStore()
	mgr := auth.New(store)
	pw, err := mgr.EnsureAdmin()
	if err != nil {
		t.Fatalf("EnsureAdmin: %v", err)
	}
	env := &testEnv{password: pw}
	env.api = New(Deps{
		Auth:    mgr,
		Limiter: auth.NewRateLimiter(100, 100),
		Secrets: secrets.NewMemStore(),
		Events:  events.NewHub(0),
		Version: "test",
		DataDir: "/tmp/data",
		Config:  config.Default(),
		ASR:     ASRInfo{Backend: "mlx-whisper", Available: true, Device: "metal", Version: "Python 3.13"},
		Save: func(c config.Config) error {
			cp := c
			env.saved = &cp
			return nil
		},
	})
	env.srv = httptest.NewServer(env.api.Handler())
	t.Cleanup(env.srv.Close)
	return env
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
			line := sc.Text()
			if strings.HasPrefix(line, "event: ") {
				done <- strings.TrimPrefix(line, "event: ")
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
