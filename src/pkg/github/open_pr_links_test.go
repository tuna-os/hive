package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// openPRGuardServer serves one repo with the given open PRs and issue #42
// carrying issueLabels, recording label writes and refusing anything else.
type openPRGuardServer struct {
	mu          sync.Mutex
	pullsCalls  int
	addedLabels []string
	createdLbl  []string
	failPulls   bool
	// rateLimitReset, when set, answers the pulls listing as GitHub does
	// once the installation's primary limit is spent (403, remaining 0).
	rateLimitReset time.Time
	// secondaryRetryAfter, when positive, answers it with a secondary
	// rate limit carrying Retry-After in seconds.
	secondaryRetryAfter int
}

func newOpenPRGuardServer(t *testing.T, prs []map[string]any, issueLabels []string) (*httptest.Server, *openPRGuardServer) {
	t.Helper()
	obs := &openPRGuardServer{}
	labels := make([]map[string]any, 0, len(issueLabels))
	for _, l := range issueLabels {
		labels = append(labels, map[string]any{"name": l})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		obs.mu.Lock()
		obs.pullsCalls++
		fail := obs.failPulls
		reset := obs.rateLimitReset
		retryAfter := obs.secondaryRetryAfter
		obs.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if !reset.IsZero() {
			w.Header().Set("X-RateLimit-Limit", "6450")
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"message":"API rate limit of 6450 exceeded for installation ID 1."}`)
			return
		}
		if retryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"message":"You have exceeded a secondary rate limit.","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api#about-secondary-rate-limits"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(prs)
	})
	mux.HandleFunc("/repos/o/r/issues/42", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "state": "open", "labels": labels})
	})
	mux.HandleFunc("/repos/o/r/issues/42/labels", func(w http.ResponseWriter, r *http.Request) {
		var names []string
		_ = json.NewDecoder(r.Body).Decode(&names)
		obs.mu.Lock()
		obs.addedLabels = append(obs.addedLabels, names...)
		obs.mu.Unlock()
		_, _ = io.WriteString(w, `[]`)
	})
	mux.HandleFunc("/repos/o/r/labels/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/repos/o/r/labels", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		obs.mu.Lock()
		obs.createdLbl = append(obs.createdLbl, body.Name)
		obs.mu.Unlock()
		_, _ = io.WriteString(w, `{"name":"`+body.Name+`"}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	return httptest.NewServer(mux), obs
}

func openPRFixture(number int, body, author string, createdAt time.Time) map[string]any {
	return map[string]any{
		"number":     number,
		"state":      "open",
		"title":      fmt.Sprintf("fix %d", number),
		"body":       body,
		"html_url":   fmt.Sprintf("https://github.com/o/r/pull/%d", number),
		"created_at": createdAt.UTC().Format(time.RFC3339),
		"updated_at": createdAt.UTC().Format(time.RFC3339),
		"user":       map[string]any{"login": author, "type": "User"},
		"head":       map[string]any{"sha": fmt.Sprintf("sha%d", number)},
	}
}

func TestOpenPRGuard_LinkedIssueIsRefusedAndLabelled(t *testing.T) {
	t0 := time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC)
	srv, obs := newOpenPRGuardServer(t, []map[string]any{
		openPRFixture(14775, "Fixes #42", "clubanderson", t0),
		openPRFixture(14780, "Fixes #42", "thatguywob", t0.Add(9*time.Minute)),
		openPRFixture(14790, "Refs #42", "someone", t0.Add(-time.Hour)),
	}, nil)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"r"})

	got, err := c.IssueDispatchBlockReason(context.Background(), "o/r", 42)
	if err != nil {
		t.Fatalf("IssueClaimBlockReason: %v", err)
	}
	if want := ClaimBlockReasonOpenPRPrefix + "14775"; got != want {
		t.Fatalf("reason = %q, want %q (earliest closing PR wins; Refs #42 is not a fix)", got, want)
	}
	if len(obs.addedLabels) != 1 || obs.addedLabels[0] != HasOpenPRLabel {
		t.Fatalf("labels added = %v, want [%s]", obs.addedLabels, HasOpenPRLabel)
	}
	if len(obs.createdLbl) != 1 || obs.createdLbl[0] != HasOpenPRLabel {
		t.Fatalf("labels created = %v, want the guard label created on first use", obs.createdLbl)
	}

	// Second call inside the TTL: no second listing, and the label is not
	// re-added when the issue already carries it.
	link, ok := c.OpenPRLinkGuard(context.Background(), "o/r", 42, []string{HasOpenPRLabel})
	if !ok || link.PRNumber != 14775 || link.PRAuthor != "clubanderson" {
		t.Fatalf("guard = %+v, %v; want PR 14775 by clubanderson", link, ok)
	}
	if obs.pullsCalls != 1 {
		t.Fatalf("open-PR listings = %d, want 1 (cached per sweep)", obs.pullsCalls)
	}
	if len(obs.addedLabels) != 1 {
		t.Fatalf("labels added = %v, want no second write when the label is present", obs.addedLabels)
	}
}

