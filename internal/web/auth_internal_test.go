package web //nolint:testpackage // white-box tests cover the signed session payload

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSessionClaimsEnforceIdleAndAbsoluteLifetimes(t *testing.T) {
	t.Parallel()

	a := newAuthenticator("test-token-0123456789")
	login := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	cookie := a.issueSession(login, login)

	if _, ok := a.sessionClaims(cookie, login.Add(time.Hour)); !ok {
		t.Fatal("a one-hour-old session was rejected")
	}
	if _, ok := a.sessionClaims(cookie, login.Add(sessionIdleTTL-time.Second)); !ok {
		t.Fatal("a session inside the idle window was rejected")
	}
	if _, ok := a.sessionClaims(cookie, login.Add(sessionIdleTTL+time.Second)); ok {
		t.Fatal("a session past the idle window was accepted")
	}
}

func TestSessionClaimsKeepTheAbsoluteCapAcrossRefresh(t *testing.T) {
	t.Parallel()

	a := newAuthenticator("test-token-0123456789")
	login := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

	// A refresh six days in keeps issued=login and extends the idle deadline,
	// which is the point: activity slides the idle window, not the 7-day cap.
	refreshed := a.issueSession(login.Add(6*24*time.Hour), login)
	if _, ok := a.sessionClaims(refreshed, login.Add(6*24*time.Hour+time.Hour)); !ok {
		t.Fatal("a refreshed session was rejected inside its idle window")
	}
	if _, ok := a.sessionClaims(refreshed, login.Add(sessionAbsoluteTTL+time.Second)); ok {
		t.Fatal("a refreshed session outlived the absolute cap")
	}
}

func TestSessionClaimsRejectTamperingAndOldPayloads(t *testing.T) {
	t.Parallel()

	a := newAuthenticator("test-token-0123456789")
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

	cookie := a.issueSession(now, now)
	parts := strings.Split(cookie, ".")
	if len(parts) != 2 {
		t.Fatalf("cookie shape = %q", cookie)
	}
	if _, ok := a.sessionClaims(parts[0]+"."+parts[1][:len(parts[1])-1]+"A", now); ok {
		t.Fatal("a tampered signature was accepted")
	}

	// The pre-idle format was one integer. It must not verify as a session.
	legacy := sign(a.key, strconv.FormatInt(now.Add(time.Hour).Unix(), 10))
	if _, ok := a.sessionClaims(legacy, now); ok {
		t.Fatal("a single-field legacy payload was accepted")
	}
	if _, ok := a.sessionClaims("", now); ok {
		t.Fatal("an empty cookie was accepted")
	}
}
