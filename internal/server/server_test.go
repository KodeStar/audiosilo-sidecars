package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestPrintBannerFirstRunShowsPassword(t *testing.T) {
	var b strings.Builder
	printBanner(&b, "127.0.0.1:8090", "/data", "abcd-efgh-ijkl-mnop", false)
	out := b.String()
	if !strings.Contains(out, "abcd-efgh-ijkl-mnop") {
		t.Error("first-run banner omitted the one-time password")
	}
	if !strings.Contains(out, "http://127.0.0.1:8090") {
		t.Error("banner omitted the listen URL")
	}
}

func TestPrintBannerLaterRunHidesPassword(t *testing.T) {
	var b strings.Builder
	printBanner(&b, "127.0.0.1:8090", "/data", "", false)
	if strings.Contains(strings.ToLower(b.String()), "password") {
		t.Errorf("later-run banner mentioned a password: %s", b.String())
	}
}

func TestPrintBannerKeychainFallbackWarning(t *testing.T) {
	var b strings.Builder
	printBanner(&b, "127.0.0.1:8090", "/data", "", true)
	if !strings.Contains(b.String(), "secrets.json") {
		t.Error("fallback banner omitted the secrets.json warning")
	}
}

func TestRunBootsAndServesThenShutsDown(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			DataDir: dir,
			Listen:  "127.0.0.1:8137",
			Version: "test",
			Out:     io.Discard,
		})
	}()

	// Poll until the server answers.
	base := "http://127.0.0.1:8137"
	if !waitReady(base+"/api/v1/system", 3*time.Second) {
		cancel()
		<-done
		t.Fatal("server never became ready")
	}

	// Unauthed /system must be 401.
	resp, err := http.Get(base + "/api/v1/system")
	if err != nil {
		t.Fatalf("GET /system: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthed /system = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Embedded UI root serves 200.
	resp, err = http.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET / = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not shut down")
	}
}

func waitReady(url string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url) //nolint:noctx // test helper
		if err == nil {
			resp.Body.Close()
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