// The plain claim gate — renewals, PR-backed start signals, the release
// tick — never lists PRs: the holder's own PR is the open PR.
func TestOpenPRGuard_ClaimGateDoesNotListPRs(t *testing.T) {
	srv, obs := newOpenPRGuardServer(t, []map[string]any{
		openPRFixture(7, "Fixes #42", "hive-app[bot]", time.Now()),
	}, nil)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"r"})
	got, err := c.IssueClaimBlockReason(context.Background(), "o/r", 42)
	if err != nil || got != "" {
		t.Fatalf("reason = %q, %v; want admitted", got, err)
	}
	if obs.pullsCalls != 0 {
		t.Fatalf("open-PR listings = %d, want 0 from the claim gate", obs.pullsCalls)
	}
	var nilClient *Client
	if _, err := nilClient.IssueDispatchBlockReason(context.Background(), "o/r", 42); err == nil {
		t.Fatal("nil client must error")
	}
}

func TestOpenPRGuard_DispatchGateKeepsLabelRefusalsFirst(t *testing.T) {
	srv, obs := newOpenPRGuardServer(t, []map[string]any{
		openPRFixture(7, "Fixes #42", "hive-app[bot]", time.Now()),
	}, []string{"needs-human"})
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"r"})
	got, err := c.IssueDispatchBlockReason(context.Background(), "o/r", 42)
	if err != nil || got != "needs-human" {
		t.Fatalf("reason = %q, %v; want needs-human before any listing", got, err)
	}
	if obs.pullsCalls != 0 {
		t.Fatalf("open-PR listings = %d, want 0 when a label already refuses", obs.pullsCalls)
	}
}

func TestOpenPRGuard_UnlinkedIssueIsAdmitted(t *testing.T) {
	srv, obs := newOpenPRGuardServer(t, []map[string]any{
		openPRFixture(7, "Fixes #41", "someone", time.Now()),
	}, nil)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"r"})
	got, err := c.IssueDispatchBlockReason(context.Background(), "o/r", 42)
	if err != nil || got != "" {
		t.Fatalf("reason = %q, %v; want admitted", got, err)
	}
	if len(obs.addedLabels) != 0 {
		t.Fatalf("labels added = %v, want none", obs.addedLabels)
	}
}

func TestOpenPRGuard_ConfigOffAdmitsWithoutListing(t *testing.T) {
	srv, obs := newOpenPRGuardServer(t, []map[string]any{
		openPRFixture(7, "Fixes #42", "someone", time.Now()),
	}, nil)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"r"})
	c.SetSkipIssuesWithOpenPR(false)
	if c.SkipIssuesWithOpenPR() {
		t.Fatal("SkipIssuesWithOpenPR must report the switch off")
	}
	got, err := c.IssueDispatchBlockReason(context.Background(), "o/r", 42)
	if err != nil || got != "" {
		t.Fatalf("reason = %q, %v; want admitted with the guard off", got, err)
	}
	if obs.pullsCalls != 0 {
		t.Fatalf("open-PR listings = %d, want 0 with the guard off", obs.pullsCalls)
	}
}

func TestOpenPRGuard_ListingFailureFailsOpen(t *testing.T) {
	srv, obs := newOpenPRGuardServer(t, nil, nil)
	defer srv.Close()
	obs.failPulls = true
	c := newTestClient(t, srv, "o", []string{"r"})
	got, err := c.IssueDispatchBlockReason(context.Background(), "o/r", 42)
	if err != nil || got != "" {
		t.Fatalf("reason = %q, %v; want admitted when the listing fails", got, err)
	}
	if _, ok, err := c.OpenPRLinkingIssue(context.Background(), "o/r", 42); ok || err == nil {
		t.Fatalf("OpenPRLinkingIssue = ok=%v err=%v; want the listing error surfaced", ok, err)
	}
}

