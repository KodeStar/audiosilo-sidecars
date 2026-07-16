package auth

import (
	"errors"
	"testing"
	"time"
)

func TestHashVerifyRoundTrip(t *testing.T) {
	enc, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	ok, err := VerifyPassword("correct horse battery staple", enc)
	if err != nil || !ok {
		t.Fatalf("verify correct: ok=%v err=%v", ok, err)
	}
	ok, err = VerifyPassword("wrong", enc)
	if err != nil {
		t.Fatalf("verify wrong err: %v", err)
	}
	if ok {
		t.Fatal("verify wrong: ok=true, want false")
	}
}

func TestEnsureAdminGeneratesOnceOnly(t *testing.T) {
	m := New(NewMemStore())
	pw, err := m.EnsureAdmin()
	if err != nil {
		t.Fatalf("EnsureAdmin: %v", err)
	}
	if pw == "" {
		t.Fatal("expected a one-time password on first run")
	}
	// Second call must NOT regenerate or reprint.
	pw2, err := m.EnsureAdmin()
	if err != nil {
		t.Fatalf("EnsureAdmin 2: %v", err)
	}
	if pw2 != "" {
		t.Errorf("second EnsureAdmin returned a password %q, want empty", pw2)
	}
}

func TestLoginAllowedAndDenied(t *testing.T) {
	m := New(NewMemStore())
	pw, err := m.EnsureAdmin()
	if err != nil {
		t.Fatalf("EnsureAdmin: %v", err)
	}

	// Denied: wrong password.
	if _, err := m.Login("nope"); !errors.Is(err, ErrInvalidCreds) {
		t.Errorf("wrong password err = %v, want ErrInvalidCreds", err)
	}

	// Allowed: correct password mints a token that resolves.
	tok, err := m.Login(pw)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	ok, err := m.Resolve(tok)
	if err != nil || !ok {
		t.Fatalf("Resolve minted token: ok=%v err=%v", ok, err)
	}

	// Denied: a bogus token does not resolve.
	if ok, _ := m.Resolve("garbage"); ok {
		t.Error("bogus token resolved")
	}
	if ok, _ := m.Resolve(""); ok {
		t.Error("empty token resolved")
	}
}

func TestLogoutRevokes(t *testing.T) {
	m := New(NewMemStore())
	pw, _ := m.EnsureAdmin()
	tok, err := m.Login(pw)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if err := m.Logout(tok); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if ok, _ := m.Resolve(tok); ok {
		t.Error("token still resolves after logout")
	}
}

func TestChangePassword(t *testing.T) {
	m := New(NewMemStore())
	pw, _ := m.EnsureAdmin()

	// Denied: wrong current password.
	if err := m.ChangePassword("wrong", "a-new-strong-pass"); !errors.Is(err, ErrInvalidCreds) {
		t.Errorf("wrong current err = %v, want ErrInvalidCreds", err)
	}
	// Denied: new password too short.
	if err := m.ChangePassword(pw, "short"); !errors.Is(err, ErrPasswordTooShort) {
		t.Errorf("short new err = %v, want ErrPasswordTooShort", err)
	}
	// Allowed.
	if err := m.ChangePassword(pw, "a-new-strong-pass"); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	// Old password no longer works; new one does.
	if _, err := m.Login(pw); !errors.Is(err, ErrInvalidCreds) {
		t.Errorf("old password still works: %v", err)
	}
	if _, err := m.Login("a-new-strong-pass"); err != nil {
		t.Errorf("new password login: %v", err)
	}
}

func TestFileStorePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	m := New(store)
	pw, _ := m.EnsureAdmin()
	tok, err := m.Login(pw)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	// Reopen: admin and session survive.
	store2, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	m2 := New(store2)
	// First-run must NOT re-provision.
	if again, _ := m2.EnsureAdmin(); again != "" {
		t.Error("EnsureAdmin reprovisioned on reopen")
	}
	if ok, _ := m2.Resolve(tok); !ok {
		t.Error("session did not survive reopen")
	}
	if _, err := m2.Login(pw); err != nil {
		t.Errorf("login after reopen: %v", err)
	}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestRateLimiterAllowsBurstThenDenies(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	rl := NewRateLimiter(3, 1) // burst 3, 1 token/sec
	rl.now = c.now

	for i := 0; i < 3; i++ {
		if !rl.Allow("1.2.3.4") {
			t.Fatalf("attempt %d denied within burst", i)
		}
	}
	// Burst exhausted.
	if rl.Allow("1.2.3.4") {
		t.Fatal("4th attempt allowed, want denied")
	}
	// A different key has its own bucket.
	if !rl.Allow("5.6.7.8") {
		t.Fatal("different key denied")
	}
	// After 1 second, one token refills.
	c.t = c.t.Add(time.Second)
	if !rl.Allow("1.2.3.4") {
		t.Fatal("attempt after refill denied")
	}
	if rl.Allow("1.2.3.4") {
		t.Fatal("second attempt after single refill allowed")
	}
}