func TestOpenPRGuard_ListingFailureIsNotRetriedWithinWindow(t *testing.T) {
	srv, obs := newOpenPRGuardServer(t, nil, nil)
	defer srv.Close()
	obs.failPulls = true
	c := newTestClient(t, srv, "o", []string{"r"})
	if _, ok := c.OpenPRLinkGuard(context.Background(), "o/r", 42, nil); ok {
		t.Fatal("guard refused; want fail-open")
	}
	calls := obs.pullsCalls
	if calls == 0 {
		t.Fatal("first claim did not attempt a listing")
	}
	for i := 0; i < 5; i++ {
		if _, ok := c.OpenPRLinkGuard(context.Background(), "o/r", 42, nil); ok {
			t.Fatal("guard refused; want fail-open")
		}
	}
	if obs.pullsCalls != calls {
		t.Fatalf("listing attempts = %d, want %d: failure must be replayed within the window", obs.pullsCalls, calls)
	}
}

// rateLimitedGuardClient is a client whose pulls listing is rate-limited
// until reset, on a fake clock starting at t0, with PR 14775 open and
// closing #42 — the PR a rate-limited guard cannot see.
func rateLimitedGuardClient(t *testing.T, t0, reset time.Time) (*Client, *openPRGuardServer, *time.Time) {
	t.Helper()
	srv, obs := newOpenPRGuardServer(t, []map[string]any{
		openPRFixture(14775, "Fixes #42", "clubanderson", t0.Add(-time.Hour)),
	}, nil)
	t.Cleanup(srv.Close)
	// The 403s below engage the process-wide pacer; keep it out of other tests.
	ResetRateLimitPacingForTest()
	t.Cleanup(ResetRateLimitPacingForTest)
	obs.rateLimitReset = reset
	c := newTestClient(t, srv, "o", []string{"r"})
	clock := t0
	c.openPRLinks().now = func() time.Time { return clock }
	return c, obs, &clock
}

// Production: an exhausted installation limit made go-github fail every
// listing locally, and each claim candidate re-listed and re-warned (47k
// warnings an hour). The failure must be replayed until the reset — past
// the one-minute failure TTL, across both repo spellings — and warned once.
func TestOpenPRGuard_RateLimitedListingIsNotRetriedUntilReset(t *testing.T) {
	t0 := time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC)
	reset := t0.Add(30 * time.Minute)
	c, obs, clock := rateLimitedGuardClient(t, t0, reset)
	var logs bytes.Buffer
	c.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	for i := range 20 {
		*clock = t0.Add(time.Duration(i) * time.Minute)
		repo := "o/r"
		if i%2 == 1 {
			repo = "r"
		}
		c.OpenPRLinkGuard(context.Background(), repo, 42, nil)
	}
	if obs.pullsCalls != 1 {
		t.Fatalf("listing attempts = %d, want 1 until the rate limit resets", obs.pullsCalls)
	}
	if n := strings.Count(logs.String(), "open-PR guard"); n != 1 {
		t.Fatalf("guard warnings = %d, want 1 per repo per window:\n%s", n, logs.String())
	}
}

// While rate-limited the guard cannot see PR 14775, so it must refuse the
// claim (fail closed) instead of admitting an already-linked issue — and
// write no has-open-pr label, since no PR is known.
func TestOpenPRGuard_RateLimitedListingRefusesClaim(t *testing.T) {
	t0 := time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC)
	reset := t0.Add(30 * time.Minute)
	c, obs, _ := rateLimitedGuardClient(t, t0, reset)
	got, err := c.IssueDispatchBlockReason(context.Background(), "o/r", 42)
	if err != nil || got != ClaimBlockReasonOpenPRRateLimited {
		t.Fatalf("reason = %q, %v; want %q while rate-limited", got, err, ClaimBlockReasonOpenPRRateLimited)
	}
	link, ok := c.OpenPRLinkGuard(context.Background(), "r", 42, nil)
	if !ok || !link.RateLimited() || !link.RateLimitedUntil.Equal(reset) || link.PRNumber != 0 {
		t.Fatalf("guard = %+v, %v; want a refusal deferred until %s", link, ok, reset)
	}
	if len(obs.addedLabels) != 0 || len(obs.createdLbl) != 0 {
		t.Fatalf("labels added = %v created = %v; want none for a rate-limit deferral", obs.addedLabels, obs.createdLbl)
	}
}

func TestOpenPRGuard_RateLimitedListingRetriesAfterReset(t *testing.T) {
	t0 := time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC)
	reset := t0.Add(30 * time.Minute)
	c, obs, clock := rateLimitedGuardClient(t, t0, reset)
	if link, ok := c.OpenPRLinkGuard(context.Background(), "o/r", 42, nil); !ok || !link.RateLimited() {
		t.Fatalf("guard = %+v, %v; want deferred while rate-limited", link, ok)
	}
	*clock = reset.Add(-time.Second)
	if link, ok := c.OpenPRLinkGuard(context.Background(), "o/r", 42, nil); !ok || !link.RateLimited() || obs.pullsCalls != 1 {
		t.Fatalf("guard = %+v, %v after %d listings; want still deferred just before the reset", link, ok, obs.pullsCalls)
	}
	obs.mu.Lock()
	obs.rateLimitReset = time.Time{}
	obs.mu.Unlock()
	ResetRateLimitPacingForTest() // the reset has passed for the pacer too
	*clock = reset.Add(time.Second)
	got, err := c.IssueDispatchBlockReason(context.Background(), "o/r", 42)
	if want := ClaimBlockReasonOpenPRPrefix + "14775"; err != nil || got != want {
		t.Fatalf("reason = %q, %v; want %q once the limit reset", got, err, want)
	}
	if obs.pullsCalls != 2 {
		t.Fatalf("listing attempts = %d, want 2 (one relist after the reset)", obs.pullsCalls)
	}
}

// A secondary limit backs off for its Retry-After, not the failure TTL.
func TestOpenPRGuard_SecondaryRateLimitRefusesForRetryAfter(t *testing.T) {
	t0 := time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC)
	c, obs, clock := rateLimitedGuardClient(t, t0, time.Time{})
	obs.secondaryRetryAfter = 120
	link, ok := c.OpenPRLinkGuard(context.Background(), "o/r", 42, nil)
	if want := t0.Add(2 * time.Minute); !ok || !link.RateLimitedUntil.Equal(want) {
		t.Fatalf("guard = %+v, %v; want deferred until %s", link, ok, want)
	}
	*clock = t0.Add(90 * time.Second)
	if _, ok := c.OpenPRLinkGuard(context.Background(), "o/r", 42, nil); !ok || obs.pullsCalls != 1 {
		t.Fatalf("guard ok=%v after %d listings; want deferred without relisting inside Retry-After", ok, obs.pullsCalls)
	}
}

func TestOpenPRGuard_InvalidateForcesRelist(t *testing.T) {
	srv, obs := newOpenPRGuardServer(t, nil, nil)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"r"})
	for i := 0; i < 2; i++ {
		if _, ok, err := c.OpenPRLinkingIssue(context.Background(), "r", 42); ok || err != nil {
			t.Fatalf("OpenPRLinkingIssue = %v, %v", ok, err)
		}
	}
	if obs.pullsCalls != 1 {
		t.Fatalf("listings = %d, want 1 before invalidation", obs.pullsCalls)
	}
	c.InvalidateOpenPRLinks("o/r")
	if _, _, err := c.OpenPRLinkingIssue(context.Background(), "o/r", 42); err != nil {
		t.Fatal(err)
	}
	c.InvalidateOpenPRLinks("")
	if _, _, err := c.OpenPRLinkingIssue(context.Background(), "o/r", 42); err != nil {
		t.Fatal(err)
	}
	if obs.pullsCalls != 3 {
		t.Fatalf("listings = %d, want 3 after two invalidations", obs.pullsCalls)
	}
	var nilClient *Client
	if nilClient.SkipIssuesWithOpenPR() {
		t.Fatal("nil client must report the guard off")
	}
	nilClient.SetSkipIssuesWithOpenPR(true)
	nilClient.InvalidateOpenPRLinks("")
	if _, ok := nilClient.OpenPRLinkGuard(context.Background(), "o/r", 1, nil); ok {
		t.Fatal("nil client guard must admit")
	}
	if !strings.HasPrefix(ClaimBlockReasonOpenPRPrefix, "open PR already linked") {
		t.Fatalf("prefix %q drifted from the log line", ClaimBlockReasonOpenPRPrefix)
	}
}
