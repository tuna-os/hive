package hub

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"golang.org/x/net/publicsuffix"
)

var saasUsersDir = "/data/saas/users"

// defaultHubAdminUsername is the compile-time fallback fleet-superuser
// GitHub login, used when HIVE_HUB_ADMIN_USERNAME is not set in the
// environment. Keeping the historical value as the default preserves
// backward compatibility for existing deployments.
const defaultHubAdminUsername = "clubanderson"

// hubAdminUsername is the GitHub login treated as the fleet superuser. It is
// resolved once at package init from HIVE_HUB_ADMIN_USERNAME (falling back to
// defaultHubAdminUsername), rather than being a hardcoded constant (audit F12,
// CWE-798). This lets a deployment override the admin login via config and
// removes the fragility where a GitHub username rename or release would silently
// transfer — or forfeit — fleet-superuser privilege. It is set once at startup
// and treated as read-only thereafter (never mutated at runtime).
var hubAdminUsername = resolveHubAdminUsername()

// resolveHubAdminUsername reads the admin login from the environment, trimming
// surrounding whitespace, and falls back to defaultHubAdminUsername when the
// env var is unset or blank.
func resolveHubAdminUsername() string {
	if v := strings.TrimSpace(os.Getenv("HIVE_HUB_ADMIN_USERNAME")); v != "" {
		return v
	}
	return defaultHubAdminUsername
}

// hubAdminsEnv is the env var (comma-separated canonical ids, e.g.
// "github:clubanderson,google:1078...") that overrides the admin SET for
// multi-provider login. A bare login in the list is accepted and treated as
// github: via the identity shim. When unset, the admin set is the single
// hubAdminUsername above (itself overridable via HIVE_HUB_ADMIN_USERNAME), so
// both existing env contracts keep working. Prefer isHubAdmin()/
// primaryHubAdmin() over comparing against hubAdminUsername directly, so
// multi-provider admins work and so a same-subject identity on a DIFFERENT
// provider can never inherit admin.
const hubAdminsEnv = "HIVE_HUB_ADMINS"

// hubAdminSet returns the canonicalized set of admin identities. Sourced from
// HIVE_HUB_ADMINS when set, else the single hubAdminUsername. Every entry is
// run through canonicalizeLegacy so a bare login becomes github:<login>; this
// is what stops a Google/IBMid user whose subject happens to be the admin's
// login from matching the GitHub admin.
func hubAdminSet() map[string]bool {
	raw := strings.TrimSpace(os.Getenv(hubAdminsEnv))
	entries := []string{hubAdminUsername}
	if raw != "" {
		entries = splitCSV(raw)
	}
	set := make(map[string]bool, len(entries))
	for _, e := range entries {
		if c := canonicalizeLegacy(e); c != "" {
			set[strings.ToLower(c)] = true
		}
	}
	return set
}

// isHubAdmin reports whether the given identity (bare-legacy or canonical) is a
// hub admin. Both the input and the configured admin ids are canonicalized, so
// "clubanderson", "github:clubanderson", and "GitHub:ClubAnderson" all match the
// default admin, while "google:clubanderson" does NOT.
func isHubAdmin(id string) bool {
	if id == "" {
		return false
	}
	return hubAdminSet()[strings.ToLower(canonicalizeLegacy(id))]
}

// primaryHubAdmin returns the canonical identity of the primary hub admin — the
// first entry of HIVE_HUB_ADMINS, else the resolved hubAdminUsername. Used where
// the code needs a concrete admin identity to WRITE (e.g. audit attribution).
func primaryHubAdmin() string {
	raw := strings.TrimSpace(os.Getenv(hubAdminsEnv))
	if raw != "" {
		if list := splitCSV(raw); len(list) > 0 {
			return canonicalizeLegacy(list[0])
		}
	}
	return canonicalizeLegacy(hubAdminUsername)
}

// userCanonicalID returns a user's canonical wire-form identity: the explicit
// CanonicalID when present, else the legacy-shimmed GitHubUsername (a bare login
// becomes github:<login>). This is the single source of truth for "who is this
// record" across the dual-read storage path and the provider badge.
func userCanonicalID(u *SaaSUser) string {
	if u == nil {
		return ""
	}
	if u.CanonicalID != "" {
		return canonicalizeLegacy(u.CanonicalID)
	}
	return canonicalizeLegacy(u.GitHubUsername)
}

// userProvider returns a user's login provider ("github"/"google"/"ibmid"/
// "redhat"/"microsoft"/"custom"), from the stored Provider field when set, else
// parsed from the canonical identity. Legacy records with neither resolve to
// "github" via the shim. Drives the admin Users-table auth-method badge.
func userProvider(u *SaaSUser) string {
	if u == nil {
		return ""
	}
	if u.Provider != "" {
		return strings.ToLower(u.Provider)
	}
	if p, _, ok := parseCanonical(userCanonicalID(u)); ok {
		return p
	}
	return legacyProvider
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func repoTargetForgeHost(baseURL string) string {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return "github.com"
	}
	if u, err := url.Parse(baseURL); err == nil && u.Host != "" {
		return strings.ToLower(u.Host)
	}
	return strings.ToLower(strings.Trim(strings.TrimPrefix(strings.TrimPrefix(baseURL, "https://"), "http://"), "/"))
}

// hubUpgradeDebounce is the minimum gap between hub self-upgrade rollout
// restarts. The behind-latest check runs every SHA-poll cycle, so without this
// the hub could re-trigger a restart before the previous rollout's new pod
// reports the new hash. One cycle plus rollout headroom.
const hubUpgradeDebounce = 4 * time.Minute

// upgradeKubectlTimeout bounds a single auto-upgrade kubectl call. kubectl's own
// default retries an unreachable API server for ~2 minutes before giving up;
// paying that per hive serialized the upgrade loop and starved the hub's own
// upgrade check that runs after it. The heartbeat fallback is the real delivery
// path for unreachable clusters, so failing fast costs nothing.
//
// Retained under nolint despite having no current caller: three other files
// cite it BY NAME as the basis for their own timeouts
// (hosted_namespace_identity.go, netadmin_reconcile.go, saas_bulk.go). Deleting
// it to satisfy the linter would orphan those comments and lose the recorded
// reasoning for the 15s figure, which is the thing worth keeping.
//
//nolint:unused // referenced by name from three sibling timeout comments
const upgradeKubectlTimeout = 15 * time.Second

// clusterUnreachableTTL is how long the hub skips kubectl for a cluster after a
// dial failure. Long enough that one poll cycle probes a firewalled cluster at
// most once, short enough that a cluster coming back online is picked up soon.
const clusterUnreachableTTL = 10 * time.Minute

// beginUpgrade marks the registry entry at index i as upgrading toward target
// and stamps UpgradeStartedAt so the dashboard can show a TRUE elapsed time.
//
// The invariant it enforces: UpgradeStartedAt is the moment an upgrade toward a
// given target FIRST began, and it survives every retry of that same upgrade. A
// hive that is already Upgrading toward the SAME target keeps its original start
// time — it is retrying, not starting over. Only a genuinely new upgrade (the
// hive was not upgrading, or the target actually changed) re-stamps the clock.
//
// This is the fix for the reset-every-retry bug: a crash-looping self-upgrade
// re-enters the arm/retry paths every heartbeat cycle with the SAME target, and
// each re-stamp of UpgradeStartedAt to time.Now() reset the displayed
// "Upgrading Ns" back toward zero. The elapsed time therefore never crossed
// staleUpgradeTimeout, so the row never turned red and the stuck-upgrade alert
// never fired even while the hive thrashed for hours. Routing every site that
// sets Upgrading=true through this one helper makes the invariant impossible to
// violate in one place and not another.
//
// The caller MUST hold s.mu. i must be a valid index into s.registry.Hives.
func (s *HubServer) beginUpgrade(i int, target string) {
	h := &s.registry.Hives[i]
	// (Re)stamp the start ONLY on a genuinely new upgrade: the hive was not
	// already upgrading, or it is now aimed at a different target. A retry
	// toward the same target keeps the original start so the timer is honest.
	if !h.Upgrading || h.UpgradeTarget != target || h.UpgradeStartedAt.IsZero() {
		h.UpgradeStartedAt = time.Now()
	}
	h.Upgrading = true
	h.UpgradeTarget = target
}

// clearUpgradeLatch drops every trace of an in-flight upgrade on the entry at
// index i: the Upgrading flag, its target, AND the start clock. Zeroing
// UpgradeStartedAt on completion/cancel/orphan-clear guarantees the NEXT upgrade
// starts a fresh timer even if some future path forgot to re-stamp — the
// stuck-upgrade signal is only as honest as the clock it reads. Callers that
// clear Upgrading MUST route through this so the invariant (a non-zero
// UpgradeStartedAt implies an upgrade is genuinely in flight) holds everywhere.
//
// The caller MUST hold s.mu. i must be a valid index into s.registry.Hives.
func (s *HubServer) clearUpgradeLatch(i int) {
	h := &s.registry.Hives[i]
	h.Upgrading = false
	h.UpgradeTarget = ""
	h.UpgradeStartedAt = time.Time{}
}

// stampObservedUpgrade extends the beginUpgrade invariant to upgrades the hub
// merely OBSERVES rather than arms: whenever an entry ends up Upgrading=true
// from ANY source, it must carry a non-zero UpgradeStartedAt — the dashboard
// row only renders the elapsed counter, and the stuck-upgrade alert only
// fires, when the clock is non-zero. The spoke-reported path (a heartbeat
// whose payload says Upgrading with no hub-side target) rebuilt the registry
// entry from scratch every beat with a zero clock, so the badge rendered
// without its counter and the alert was blind for the whole upgrade.
//
// prevStart is the previous beat's clock for the same entry (zero when there
// is no previous state, e.g. a first heartbeat). Carrying it forward — never
// re-stamping a live clock — is what keeps this compatible with the #2725
// rule that a retry of the same upgrade preserves its original start time.
// Entries that are not Upgrading, or already carry a clock (the hub-armed
// paths route through beginUpgrade), are left untouched.
func stampObservedUpgrade(entry *RegistryEntry, prevStart time.Time) {
	if !entry.Upgrading || !entry.UpgradeStartedAt.IsZero() {
		return
	}
	if !prevStart.IsZero() {
		entry.UpgradeStartedAt = prevStart
		return
	}
	entry.UpgradeStartedAt = time.Now()
}

type SaaSUser struct {
	GitHubUsername string            `json:"github_username"`
	CreatedAt      string            `json:"created_at"`
	LastLogin      string            `json:"last_login"`
	Hives          map[string]string `json:"hives"`
	// HiveExpiry optionally bounds a grant in Hives: hive ID → RFC3339 UTC
	// instant after which that grant is revoked (#4150). Grants without an
	// entry are permanent — omitempty keeps every pre-expiry record
	// byte-identical on disk. Enforced at read time by loadSaaSUser's prune
	// and persisted/audited by sweepExpiredAccess (access_expiry.go).
	HiveExpiry     map[string]string `json:"hive_expiry,omitempty"`
	SaaSQuota      int               `json:"saas_quota"`
	Blocked        bool              `json:"blocked"`
	EncryptedToken string            `json:"encrypted_token,omitempty"`

	// Multi-provider identity (phase 1d). All omitempty so the thousands of
	// existing GitHub-only records on the PVC round-trip byte-identical until a
	// user first logs in / links after this ships.
	//
	//   CanonicalID  the wire-form primary identity ("google:1078", "github:foo").
	//                Empty on a legacy record → the shim treats GitHubUsername as
	//                the (github:) primary. saveSaaSUser/loadSaaSUser already dual-
	//                read on GitHubUsername; CanonicalID is the explicit form used
	//                by the badge and by Phase 2's OIDC callback when it creates a
	//                non-GitHub user.
	//   Provider     "github" | "google" | "ibmid" | "redhat" | "microsoft" |
	//                "custom" — drives the admin Users-table auth-method badge.
	//                Derivable from CanonicalID but stored so the badge needs no
	//                parse per render.
	//   AvatarURL    stored avatar (Google/IBMid give a picture claim); replaces
	//                the derived github.com/<login>.png where present.
	//   Email        the provider email claim (display only; NEVER the key — subs
	//                are stable, emails are reassignable).
	//   LinkedGitHubLogin  an OPTIONAL attached GitHub identity for a non-GitHub
	//                primary who needs user-scoped GitHub calls (contributor
	//                reissue). Never required to own a hive — the App does the
	//                GitHub work.
	CanonicalID       string `json:"canonical_id,omitempty"`
	Provider          string `json:"provider,omitempty"`
	AvatarURL         string `json:"avatar_url,omitempty"`
	Email             string `json:"email,omitempty"`
	LinkedGitHubLogin string `json:"linked_github_login,omitempty"`

	// DisplayName is the PROVIDER-ASSERTED human name from the OIDC name claim
	// (or userinfo), refreshed on every completed login. Distinct from FullName,
	// which is ADMIN-entered CRM text and must never be clobbered by a login.
	// Display only — the identity key stays provider:sub. omitempty so existing
	// records round-trip byte-identical until the user's next login enriches
	// them (backfill-by-login, no migration).
	DisplayName string `json:"display_name,omitempty"`

	// Contact/CRM fields. Admin-maintained free text used to reach a hub user
	// outside GitHub (and to remember what was said last time). All three are
	// omitempty so the thousands of existing user records already on the PVC
	// stay byte-identical until an admin actually fills one in — a record
	// without them round-trips through load/save unchanged.
	//
	// These are operator-entered free text rendered into the dashboard, so
	// every render path must escape them and every write path must cap them
	// (see maxContactNameLen / maxContactSlackIDLen / maxContactNotesLen).
	FullName string `json:"full_name,omitempty"`
	SlackID  string `json:"slack_id,omitempty"`
	Notes    string `json:"notes,omitempty"`
	// Company is ADMIN-entered CRM free text — the user's company/organization.
	// Like FullName/SlackID/Notes it is operator-maintained (never asserted by a
	// login) and is deliberately NOT collected in the hive request/provision
	// form; the operator fills it in manually from the admin Users table. Same
	// escaping + length-cap discipline as the other contact fields
	// (maxContactCompanyLen); omitempty so existing records round-trip
	// byte-identical until an admin sets it.
	Company string `json:"company,omitempty"`

	// Country is an OPTIONAL ISO 3166-1 alpha-2 code (uppercase, e.g. "GB"),
	// rendered as a small flag beside the user's avatar. Two sources, in
	// priority order: the explicit dropdown in the get-started wizard (copied
	// here on approval, like FullName/SlackID above), else a best-effort
	// inference from the Accept-Language region subtag at login, which only
	// ever fills an EMPTY value. See user_country.go for the full rationale and
	// the privacy posture.
	//
	// Stored as the code, never as the glyph: the flag is derived at render
	// time from regional-indicator code points, so no external image host is
	// involved and an unknown country renders nothing at all.
	//
	// omitempty so the thousands of existing records on the PVC round-trip
	// byte-identical until a user actually picks a country or logs in from a
	// browser that states a region.
	Country string `json:"country,omitempty"`

	// CountrySetByUser records that the country above was chosen DELIBERATELY
	// by the user rather than inferred, and it is what makes an explicit CLEAR
	// stick.
	//
	// Without it, "explicit" is inferred from `Country != ""` (see
	// applyInferredCountry), which is fine for a pick but wrong for a clear: a
	// user who removes their country via the self-service endpoint leaves an
	// empty field, and the very next login's Accept-Language inference would
	// silently put a flag back. "Prefer not to say" would become impossible to
	// express — and impossible to notice failing, since the flag reappears a
	// login later, far from the action that was supposed to remove it.
	//
	// Set only by the user's own writes: the self-service endpoint
	// (handleMyCountry) and the wizard pick copied on approval
	// (applyRequestContactToUser). NEVER set by the login-path inference, which
	// is precisely the distinction this field exists to draw.
	//
	// omitempty bool so every record that has not been through a deliberate
	// pick — which today is all of them — round-trips byte-identical.
	//
	// STILL WRITTEN, not deprecated: CountrySource below is the finer-grained
	// successor, but this boolean is what other readers and every record
	// already on the PVC speak, so every user-chosen write keeps setting it.
	CountrySetByUser bool `json:"country_set_by_user,omitempty"`

	// CountrySource is the PROVENANCE of the country above — who put it there.
	// One of countrySourceInferred / countrySourceAdmin / countrySourceUser, or
	// "" for a record nothing has ever touched.
	//
	// A boolean stopped being enough the moment an ADMIN could assign a country
	// on someone else's behalf, because that is a third kind of claim and it
	// sits BETWEEN the two the boolean can express:
	//
	//   - It is not user-chosen. Stamping CountrySetByUser for an admin edit
	//     would assert the user made a statement about themselves that they
	//     never made, and — since that marker is also what suppresses ever
	//     asking again — would permanently silence the question for them.
	//   - But it must still outrank Accept-Language inference. An admin's
	//     best-effort attribution is a human looking at evidence; the header is
	//     a language preference. Letting the next login overwrite it would
	//     re-introduce, in a new form, exactly the silent-clobber bug #4374 was
	//     opened to fix.
	//
	// Precedence, strongest first: user > admin > inferred > unset. See
	// countryProvenanceRank and mayOverwriteCountry in user_country.go, which
	// are the single arbiters — no caller compares these strings by hand.
	//
	// BACKWARD COMPATIBILITY. Records written before this field exists carry
	// only CountrySetByUser, so an ABSENT source is read through that boolean:
	// CountrySetByUser=true with no source means user-chosen (see
	// effectiveCountrySource). That is why this is omitempty and why nothing
	// backfills it — an untouched record must still serialize byte-identically.
	CountrySource string `json:"country_source,omitempty"`

	// Engagement stats, admin-only (they ride /api/saas/admin/users, which is
	// requireAdmin). Both omitempty ints so existing records round-trip
	// byte-identical until the user first logs in / opens a hive after this ships.
	//
	// LoginCount is the number of completed hub OAuth logins. Incremented in
	// exactly one place — handleOAuthCallback — never in ensureSaaSUser, whose
	// other callers (my-hives poll, admin provisioning) would inflate it.
	LoginCount int `json:"login_count,omitempty"`
	// SessionSeconds is the cumulative time this user has had at least one live
	// session on a hive dashboard, accumulated by the hub from the spoke's
	// per-heartbeat active-session report (see handleHeartbeat). It is a sampled
	// sum of inter-beat intervals, so it is accurate to roughly the beat interval,
	// not to the second.
	SessionSeconds int64 `json:"session_seconds,omitempty"`
	// EngagedSeconds is the honest subset of SessionSeconds: time accumulated
	// only on beats where the user's browser reported ENGAGED presence — tab
	// visible AND input within the idle window (heartbeat EngagedSessionUsers).
	// An idle open tab grows SessionSeconds but never this. omitempty, and
	// absent on records that predate the field or whose spokes don't report
	// presence yet — absence means NO DATA, never "provably unengaged".
	EngagedSeconds int64 `json:"engaged_seconds,omitempty"`
	// LastEngagedAt is the RFC3339 time of the most recent beat that credited
	// EngagedSeconds — when a human was last actually behind this user's
	// session. Feeds the `active` status tier. Same absence semantics as
	// EngagedSeconds.
	LastEngagedAt string `json:"last_engaged_at,omitempty"`
	// LastActionAt is the RFC3339 time of the user's most recent REAL audited
	// action on any of their hives (config save, agent restart, ACMM change,
	// login, …), folded hub-ward from the spoke audit logs (heartbeat
	// UserLastActions) keeping the per-user maximum. Same absence semantics.
	LastActionAt string `json:"last_action_at,omitempty"`
}

// Length caps for the admin-editable contact fields. These are free text
// written straight to the PVC, so each is bounded independently rather than
// relying on the request-body cap alone: name and Slack ID are identifiers and
// stay short, while notes is the running CRM log for a user and gets the most
// room. Values over the cap are truncated (not rejected) so a long paste still
// saves something useful instead of silently failing.
const (
	// maxContactNameLen bounds a person's full name. Generous versus real
	// names so non-Latin scripts and long multi-part names still fit.
	maxContactNameLen = 128
	// maxContactSlackIDLen bounds a Slack member ID or handle. Real Slack IDs
	// are ~11 chars (U01ABCDEF23); the headroom allows an @handle or a
	// workspace-qualified form.
	maxContactSlackIDLen = 64
	// maxContactCompanyLen bounds the company/organization name — an identifier
	// like the name/Slack fields, sized generously for long legal entity names.
	maxContactCompanyLen = 128
	// maxContactNotesLen bounds the free-text notes field — the longest of the
	// three, sized for a few paragraphs of admin scratch notes per user.
	maxContactNotesLen = 8192
	// maxUpdateUserBodyBytes caps the PUT body for the admin user-update
	// endpoint. Comfortably above the sum of the field caps plus JSON
	// overhead/escaping, and small enough that the endpoint can never be used
	// to push a large blob at the PVC.
	maxUpdateUserBodyBytes = 64 * 1024
)

// truncateRunes clips s to at most max runes. It counts runes rather than
// bytes so a cap never splits a multi-byte character (which would write
// invalid UTF-8 into the user record and then into the dashboard HTML).
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

var hmacKeyPath = "/data/saas/hmac.key"

const hmacKeySize = 32

func loadOrCreateHMACKey() ([]byte, error) {
	// Best-effort: a failed mkdir surfaces via the WriteFile error below.
	_ = os.MkdirAll(filepath.Dir(hmacKeyPath), 0o755)
	if data, err := os.ReadFile(hmacKeyPath); err == nil && len(data) == hmacKeySize {
		return data, nil
	}
	key := make([]byte, hmacKeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(hmacKeyPath, key, 0o600); err != nil {
		return nil, fmt.Errorf("write HMAC key: %w", err)
	}
	return key, nil
}

func encryptToken(plaintext string) (string, error) {
	key, err := loadOrCreateHMACKey()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func decryptToken(encoded string) (string, error) {
	key, err := loadOrCreateHMACKey()
	if err != nil {
		return "", err
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}
	plaintext, err := gcm.Open(nil, data[:nonceSize], data[nonceSize:], nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func (s *HubServer) registerSaaSRoutes() {
	s.mux.HandleFunc("GET /dashboard", s.handleDashboard)
	s.mux.HandleFunc("GET /access-denied", s.handleAccessDenied)
	s.mux.HandleFunc("GET /api/saas/my-hives", s.requireAuth(s.handleMyHives))
	// Per-release image-pulls series, headline (active release line) plus
	// per-line (external adoption gauge). requireAuth,
	// not requireAdmin: any signed-in hub user sees the same public-adoption
	// number, and the underlying data is scraped from the PUBLIC package page.
	s.mux.HandleFunc("GET /api/hub/image-pulls", s.requireAuth(s.handleImagePulls))
	// Token attribution rollups. requireAuth (not requireAdmin) because a
	// non-admin legitimately sees their OWN hives' usage; the handler scopes
	// fleet-wide data to admins itself.
	s.mux.HandleFunc("GET /api/saas/usage", s.requireAuth(s.handleUsage))
	// Self-service country: the ONE field a non-admin may write on their own
	// user record. requireAuth, not requireAdmin — that is the entire point,
	// since every other SaaSUser write is admin-gated and the wizard is a
	// one-time surface. The handler resolves the acting user from the SESSION
	// and the body carries no identity, so this cannot reach anyone else's
	// record. See handleMyCountry in user_country.go.
	//
	// PUT with a JSON body rather than a code in the path: country is personal
	// data and a URL would put it in access logs, Referer headers and history.
	s.mux.HandleFunc("GET /api/saas/me/country", s.requireAuth(s.handleMyCountry))
	s.mux.HandleFunc("PUT /api/saas/me/country", s.requireAuth(s.handleMyCountry))
	s.mux.HandleFunc("POST /api/saas/lite/enroll", s.requireAuth(s.handleLiteEnroll))
	s.mux.HandleFunc("POST /api/saas/hives", s.requireAuth(s.handleCreateHive))
	s.mux.HandleFunc("GET /api/saas/hives/{id}/status", s.requireAuth(s.handleHiveStatus))
	// /open is a browser NAVIGATION endpoint (the SSO handoff), not an API call.
	// It is registered WITHOUT requireAuth so an unauthenticated visit redirects
	// to the hub login (and back) instead of dumping a raw {"error":...} JSON.
	// handleOpenHive does its own auth check + login redirect.
	s.mux.HandleFunc("GET /api/saas/hives/{id}/open", s.handleOpenHive)
	s.mux.HandleFunc("DELETE /api/saas/hives/{id}", s.requireAuth(s.handleDeleteHive))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/upgrade", s.requireAuthOrSpokeUpgrade(s.handleUpgradeHive))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/switch-branch", s.requireAuth(s.handleSwitchBranch))
	s.mux.HandleFunc("PUT /api/saas/hives/{id}/visibility", s.requireAuth(s.handleToggleVisibility))
	s.mux.HandleFunc("PUT /api/saas/hives/{id}/auto-upgrade", s.requireAuth(s.handleToggleAutoUpgrade))
	// Rename a hive's display name (its ProjectName). requireAuth plus an inner
	// owner-or-admin check, exactly like visibility/auto-upgrade above — the
	// gate is the security boundary, not just the hidden UI affordance.
	s.mux.HandleFunc("PUT /api/saas/hives/{id}/name", s.requireAuth(s.handleRenameHive))
	// Move a hive between forges (github.com <-> a GitHub Enterprise host).
	// requireAuth plus an inner owner-or-admin check, exactly like
	// switch-branch and auto-upgrade above.
	s.mux.HandleFunc("POST /api/saas/hives/{id}/forge", s.requireAuth(s.handleSwitchForge))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/reset-app", s.requireAuth(s.handleResetApp))
	// Assigns a hive its OPTIONAL second GitHub App (#4815). requireAuth is the
	// same outer gate reset-app uses; the handler itself re-checks isHubAdmin,
	// which is the authoritative check for both.
	s.mux.HandleFunc("PUT /api/saas/hives/{id}/secondary-app", s.requireAuth(s.handleSetHiveSecondaryApp))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/restart-spoke", s.requireAuth(s.handleRestartSpoke))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/agents/{agent}/restarts/reset", s.requireAuth(s.handleResetAgentRestarts))
	s.mux.HandleFunc("GET /api/saas/hive-config/{hiveID}", s.requireAuth(s.handleProxyHiveConfig))
	s.mux.HandleFunc("GET /api/saas/latest-sha", s.handleLatestSHA)
	s.mux.HandleFunc("POST /api/saas/hub/upgrade", s.requireAdmin(s.handleHubSelfUpgrade))
	s.mux.HandleFunc("PUT /api/saas/hub/auto-upgrade", s.requireAdmin(s.handleHubAutoUpgrade))
	// Admin upgrade kill switch (upgrade_pause.go): pause hub self-upgrades
	// and/or ALL automatic spoke image changes, fleet-wide.
	s.mux.HandleFunc("GET /api/saas/upgrade-pause", s.requireAdmin(s.handleGetUpgradePause))
	s.mux.HandleFunc("POST /api/saas/upgrade-pause", s.requireAdmin(s.handleSetUpgradePause))
	s.mux.HandleFunc("GET /api/saas/auth-check", s.handleSaaSAuthCheck)
	// Sibling-product identity bridge (#4171): dibs.kubestellar.io forwards the
	// browser's hive_hub_user cookie here server-to-server to resolve the
	// session. Registered GET-only via the method pattern, and WITHOUT
	// requireAuth so the unauthenticated answer is the exact 401 JSON shape the
	// dibs bridge expects rather than the generic middleware error.
	s.mux.HandleFunc("GET /api/saas/whoami", s.handleSaaSWhoami)
	// Sibling-product repo registry (#4193): dibs polls this server-to-server
	// (no session) every ~5 minutes to learn which repos hives manage. Public
	// by design, so it returns ONLY already-public data — see handleDibsRepos.
	s.mux.HandleFunc("GET /api/saas/dibs/repos", s.handleDibsRepos)
	s.mux.HandleFunc("POST /api/saas/user-token", s.requireAuth(s.handleUserToken))
	s.mux.HandleFunc("GET /api/saas/hives/{id}/access", s.requireAuth(s.handleAccessList))
	s.mux.HandleFunc("GET /api/saas/grantable-users", s.requireAuth(s.handleGrantableUsers))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/access", s.requireAuth(s.handleAccessAdd))
	s.mux.HandleFunc("DELETE /api/saas/hives/{id}/access/{username}", s.requireAuth(s.handleAccessRemove))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/request-access", s.requireAuth(s.handleRequestAccess))
	s.mux.HandleFunc("GET /api/saas/hives/{id}/requests", s.requireAuth(s.handleGetRequests))
	s.mux.HandleFunc("GET /api/saas/hives/{id}/timeline", s.requireAuth(s.handleHiveTimeline))
	s.mux.HandleFunc("GET /api/saas/hives/{id}/access-log", s.requireAuth(s.handleAccessLog))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/requests/{username}/approve", s.requireAuth(s.handleApproveRequest))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/requests/{username}/deny", s.requireAuth(s.handleDenyRequest))
	s.mux.HandleFunc("PUT /api/saas/hives/{id}/approve-access/{username}", s.requireAuth(s.handleApproveAccess))
	s.mux.HandleFunc("DELETE /api/saas/hives/{id}/deny-access/{username}", s.requireAuth(s.handleDenyAccess))
	s.mux.HandleFunc("GET /api/saas/access-status", s.handleAccessStatus)
	// GET /api/saas/repos is deliberately gone. Repo discovery ran on the
	// requester's github.com OAuth token, so it could only ever see public
	// GitHub — invisible to GitHub Enterprise, and structurally unable to list
	// a GitLab or Gitea repo when those forges arrive. The request flow now
	// takes a typed repository URL, which works for every forge without new
	// code, and that is what let the login drop to an empty OAuth scope.
	// Restoring this endpoint would also require restoring that scope.
	s.mux.HandleFunc("POST /api/saas/request-provision", s.requireAuth(s.handleRequestProvision))
	s.mux.HandleFunc("PUT /api/saas/approve-provision/{username}", s.requireAdmin(s.handleApproveProvision))
	s.mux.HandleFunc("DELETE /api/saas/deny-provision/{username}", s.requireAdmin(s.handleDenyProvision))
	s.mux.HandleFunc("GET /api/saas/admin/available-placeholders", s.requireAdmin(s.handleAvailablePlaceholders))
	s.mux.HandleFunc("GET /api/saas/admin/scale-settings", s.requireAdmin(s.handleGetScaleSettings))
	s.mux.HandleFunc("POST /api/saas/admin/scale-settings", s.requireAdmin(s.handleSetScaleSettings))
	s.mux.HandleFunc("GET /api/saas/admin/users", s.requireAdmin(s.handleAdminUsers))
	// Aggregate geographic rollup of the user base (counts only, no usernames).
	// Admin-gated like the rest of the CRM/Users surface: country is personal
	// data, so even the aggregate stays behind requireAdmin. Takes no query
	// parameters — no country ever appears in a URL. See user_country_rollup.go.
	s.mux.HandleFunc("GET /api/saas/admin/user-countries", s.requireAdmin(s.handleAdminUserCountries))
	// #3234: fleet readiness for removing the N1/N2 legacy compatibility lanes.
	s.mux.HandleFunc("GET /api/saas/admin/auth-rollout", s.requireAdmin(s.handleAuthRollout))
	// Master-secret rotation (src/docs/design/master-key-rotation.md). Both are
	// requireAdmin, which enforces isCSRFSafe BEFORE resolving identity — an
	// ambient hub session cookie would otherwise make a cross-site POST able to
	// rotate the fleet's master key. The rotate route is a POST for that reason
	// too: isCSRFSafe exempts safe methods.
	s.mux.HandleFunc("GET /api/saas/admin/key-generations", s.requireAdmin(s.handleKeyGenerations))
	s.mux.HandleFunc("POST /api/saas/admin/rotate-master-key", s.requireAdmin(s.handleRotateMasterKey))
	s.mux.HandleFunc("PUT /api/saas/admin/users/{username}", s.requireAdmin(s.handleAdminUpdateUser))
	s.mux.HandleFunc("DELETE /api/saas/admin/users/{username}", s.requireAdmin(s.handleAdminDeleteUser))
	// Admin read-only "View as user" impersonation. Enter is admin-only and
	// sets the short-lived signed hive_hub_impersonate cookie; exit clears it
	// and is exempt from the impersonation write-block (see impersonateExitPath)
	// so the admin can always get back out. Status folds into /api/auth/user for
	// the banner, but a dedicated read is offered too.
	s.mux.HandleFunc("POST /api/saas/admin/impersonate/exit", s.requireAdmin(s.handleImpersonateExit))
	s.mux.HandleFunc("POST /api/saas/admin/impersonate/{username}", s.requireAdmin(s.handleImpersonateStart))
	s.mux.HandleFunc("GET /api/saas/impersonation-status", s.requireAuth(s.handleImpersonationStatus))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/assign", s.requireAuth(s.handleAssignHive))
	// Escape hatch: return an assigned-but-unclaimed placeholder to the available
	// pool so it can be re-armed. Admin-only, and guarded to the wedged middle
	// state (assigned && !claim_delivered) inside the handler.
	s.mux.HandleFunc("POST /api/saas/hives/{id}/reset-assignment", s.requireAdmin(s.handleResetAssignment))
	s.mux.HandleFunc("GET /api/saas/cluster-health", s.requireAdmin(s.handleClusterHealth))
	// PR reach telemetry (#3994): the read-only join of merged PRs against
	// the commits/components the fleet reports actually running. The payload
	// names hives fleet-wide — the same exposure class as cluster-health
	// directly above, so the same requireAdmin gate.
	s.mux.HandleFunc("GET /api/reach", s.requireAdmin(s.handleReach))
	// Advisory-staleness diagnostics (#4167): the read-only fleet view of WHICH
	// gate decided each hive's advisory verdict, and how many stale digests no
	// pill is reporting. Names hives fleet-wide with their App state, so the
	// same requireAdmin gate as cluster-health and /api/reach above.
	s.mux.HandleFunc("GET /api/saas/admin/advisory-diagnostics", s.requireAdmin(s.handleAdvisoryDiagnostics))
	// Acknowledging a fleet alert is an operator action on the operator's own
	// view, so it is admin-only (see alerts.go).
	s.mux.HandleFunc("POST /api/saas/admin/alert-ack", s.requireAdmin(s.handleAlertAck))
	s.mux.HandleFunc("GET /api/hub/clusters", s.requireAuth(s.handleListClusters))
	// Per-cluster GitHub App key store. The GET is fingerprints only (never key
	// material); the PUT is the single write-only entry point for a key.
	s.mux.HandleFunc("GET /api/saas/admin/cluster-app-keys", s.requireAdmin(s.handleGetClusterAppKeys))
	s.mux.HandleFunc("PUT /api/saas/admin/cluster-app-keys/{clusterID}", s.requireAdmin(s.handlePutClusterAppKey))
	s.mux.HandleFunc("POST /api/saas/admin/hub-banner", s.requireAdmin(s.handleSendHubBanner))
	s.mux.HandleFunc("DELETE /api/saas/admin/hub-banner", s.requireAdmin(s.handleClearHubBanner))
	s.mux.HandleFunc("GET /api/saas/admin/hub-banner", s.requireAdmin(s.handleGetHubBanner))
	s.registerBulkRoutes()
	// Slack messaging. The single-user and hive-owner routes are admin-or-owner
	// (checked inside each handler, like switch-branch); the BROADCAST is
	// admin-only, because it reaches every user with a slack_id and cannot be
	// recalled. It additionally requires a typed confirmation and offers a dry
	// run — see slack.go.
	s.mux.HandleFunc("POST /api/saas/slack/user/{username}", s.requireAuth(s.handleSlackMessageUser))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/slack", s.requireAuth(s.handleSlackMessageHiveOwner))
	s.mux.HandleFunc("POST /api/saas/admin/slack/broadcast", s.requireAdmin(s.handleSlackBroadcast))
	s.mux.HandleFunc("POST /api/saas/admin/journey-snooze", s.requireAdmin(s.handleJourneySnooze))
	s.mux.HandleFunc("GET /api/saas/admin/journey-status", s.requireAdmin(s.handleJourneyStatus))
}

// impersonateExitPath is the one mutating endpoint that stays callable while
// impersonation is active — it is how the admin gets OUT. Every other write is
// refused 403 by the write-block below.
const impersonateExitPath = "/api/saas/admin/impersonate/exit"

// blockIfImpersonatingWrite enforces the read-only property of impersonation.
// While a valid admin grant is active, ANY non-GET/HEAD request (except the
// exit endpoint) is refused 403 before it reaches its handler. This is the
// central write gate: it lives in both requireAuth and requireAdmin, through
// which every user-facing mutation is routed, so no write path can slip past.
// It returns true when it has already written the 403 response and the caller
// must stop.
func (s *HubServer) blockIfImpersonatingWrite(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return false
	}
	if r.URL.Path == impersonateExitPath {
		return false
	}
	_, _, impersonating := s.resolveIdentity(r)
	if !impersonating {
		return false
	}
	target := "user"
	if grant, ok := s.activeImpersonationGrant(r); ok {
		target = grant.Target
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	// target is a GitHub login (no quotes/backslashes possible), safe to inline.
	_, _ = w.Write([]byte(`{"error":"read-only while viewing as ` + target + ` — exit impersonation to make changes"}`))
	return true
}

// activeImpersonationGrant returns the verified grant behind the current
// request when (and only when) resolveIdentity would honor it. It is a thin
// read used for messaging/audit/status; the security decisions live in
// resolveIdentity.
func (s *HubServer) activeImpersonationGrant(r *http.Request) (impersonationGrant, bool) {
	if !isHubAdmin(s.getRealAuthUser(r)) {
		return impersonationGrant{}, false
	}
	cookie, err := r.Cookie(impersonateCookieName)
	if err != nil || cookie.Value == "" {
		return impersonationGrant{}, false
	}
	grant, _, ok := verifyImpersonateCookieValueWithGenerations(s.currentGenerations(), cookie.Value, time.Now())
	if !ok || !isHubAdmin(grant.Admin) {
		return impersonationGrant{}, false
	}
	if loadSaaSUser(grant.Target) == nil {
		return impersonationGrant{}, false
	}
	return grant, true
}

func (s *HubServer) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isCSRFSafe(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"CSRF check failed"}`))
			return
		}
		if s.blockIfImpersonatingWrite(w, r) {
			return
		}
		username := s.getAuthUser(r)
		if username == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"not authenticated"}`))
			return
		}
		user := loadSaaSUser(username)
		if user == nil {
			ensureSaaSUser(username)
			user = loadSaaSUser(username)
		}
		if user == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unknown user — please log in again"}`))
			return
		}
		if user.Blocked {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"account blocked"}`))
			return
		}
		next(w, r)
	}
}

// requireAuthOrSpokeUpgrade accepts the normal hub session for hub-dashboard
// clicks and, for a hosted spoke dashboard, the spoke's server-to-server proof
// plus the already-authenticated operator identity injected by that spoke.
func (s *HubServer) requireAuthOrSpokeUpgrade(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isCSRFSafe(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"CSRF check failed"}`))
			return
		}
		if s.blockIfImpersonatingWrite(w, r) {
			return
		}
		username := s.getAuthUser(r)
		if username == "" {
			spokeUser, reason := s.trustedSpokeUpgradeUser(r, r.PathValue("id"))
			if spokeUser != "" {
				next(w, r)
				return
			}
			// Honest-error standard (#4446): every rejection on the spoke lane
			// names WHICH credential failed and what to do about it, because the
			// spoke dashboard relays this body verbatim into the operator's
			// toast — a bare "not authenticated" told a logged-in owner nothing.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": reason})
			return
		}
		user := loadSaaSUser(username)
		if user == nil {
			ensureSaaSUser(username)
			user = loadSaaSUser(username)
		}
		if user == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unknown user — please log in again"}`))
			return
		}
		if user.Blocked {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"account blocked"}`))
			return
		}
		next(w, r)
	}
}

// trustedSpokeUpgradeUser authenticates the spoke-relayed upgrade lane. It
// returns the user to attribute the upgrade to and an empty reason on success,
// or ("", reason) on failure, where reason is an operator-facing explanation of
// exactly which credential failed (the spoke shows it verbatim in a toast).
//
// The proof (X-Hive-Proxy-Auth = the hive's own dashboard token) is the
// load-bearing credential: possession of that per-hive secret already grants
// owner on the spoke dashboard itself, so a proof-verified request is the
// spoke's authenticated operator by construction. X-Hive-User is attribution,
// not authentication — gateway-fronted spokes (the Node auth proxy on :3001)
// authenticate the operator with the shared token, strip per-user identity
// headers, and therefore relay NO X-Hive-User even for a legitimate logged-in
// owner. Rejecting that shape was the "Upgrade failed: not authenticated" bug:
// a proof-verified request with no user identity is now attributed to the
// hive's registered owner instead of being turned away.
func (s *HubServer) trustedSpokeUpgradeUser(r *http.Request, hiveID string) (string, string) {
	username := r.Header.Get("X-Hive-User")
	proof := r.Header.Get(proxyAuthHeader)
	if hiveID == "" {
		return "", "not authenticated — upgrade request named no hive"
	}
	if username == "" && proof == "" {
		// Nothing to verify at all. Old spoke builds (pre proof-forwarding)
		// relay the upgrade click with no credentials whatsoever; tell the
		// operator how to upgrade past that build instead of a dead end.
		return "", "not authenticated — this upgrade request reached the hub with no hub session and no spoke credentials; if it came from a spoke dashboard, that spoke build is too old to relay its upgrade credentials — trigger this hive's upgrade from the hub dashboard (or enable auto-upgrade), after which the spoke button will work"
	}
	if r.Header.Get("X-Hive-Role") != saasRoleOwner {
		return "", "not authenticated — spoke upgrade requests must carry the owner role"
	}
	if proof == "" {
		return "", "not authenticated — spoke upgrade proof missing: the spoke sent no dashboard-token proof (X-Hive-Proxy-Auth); set DASHBOARD_AUTH_TOKEN on the spoke to its hive-secrets/dashboard-token value"
	}
	switch s.verifySpokeUpgradeProof(hiveID, proof) {
	case spokeProofOK:
		// verified — fall through to attribution below
	case spokeProofUnverifiable:
		return "", "not authenticated — the hub has no stored dashboard-token record for this hive and could not read its hive-secrets/dashboard-token secret (the hive's cluster is unreachable from the hub, e.g. pull-only); a spoke on a current build reports its token over the authenticated heartbeat — trigger this hive's upgrade from the hub dashboard once, and the spoke's Upgrade button will verify against the stored record from then on"
	default: // spokeProofMismatch
		return "", "not authenticated — spoke upgrade proof rejected: the spoke's DASHBOARD_AUTH_TOKEN does not match this hive's dashboard-token secret; re-sync the spoke's token"
	}
	if username == "" {
		// Proof verified but no per-user identity (shared-token gateway
		// topology): attribute the upgrade to the hive's registered owner.
		if h := loadSaaSHive(hiveID); h != nil {
			username = h.Owner
		}
		if username == "" {
			return "", "not authenticated — spoke upgrade request carried no user identity and this hive has no registered owner to attribute it to"
		}
	}
	user := loadSaaSUser(username)
	if user == nil {
		return "", fmt.Sprintf("not authenticated — the hub has no record of user %q; log in to the hub once, then retry", username)
	}
	if user.Blocked {
		return "", "not authenticated — this account is blocked on the hub"
	}
	return username, ""
}

// spokeProofVerdict is the outcome of verifying a spoke's dashboard-token
// upgrade proof. The three-way split exists for the honest-error chain:
// "your token is wrong" and "the hub cannot check any token" demand different
// operator actions and must never share one message.
type spokeProofVerdict int

const (
	spokeProofOK spokeProofVerdict = iota
	// spokeProofMismatch: at least one reference credential was available and
	// the presented proof matched none of them.
	spokeProofMismatch
	// spokeProofUnverifiable: the hub has NO reference to check against — no
	// stored DashboardTokenHash record and no readable secret (pull-only or
	// otherwise unreachable cluster).
	spokeProofUnverifiable
)

// verifySpokeUpgradeProof checks a spoke-relayed upgrade proof against, in
// order:
//
//  1. The hub's OWN stored record (SaaSHive.DashboardTokenHash — written at
//     provisioning when the hub mints the token, refreshed from the spoke's
//     authenticated heartbeat). This needs no cluster access at all, which is
//     the point: hosted spokes on pull-only clusters (e.g. fmaas) are reached
//     only by their outbound heartbeat, and requiring a live kubectl secret
//     read there made every proof unverifiable by design.
//  2. A live read of the hive's hive-secrets/dashboard-token secret
//     (spokeProxyAuthToken, cached) — the pre-existing lane, kept as fallback
//     for hives that predate the stored record on clusters the hub CAN reach.
//     A successful live read that matches also backfills the stored record, so
//     the next verification (and a later loss of cluster access) no longer
//     depends on the cluster. A rotation the stored record missed is adopted
//     the same way: stale hash, live read matches, record refreshed.
func (s *HubServer) verifySpokeUpgradeProof(hiveID, proof string) spokeProofVerdict {
	verifiable := false
	if h := loadSaaSHive(hiveID); h != nil && h.DashboardTokenHash != "" {
		verifiable = true
		if secureCompareHub(HashDashboardToken(proof), h.DashboardTokenHash) {
			return spokeProofOK
		}
	}
	if expected := s.spokeProxyAuthToken(hiveID); expected != "" {
		verifiable = true
		if secureCompareHub(proof, expected) {
			if h := loadSaaSHive(hiveID); h != nil {
				if hash := HashDashboardToken(expected); h.DashboardTokenHash != hash {
					h.DashboardTokenHash = hash
					_ = saveSaaSHive(h)
				}
			}
			return spokeProofOK
		}
	}
	if !verifiable {
		return spokeProofUnverifiable
	}
	return spokeProofMismatch
}

// adoptSpokeDashboardTokenHash folds a heartbeat-reported dashboard-token hash
// into the hive's stored record (see HeartbeatPayload.DashboardTokenHash for
// why the spoke reports it, and verifySpokeUpgradeProof for what reads it).
// Callers must have authenticated the beat's per-hive bearer first. An empty
// or malformed value changes nothing: old spokes and token-less spokes report
// nothing, and the hub must keep whatever record it already has.
func (s *HubServer) adoptSpokeDashboardTokenHash(payload *HeartbeatPayload) {
	reported := payload.DashboardTokenHash
	if reported == "" || !isHexSHA256(reported) {
		return
	}
	if !strings.HasPrefix(payload.HiveID, "hosted-") && !strings.HasPrefix(payload.HiveID, "saas-") {
		return
	}
	h := loadSaaSHive(payload.HiveID)
	if h == nil || h.DashboardTokenHash == reported {
		return
	}
	h.DashboardTokenHash = reported
	if err := saveSaaSHive(h); err != nil {
		s.logger.Warn("failed to store heartbeat-reported dashboard token hash", "hive_id", payload.HiveID, "error", err)
	}
}

// isHexSHA256 reports whether s is a well-formed lowercase-or-uppercase hex
// SHA-256 digest — 64 hex characters, the only shape adoptSpokeDashboardTokenHash
// will persist.
func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// isCSRFSafe reports whether a request may be allowed to MUTATE state.
//
// AUDIT F4 (replay half). This used to end with
//
//	return strings.Contains(ct, "application/json")
//
// which meant a mutation carrying NEITHER Origin NOR Referer was treated as
// safe as long as it said `Content-Type: application/json`. That fails OPEN, and
// the Content-Type is not a defence: it is fully attacker-controlled, and — the
// part that actually matters — Origin is absent in exactly the cases CSRF cares
// about. A browser omits Origin on plenty of navigations and redirect-issued
// requests, and Referer is routinely stripped by
// `Referrer-Policy: no-referrer`, privacy extensions, and corporate proxies. So
// "no headers" was never a reliable signal of "not a browser"; it was a signal
// of "we cannot tell", and the old code resolved that ambiguity in the
// attacker's favour on every requireAuth and requireAdmin write.
//
// It now fails CLOSED: a mutation must positively demonstrate it is either
// same-origin or not-a-browser. There are exactly two ways to do that.
//
//  1. Origin (preferred) or Referer matches the hub. Browsers attach Origin to
//     every cross-origin mutation and cannot forge it from script, which is what
//     makes it load-bearing.
//
//  2. The request authenticates with `Authorization: Bearer …` and sends NO
//     session cookie. This is the explicit non-browser lane, and it is safe for
//     a structural reason rather than a stylistic one: CSRF is an AMBIENT
//     credential attack. A cross-site request rides the cookie the browser
//     attaches automatically; it cannot attach an Authorization header, because
//     setting one from script requires CORS permission the hub never grants. A
//     request whose ONLY credential is a bearer token therefore cannot be
//     cross-site forged — the attacker would need the token itself, at which
//     point CSRF is irrelevant.
//
//     The "no session cookie" half is not optional. If a request carrying the
//     ambient cookie could opt out of the CSRF check merely by ALSO presenting a
//     header, then an attacker who can get any header set (or a stale token
//     lying in a client) re-opens the hole. Cookie present ⇒ ambient credential
//     ⇒ Origin must be proven.
//
// See getRealAuthUser: Bearer is already a first-class authentication path here,
// so this is not a new trust surface, only an explicit statement of which lane a
// caller is using.
func isCSRFSafe(r *http.Request) bool {
	if r.Method == "GET" || r.Method == "HEAD" || r.Method == "OPTIONS" {
		return true
	}
	// Origin first: it is present on cross-origin browser mutations and cannot
	// be spoofed by page script.
	if origin := r.Header.Get("Origin"); origin != "" {
		return isSameOriginAsHub(origin)
	}
	if referer := r.Header.Get("Referer"); referer != "" {
		return isSameOriginAsHub(referer)
	}
	// No origin information. The ONLY remaining way to be safe is to prove this
	// is not an ambient-credential browser request.
	return isNonBrowserAPIRequest(r)
}

// isNonBrowserAPIRequest reports whether a request authenticates purely with a
// bearer token and carries no ambient session cookie — the one shape that is
// structurally immune to CSRF. See isCSRFSafe for why both halves are required.
func isNonBrowserAPIRequest(r *http.Request) bool {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return false
	}
	// Any hub session cookie means an ambient credential is in play, and the
	// request must prove its origin like any other browser mutation. Checked by
	// presence, not by validity: an INVALID cookie still means a browser sent
	// one, and letting a bogus cookie value re-enable the bearer bypass would be
	// a trivially attacker-satisfiable condition.
	if c, err := r.Cookie("hive_hub_user"); err == nil && c.Value != "" {
		return false
	}
	return true
}

const (
	defaultHubPublicURL          = "https://hive.kubestellar.io"
	defaultHubCanonicalHost      = "hive.kubestellar.io"
	defaultHubSpokeDomain        = "hive.kubestellar.io"
	defaultLegacyHubCookieDomain = ".hive.kubestellar.io"
)

// hubPublicURL is the canonical public origin used to build absolute URLs.
func hubPublicURL() string {
	if v := strings.TrimSpace(os.Getenv("HIVE_HUB_PUBLIC_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultHubPublicURL
}

// oauthRedirectURI is the single OAuth/OIDC callback registered on every
// provider's side. All providers share one callback path; the state parameter
// carries which provider to complete against.
func oauthRedirectURI() string {
	return hubPublicURL() + "/api/auth/callback"
}

// hubCanonicalHost is the ONE host that serves the hub's own dashboard and API.
// Every legitimate browser mutation against the hub is issued by a document
// loaded from this host — tenant spokes are separate origins that talk to the
// hub through their own proxy, never by scripting a cross-origin POST at it.
func hubCanonicalHost() string {
	u, err := url.Parse(hubPublicURL())
	if err != nil || u.Hostname() == "" {
		return defaultHubCanonicalHost
	}
	return strings.ToLower(u.Hostname())
}

// hubSpokeDomain is the shared parent domain the hosted tenants live under
// (<id>.hive.kubestellar.io by default). It is a REDIRECT-trust boundary only
// — see isTrustedRedirectTarget — and deliberately NOT a CSRF or CORS boundary.
func hubSpokeDomain() string {
	if v := strings.TrimSpace(os.Getenv("HIVE_HUB_SPOKE_DOMAIN")); v != "" {
		return strings.TrimPrefix(strings.TrimSuffix(strings.ToLower(v), "."), ".")
	}
	return defaultHubSpokeDomain
}

func hubDomainSuffix() string {
	return "." + hubSpokeDomain()
}

func legacyHubCookieDomain() string {
	v := strings.TrimSpace(os.Getenv("HIVE_HUB_LEGACY_COOKIE_DOMAIN"))
	if v == "" {
		return ""
	}
	return "." + strings.TrimPrefix(strings.TrimSuffix(strings.ToLower(v), "."), ".")
}

func legacySessionCookieDomains(liveDomain string) []string {
	var domains []string
	if liveDomain != "" && liveDomain != defaultLegacyHubCookieDomain {
		domains = append(domains, defaultLegacyHubCookieDomain)
	}
	if legacy := legacyHubCookieDomain(); legacy != "" && legacy != liveDomain && legacy != defaultLegacyHubCookieDomain {
		domains = append(domains, legacy)
	}
	return domains
}

// sessionCookieParentDomain is the registrable parent domain the hub session
// cookie is scoped to, so first-party sibling products on other subdomains of it
// (dibs) receive the cookie and can SSO against the hub via GET /api/saas/whoami.
// Derived from hubCanonicalHost (its parent domain) rather than spelled out so
// the two can never disagree.
//
// That derivation is also the sibling bridge's precondition: the hub and the
// sibling must share a REGISTRABLE DOMAIN, whichever one it is. Moving the hub
// across registrable domains — hive.kubestellar.io to hive.hivecommons.dev —
// therefore signs users out of every sibling left behind on the old one, and no
// configuration rescues those siblings, because a browser ignores a Set-Cookie
// whose Domain does not cover the sending host (RFC 6265 5.3). The old sibling
// host has to REDIRECT to the new one; dual-serving both cannot work (#5925).
// Pinned by sibling_host_migration_test.go, explained in
// src/docs/hivecommons-migration.md.
func sessionCookieParentDomain() string {
	if parent, err := publicsuffix.EffectiveTLDPlusOne(hubCanonicalHost()); err == nil {
		return parent
	}
	if _, parent, ok := strings.Cut(hubCanonicalHost(), "."); ok {
		return parent
	}
	return hubCanonicalHost()
}

// sessionCookieDomain returns the Domain attribute the hub session cookie
// (hive_hub_user) must carry for a request served on host, or "" for a
// host-only cookie.
//
// Production (any host under the hub's own registrable domain, including the
// hub itself) gets Domain=.<that domain> — .kubestellar.io by default, or
// .hivecommons.dev once HIVE_HUB_PUBLIC_URL moves the hub there — so that BOTH
// consumers of the cookie receive it:
//   - every hosted spoke's Node proxy on <id>.hive.<domain>, which independently
//     verifies it for the tenant dashboard and terminal (the original reason the
//     cookie carried Domain=.hive.kubestellar.io); and
//   - sibling first-party products such as dibs, which read it and call back to
//     /api/saas/whoami (#4171) — but ONLY while the sibling lives under the same
//     registrable domain (#5925; see sessionCookieParentDomain).
//
// Every other host — localhost, 127.0.0.1, and any host outside that domain,
// which includes a sibling stranded on the PREVIOUS registrable domain — gets a
// host-only cookie: a browser rejects a Set-Cookie whose Domain does not cover
// the request host, so widening there would emit a cookie no browser stores and
// would break local login outright.
func sessionCookieDomain(host string) string {
	h := host
	if hp, _, err := net.SplitHostPort(host); err == nil {
		h = hp
	}
	parent := sessionCookieParentDomain()
	if h == parent || strings.HasSuffix(h, "."+parent) {
		return "." + parent
	}
	return ""
}

// hubSessionCookieValues returns every hive_hub_user value on the request, in
// jar order. During the .kubestellar.io domain-widening rollout (#4171) a
// browser may briefly hold TWO copies of the cookie — the legacy
// .hive.kubestellar.io-scoped one and the new parent-scoped one — and it sends
// both under the same name. Callers must try each candidate rather than
// trusting whichever copy the jar happens to order first, or a stale legacy
// cookie would shadow a fresh session (and vice versa) until re-login.
func hubSessionCookieValues(r *http.Request) []string {
	var vals []string
	for _, c := range r.Cookies() {
		if c.Name == "hive_hub_user" && c.Value != "" {
			vals = append(vals, c.Value)
		}
	}
	return vals
}

// isSameOriginAsHub reports whether raw names the hub's own origin, i.e. the
// only origin permitted to drive a state-changing request or to receive a
// credentialed CORS response.
//
// SECURITY (audit F4): this used to be isTrustedOrigin, which accepted EVERY
// suffix match of .hive.kubestellar.io. Because the hub session cookie is
// scoped Domain=.hive.kubestellar.io, the browser attaches it to requests
// issued from any sibling tenant — so a hostile hive operator, serving script
// from their own <id>.hive.kubestellar.io dashboard, could POST at the hub with
// the victim admin's ambient cookie and have the CSRF gate wave it through. The
// audit demonstrated exactly this by flipping another tenant's visibility from
// a sibling Origin. Suffix-matching a domain whose subdomains are handed out to
// untrusted third parties is not an origin check at all.
//
// localhost/127.0.0.1 stay trusted for local development, where the hub is
// served from those hosts and there is no multi-tenant sibling to speak of.
func isSameOriginAsHub(raw string) bool {
	host, ok := originHost(raw)
	if !ok {
		return false
	}
	return host == hubCanonicalHost() || host == "localhost" || host == "127.0.0.1"
}

// isTrustedRedirectTarget reports whether raw is a URL the hub may bounce a
// browser BACK to after login.
//
// This one MUST keep accepting sibling tenants, and that is not an oversight:
// every hosted hive's ingress carries
//
//	auth-signin: https://hive.kubestellar.io/login?redirect=$scheme://$http_host$request_uri
//
// (see saas_provision.go), so the ordinary "open my hive" flow arrives at the
// hub with redirect=https://<id>.hive.kubestellar.io/... and must be allowed to
// return there. Narrowing this to the exact hub origin would break sign-in for
// all hosted tenants.
//
// Sending a browser to a sibling is a far weaker capability than accepting a
// mutation FROM one: the tenant already controls that host and can navigate the
// user there unaided. The dangerous half — trusting a sibling to author a
// request — is what isSameOriginAsHub now refuses.
func isTrustedRedirectTarget(raw string) bool {
	host, ok := originHost(raw)
	if !ok {
		return false
	}
	return host == hubCanonicalHost() ||
		strings.HasSuffix(host, hubDomainSuffix()) ||
		host == "localhost" ||
		host == "127.0.0.1"
}

// originHost extracts the hostname from raw, rejecting values that do not parse.
// Shared by both trust predicates so they can never disagree about how a URL is
// decomposed.
func originHost(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	return u.Hostname(), true
}

// getRealAuthUser resolves the REAL authenticated user from the signed session
// cookie or a Bearer token, with NO impersonation applied. This is the identity
// the admin actually logged in as. Impersonation is layered on top of this by
// resolveIdentity — never inside it — so the write-block and the admin gate can
// always reason about who is really driving the request.
func (s *HubServer) getRealAuthUser(r *http.Request) string {
	// Every hive_hub_user copy is tried, not just the first (see
	// hubSessionCookieValues — the domain-widening rollout can leave two).
	for _, value := range hubSessionCookieValues(r) {
		// The cookie value is only trusted when its HMAC signature verifies
		// against the hub secret. A legacy unsigned cookie or a forged value
		// fails here and is treated as logged out, so the user re-authenticates
		// through the normal login flow (which re-mints a signed cookie).
		// N2: accept v2 (Ed25519) or, during rollout only, the legacy HMAC format.
		// F10: verifyHubUserCookie additionally enforces a v3 cookie's SIGNED
		// expiry and its revocation state, which MaxAge alone never did.
		if username, ok := s.verifyHubUserCookie(value); ok {
			if loadSaaSUser(username) != nil {
				return username
			}
		}
	}

	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		token := strings.TrimPrefix(auth, "Bearer ")
		if username := s.validateGitHubToken(token); username != "" {
			return username
		}
	}

	return ""
}

// resolveIdentity is the single decision point for admin "View as user"
// impersonation. It returns the EFFECTIVE identity every per-user view should
// render as, the REAL admin login driving the request, and whether an
// impersonation grant is currently active.
//
// Impersonation is honored ONLY when every one of these holds (fail closed on
// any miss — the request is then simply the real user, not impersonating):
//
//   - a hive_hub_impersonate cookie is present AND its HMAC verifies AND it has
//     not expired (verifyImpersonateCookieValue);
//   - the grant's Admin field equals the REAL signed session user;
//   - that real user is a hub admin (isHubAdmin — only an admin may impersonate
//     — a stolen cookie replayed on a non-admin session is ignored);
//   - the target resolves to a real registered user on disk.
//
// The effective identity switches to the target ONLY for GET/HEAD requests.
// For any mutating method the effective identity stays the admin so no write is
// ever attributed to the target; the write itself is separately refused 403 by
// requireAuth/requireAdmin. This split means impersonation can never elevate:
// the target is always a normal user, and writes never run under it.
func (s *HubServer) resolveIdentity(r *http.Request) (effective, realUser string, impersonating bool) {
	realUser = s.getRealAuthUser(r)
	if realUser == "" || !isHubAdmin(realUser) {
		return realUser, realUser, false
	}
	cookie, err := r.Cookie(impersonateCookieName)
	if err != nil || cookie.Value == "" {
		return realUser, realUser, false
	}
	grant, _, ok := verifyImpersonateCookieValueWithGenerations(s.currentGenerations(), cookie.Value, time.Now())
	if !ok {
		return realUser, realUser, false
	}
	// The cookie must name THIS real admin as its actor. Anything else — a
	// cookie minted for a different admin, or one lifted onto the wrong
	// session — is ignored rather than trusted.
	if grant.Admin != realUser {
		return realUser, realUser, false
	}
	if loadSaaSUser(grant.Target) == nil {
		return realUser, realUser, false
	}
	// A valid, active grant exists. Report impersonating=true regardless of
	// method (so writes can be blocked), but only SWITCH the effective identity
	// for read requests.
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return grant.Target, realUser, true
	}
	return realUser, realUser, true
}

// getAuthUser returns the EFFECTIVE identity for the request: the impersonated
// target on a GET/HEAD while a valid admin grant is active, otherwise the real
// authenticated user. Every per-user handler calls this, so making it
// impersonation-aware is what renders all per-user views as the target with no
// per-handler change.
func (s *HubServer) getAuthUser(r *http.Request) string {
	effective, _, _ := s.resolveIdentity(r)
	return effective
}

var (
	ghTokenCacheMu sync.RWMutex
	ghTokenCache   = map[string]ghTokenCacheEntry{}
)

const ghTokenCacheTTL = 5 * time.Minute

type ghTokenCacheEntry struct {
	username  string
	expiresAt time.Time
}

func (s *HubServer) validateGitHubToken(token string) string {
	if token == "" {
		return ""
	}

	ghTokenCacheMu.RLock()
	if entry, ok := ghTokenCache[token]; ok && time.Now().Before(entry.expiresAt) {
		ghTokenCacheMu.RUnlock()
		return entry.username
	}
	ghTokenCacheMu.RUnlock()

	client := &http.Client{Timeout: 10 * time.Second}
	// Hub always validates tokens against github.com (the hub is a SaaS service).
	req, err := http.NewRequest("GET", defaultGHUserURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != 200 {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	var user struct {
		Login string `json:"login"`
	}
	if json.NewDecoder(resp.Body).Decode(&user) != nil {
		return ""
	}

	ghTokenCacheMu.Lock()
	ghTokenCache[token] = ghTokenCacheEntry{username: user.Login, expiresAt: time.Now().Add(ghTokenCacheTTL)}
	ghTokenCacheMu.Unlock()

	return user.Login
}

// saaSUserFilePaths returns the candidate on-disk paths for an identity, in
// read/try order: the canonical filename first, then the legacy "<login>.json"
// fallback for a bare or github: identity. The caller has already rejected
// path-traversal characters in the raw username.
func saaSUserFilePaths(username string) []string {
	var paths []string
	if stem, err := encodeUserFilename(username); err == nil {
		paths = append(paths, filepath.Join(saasUsersDir, stem+".json"))
	}
	provider, subject, ok := parseCanonical(username)
	if ok && provider == legacyProvider {
		legacy := filepath.Join(saasUsersDir, subject+".json")
		if len(paths) == 0 || paths[0] != legacy {
			paths = append(paths, legacy)
		}
	}
	return paths
}

// readSaaSUserFile reads a user's JSON, trying the canonical filename then the
// legacy fallback (see saaSUserFilePaths). Returns the first file that reads.
func readSaaSUserFile(username string) ([]byte, error) {
	var firstErr error
	for _, p := range saaSUserFilePaths(username) {
		data, err := os.ReadFile(p)
		if err == nil {
			return data, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr == nil {
		firstErr = os.ErrNotExist
	}
	return nil, firstErr
}

// saveSaaSUserPath returns the file path a user record is written to. A GitHub
// (or bare-legacy) primary keeps its legacy "<login>.json" so existing records
// are updated in place with no rename; a non-GitHub primary writes the canonical
// "<provider>.<subject>.json".
func saveSaaSUserPath(u *SaaSUser) (string, error) {
	// The record's primary identity: the explicit CanonicalID when set (a
	// non-GitHub or newly-created user), else GitHubUsername (a bare login for
	// legacy GitHub users). Either resolves through parseCanonical + the shim.
	id := userCanonicalID(u)
	provider, subject, ok := parseCanonical(id)
	if !ok {
		return "", fmt.Errorf("invalid identity for save: %q", id)
	}
	if provider == legacyProvider {
		return filepath.Join(saasUsersDir, subject+".json"), nil
	}
	stem, err := encodeUserFilename(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(saasUsersDir, stem+".json"), nil
}

func loadSaaSUser(username string) *SaaSUser {
	if strings.Contains(username, "..") || strings.Contains(username, "/") || strings.Contains(username, "\\") {
		return nil
	}
	// Dual-read: try the canonical filename ("google.1078.json") then the legacy
	// "<login>.json". No file is rewritten — existing users resolve via legacy.
	data, err := readSaaSUserFile(username)
	if err != nil {
		return nil
	}
	var u SaaSUser
	if json.Unmarshal(data, &u) != nil {
		return nil
	}
	if u.Hives == nil {
		u.Hives = make(map[string]string)
	}
	// Backfill LoginCount for records that predate the login counter (added with
	// the admin engagement card). Those users have a real LastLogin but a zero
	// LoginCount, which renders as the contradictory "0 logins (last <date>)" on
	// the stats card. A user who has logged in at least once is, at minimum, one
	// login — so a present LastLogin with a zero count normalizes to 1. This is a
	// read-time floor only; the real counter keeps incrementing from here on the
	// next OAuth login (handleOAuthCallback), and it never lowers a genuine count.
	if u.LoginCount == 0 && strings.TrimSpace(u.LastLogin) != "" {
		u.LoginCount = 1
	}
	// On-access expiry enforcement (#4150): drop expired grants at READ time so
	// every consumer of a role — auth gates, accessForHive, the heartbeat's
	// authorized-users push — sees the revocation the instant it is due, on the
	// wall clock. Read-time only, never written here; sweepExpiredAccess
	// (access_expiry.go) persists the prune and stamps the timeline event.
	pruneExpiredHiveGrants(&u, time.Now())
	return &u
}

func saveSaaSUser(u *SaaSUser) error {
	if strings.Contains(u.GitHubUsername, "..") || strings.Contains(u.GitHubUsername, "/") || strings.Contains(u.GitHubUsername, "\\") {
		return fmt.Errorf("invalid username for save: %q", u.GitHubUsername)
	}
	// Best-effort: a failed mkdir surfaces via the WriteFile error below.
	_ = os.MkdirAll(saasUsersDir, 0o755)
	data, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		return err
	}
	path, err := saveSaaSUserPath(u)
	if err != nil {
		return err
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func ensureSaaSUser(username string) *SaaSUser {
	now := time.Now().UTC().Format(time.RFC3339)
	u := loadSaaSUser(username)
	if u != nil {
		u.LastLogin = now
		if err := saveSaaSUser(u); err != nil {
			slog.Warn("ensureSaaSUser: save failed", "user", username, "error", err)
		}
		return u
	}
	quota := 0
	if isHubAdmin(username) {
		quota = -1
	}
	u = &SaaSUser{
		GitHubUsername: username,
		CreatedAt:      now,
		LastLogin:      now,
		Hives:          map[string]string{},
		SaaSQuota:      quota,
	}
	if err := saveSaaSUser(u); err != nil {
		slog.Warn("ensureSaaSUser: create failed", "user", username, "error", err)
	}
	return u
}

func listAllSaaSUsers() []SaaSUser {
	// Best-effort: a failed mkdir surfaces via the ReadDir error below.
	_ = os.MkdirAll(saasUsersDir, 0o755)
	entries, err := os.ReadDir(saasUsersDir)
	if err != nil {
		return nil
	}
	var users []SaaSUser
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		stem := strings.TrimSuffix(e.Name(), ".json")
		// A canonical filename ("google.1078", "github.foo") decodes to its wire
		// id; a legacy filename ("foo") does not — load it as the bare login.
		key := stem
		if id, ok := decodeUserFilename(stem); ok {
			key = id
		}
		u := loadSaaSUser(key)
		if u != nil {
			users = append(users, *u)
		}
	}
	return users
}

func (s *HubServer) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// SECURITY (N6, CWE-352): admin routes carry the SAME CSRF exposure as
		// requireAuth ones — the hub session cookie is ambient, so a cross-site
		// form POST to e.g. /api/saas/hub/upgrade or the cluster-app-key writer
		// executes with the admin's identity. requireAuth has always checked this
		// (see :499); requireAdmin never did, leaving every admin mutation
		// reachable from ANY origin — strictly worse than the sibling-tenant lane,
		// which at least requires a *.hive.kubestellar.io foothold. Checked first,
		// before any identity resolution, so a forged request never reaches the
		// impersonation logic below.
		if !isCSRFSafe(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"CSRF check failed"}`))
			return
		}
		// Gate on the REAL logged-in user, not the effective (possibly
		// impersonated) identity. While the admin is viewing as a normal user,
		// getAuthUser resolves to that user on GETs — but admin routes (and in
		// particular the impersonation exit) must still be reachable by the real
		// admin, and no admin-only surface may leak to the impersonated target.
		username := s.getRealAuthUser(r)
		if !isHubAdmin(username) {
			http.Error(w, `{"error":"admin access required"}`, http.StatusForbidden)
			return
		}
		// While impersonating, NO admin-only surface may leak to the view the
		// admin is "viewing as" — the whole point of read-only impersonation is to
		// see exactly what the target user sees, and a normal user is never an
		// admin. requireAdmin gates on the REAL admin (so exit and admin routes
		// stay reachable), which means an admin-DATA GET (e.g. /api/saas/admin/users)
		// would otherwise still answer 200 under impersonation and the client would
		// render the admin Users section. So: while a grant is active, refuse every
		// admin route (GET included) EXCEPT the impersonation exit — the client's
		// 403 handling then hides the admin section, matching what the target sees.
		if r.URL.Path != impersonateExitPath {
			if _, _, impersonating := s.resolveIdentity(r); impersonating {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"admin surfaces are hidden while viewing as a user — exit impersonation for admin access"}`))
				return
			}
		}
		// Admin writes are also read-only under impersonation (exit excepted),
		// so an impersonating admin cannot mutate through an admin endpoint
		// either. (Redundant with the block above now, but kept as defense in
		// depth / a clear write-specific message if the above is ever relaxed.)
		if s.blockIfImpersonatingWrite(w, r) {
			return
		}
		next(w, r)
	}
}

func (s *HubServer) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	users := listAllSaaSUsers()
	// status_tier is computed HUB-side (it needs the hub's live/engaged
	// presence view and the tier windows) so the dashboard renders and sorts a
	// server-decided classification instead of re-deriving policy in JS.
	// See userStatusTier for the tier rules.
	live := make(map[string]bool)
	for _, name := range s.liveHiveUsernames() {
		live[name] = true
	}
	engaged := make(map[string]bool)
	for _, name := range s.engagedHiveUsernames() {
		engaged[name] = true
	}
	now := time.Now()
	type adminUserView struct {
		SaaSUser
		StatusTier string `json:"status_tier"`
		// Provider is always populated (derived via userProvider) so the Users
		// table's auth-method badge never has to parse — a legacy github-only
		// record resolves to "github". This shadows SaaSUser.Provider's
		// omitempty, so the field is present on every row.
		Provider string `json:"provider"`
	}
	views := make([]adminUserView, 0, len(users))
	for i := range users {
		users[i].EncryptedToken = ""
		name := users[i].GitHubUsername
		views = append(views, adminUserView{
			SaaSUser:   users[i],
			StatusTier: userStatusTier(&users[i], live[name], engaged[name], now),
			Provider:   userProvider(&users[i]),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"users": views})
}

// setImpersonateCookie writes (value != "") or clears (value == "") the signed
// impersonation cookie with the same hardened attributes as the session cookie:
// HttpOnly (no JS access), Secure (HTTPS only), SameSite=Lax. The short MaxAge
// mirrors impersonateTTL so the browser drops the cookie about when the server
// would stop honoring it.
//
// SECURITY (audit F4): this cookie is HOST-ONLY. It previously carried
// Domain=.hive.kubestellar.io, copied from the session cookie, which meant the
// admin's live impersonation grant was transmitted to every hosted tenant's
// dashboard — i.e. handed to ~62 untrusted third parties on every request they
// received. Unlike hive_hub_user, NOTHING outside the hub ever reads it:
// grep confirms hive_hub_impersonate appears only in this package
// (activeImpersonationGrant / resolveIdentity), never in the spoke proxy
// (src/proxy/server.js) or any manifest. Dropping Domain therefore costs
// nothing and removes the cookie from the sibling attack surface entirely.
//
// Omitting Domain (rather than setting it) is what makes a cookie host-only per
// RFC 6265 §4.1.2.3 — there is no "Domain=host" spelling that achieves this.
//
// No flag day: the mint and the read both happen on hive.kubestellar.io, so a
// browser holding the OLD domain-scoped cookie still presents it to the hub and
// still verifies. The next impersonate/exit re-mints it host-only. Worst case
// for an in-flight grant is that it expires on its own 30-minute TTL.
func setImpersonateCookie(w http.ResponseWriter, value string) {
	c := &http.Cookie{
		Name:     impersonateCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	}
	if value == "" {
		c.MaxAge = -1 // delete
	} else {
		c.MaxAge = int(impersonateTTL / time.Second)
	}
	http.SetCookie(w, c)
	if value == "" {
		// Also expire the legacy DOMAIN-scoped cookie. A host-only Set-Cookie
		// cannot delete a domain-scoped one — they are distinct entries in the
		// jar, and the browser would keep sending the old one on every hub
		// request until its own TTL lapsed, leaving "Exit impersonation"
		// silently ineffective for admins mid-migration. Emitting both
		// deletions is unconditional and idempotent: if no legacy cookie
		// exists, this is a no-op the browser discards.
		//
		// Removable once no admin can still be holding a pre-fix grant, which
		// the 30-minute impersonateTTL bounds.
		http.SetCookie(w, &http.Cookie{
			Name:     impersonateCookieName,
			Value:    "",
			Path:     "/",
			Domain:   legacyImpersonateCookieDomain,
			MaxAge:   -1,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
		})
	}
}

// legacyImpersonateCookieDomain is the sibling-wide scope the impersonation
// cookie used to carry before audit F4 made it host-only. Retained ONLY so the
// exit path can expire cookies minted by the previous build.
const legacyImpersonateCookieDomain = ".hive.kubestellar.io"

// handleImpersonateStart begins an admin read-only "View as user" session.
// Registered behind requireAdmin, so only the real hub admin reaches it (and
// the impersonation write-block cannot fire here because starting requires no
// active grant). It validates the target is a registered user, then sets the
// short-lived signed hive_hub_impersonate cookie. The admin gains NO privilege:
// subsequent GETs render as the target, and every write is refused 403.
func (s *HubServer) handleImpersonateStart(w http.ResponseWriter, r *http.Request) {
	admin := s.getRealAuthUser(r) // == hubAdminUsername (requireAdmin gated)
	target := r.PathValue("username")
	if target == "" || target == admin {
		writeJSONError(w, http.StatusBadRequest, "invalid target user")
		return
	}
	if loadSaaSUser(target) == nil {
		writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}
	value := mintImpersonateCookieValueForGeneration(s.currentGenerations(), admin, target, time.Now())
	if value == "" {
		writeJSONError(w, http.StatusInternalServerError, "cannot start impersonation")
		return
	}
	setImpersonateCookie(w, value)
	s.logger.Info("audit: admin impersonation started", "admin", admin, "target", target,
		"at", time.Now().UTC().Format(time.RFC3339))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "viewing_as": target})
}

// handleImpersonateExit ends an active "View as user" session by clearing the
// impersonation cookie. It is registered behind requireAdmin (the real admin is
// always the actor) and its path is exempt from the write-block, so it stays
// callable WHILE impersonating — that is the whole point. It is a no-op if no
// grant is active.
func (s *HubServer) handleImpersonateExit(w http.ResponseWriter, r *http.Request) {
	admin := s.getRealAuthUser(r)
	if grant, ok := s.activeImpersonationGrant(r); ok {
		s.logger.Info("audit: admin impersonation ended", "admin", admin, "target", grant.Target,
			"at", time.Now().UTC().Format(time.RFC3339))
	}
	setImpersonateCookie(w, "")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// handleImpersonationStatus reports whether the current request is inside an
// impersonation session and, if so, who is being viewed. The banner reads this
// (or the equivalent fields folded into /api/auth/user). Because getAuthUser
// resolves to the target on this GET, the status is derived from the real admin
// grant via activeImpersonationGrant rather than from getAuthUser.
func (s *HubServer) handleImpersonationStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if grant, ok := s.activeImpersonationGrant(r); ok {
		_ = json.NewEncoder(w).Encode(map[string]any{"impersonating": true, "viewing_as": grant.Target})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"impersonating": false})
}

// handleAdminUpdateUser applies a partial admin edit to one hub user record.
// Every field is a POINTER in the request body, so a request carries only the
// keys the admin actually changed: the handler loads the current record, sets
// just those fields, and saves. That read-modify-write is what keeps a contact
// edit from clobbering Hives / quota / Blocked (and vice versa) when two admin
// widgets on the dashboard post concurrently.
//
// The contact fields (full_name, slack_id, notes) are admin-entered free text.
// They are length-capped here — the last point before the value reaches the
// PVC — and escaped on every dashboard render path.
//
// `country` rides this same body rather than a route of its own. It is the only
// way the field can be set for the thousands of users who joined before it
// existed: the wizard is a one-time gate already behind them, and the
// self-service endpoint reaches only the acting user, so an admin looking at a
// row with an empty Country column previously had no control at all. It carries
// ADMIN provenance, never user provenance — see the block on the country branch
// below, which is the load-bearing decision in this change.
//
// PRIVACY: the code rides the JSON BODY, never the path or a query string, for
// the same reason the self-service endpoint does — a URL lands in access logs,
// Referer headers and browser history.
//
// Registered behind requireAdmin; this handler does no auth of its own.
func (s *HubServer) handleAdminUpdateUser(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	u := loadSaaSUser(username)
	if u == nil {
		writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}
	// Free-text fields land on a PVC, so bound the body before decoding it.
	r.Body = http.MaxBytesReader(w, r.Body, maxUpdateUserBodyBytes)
	var body struct {
		SaaSQuota *int    `json:"saas_quota"`
		Blocked   *bool   `json:"blocked"`
		FullName  *string `json:"full_name"`
		SlackID   *string `json:"slack_id"`
		Notes     *string `json:"notes"`
		Company   *string `json:"company"`
		// Pointer like the rest, and for a sharper reason here: `""` is an
		// explicit CLEAR ("remove this country"), while an absent key means the
		// admin edited some other field and this one must not be touched. A
		// plain string would collapse the two and let a quota edit silently
		// wipe a country.
		Country *string `json:"country"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	// Whether the country branch below actually APPLIED, which is not the same
	// as the key being present: a stronger user-chosen value declines the edit.
	// Tracked so the audit line records what changed rather than what was asked.
	countryEdited := false
	if body.SaaSQuota != nil {
		u.SaaSQuota = *body.SaaSQuota
	}
	if body.Blocked != nil {
		u.Blocked = *body.Blocked
	}
	// Trim before capping so trailing whitespace does not eat the budget, and
	// so clearing a field to spaces stores "" (and drops out via omitempty).
	if body.FullName != nil {
		u.FullName = truncateRunes(strings.TrimSpace(*body.FullName), maxContactNameLen)
	}
	if body.SlackID != nil {
		u.SlackID = truncateRunes(strings.TrimSpace(*body.SlackID), maxContactSlackIDLen)
	}
	if body.Notes != nil {
		u.Notes = truncateRunes(strings.TrimSpace(*body.Notes), maxContactNotesLen)
	}
	if body.Company != nil {
		u.Company = truncateRunes(strings.TrimSpace(*body.Company), maxContactCompanyLen)
	}
	if body.Country != nil {
		raw := strings.TrimSpace(*body.Country)
		code := ""
		if raw != "" {
			// The SAME validator every other country path uses, so this
			// endpoint cannot drift into accepting a shape the render sites
			// reject. Not capped like the free-text fields above: a country is
			// two letters or it is rejected outright, so there is nothing to
			// truncate — a bad value must 400 rather than be silently reshaped
			// into a different country.
			code = normalizeCountryCode(raw)
			if code == "" {
				writeJSONError(w, http.StatusBadRequest, "country must be an ISO 3166-1 alpha-2 code (two letters), or \"\" to clear it")
				return
			}
		}
		// PROVENANCE — the whole point of this branch, and the easy thing to get
		// wrong. An admin edit is countrySourceAdmin, NEVER countrySourceUser:
		//
		//   - It must not claim the user chose this. They did not; an admin
		//     inferred it from a conference badge, an email domain, a
		//     conversation. Marking it user-chosen would fabricate a statement
		//     and would permanently suppress ever asking them for a real one.
		//   - It must still outrank the login-path Accept-Language inference,
		//     or the assignment is silently reverted the next time the user
		//     signs in from a differently-configured browser — the #4374 bug in
		//     a new form, and invisible in exactly the same way.
		//
		// mayOverwriteCountry is what keeps the admin from stepping on a value
		// the USER stated about themselves. It is not an error to try: the edit
		// is simply not applied to the country, the rest of the request still
		// lands, and the response is still a 200 — the admin has changed
		// nothing they were entitled to change. A 409 here would fail an
		// otherwise-valid multi-field save over a field the admin may not even
		// have meant to touch.
		if mayOverwriteCountry(u, countrySourceAdmin) {
			setUserCountry(u, code, countrySourceAdmin)
			countryEdited = true
		}
	}
	// A failed write is the one outcome the admin MUST hear about: the dashboard
	// closes the editor on a 2xx, so reporting success here after the PVC write
	// failed would silently discard the edit.
	if err := saveSaaSUser(u); err != nil {
		s.logger.Error("admin update user: save failed", "target", username, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to save user record")
		return
	}
	// Do not log the note bodies — they are free text and may hold anything an
	// admin jotted down. Log only that contact fields were touched.
	//
	// The country VALUE is logged, unlike those bodies: it is a two-letter code
	// from a closed shape, an admin assigning one on another person's behalf is
	// exactly the attribution an audit trail exists to record, and "who decided
	// this user is in GB" is unanswerable from a bare "countryEdited: true".
	// Logged only when the write actually applied, so the line never claims a
	// change that mayOverwriteCountry declined.
	attrs := []any{"target", username, "quota", u.SaaSQuota, "blocked", u.Blocked,
		"contactEdited", body.FullName != nil || body.SlackID != nil || body.Notes != nil || body.Company != nil}
	if countryEdited {
		attrs = append(attrs, "countryAssigned", u.Country, "countrySource", u.CountrySource)
	}
	s.logger.Info("audit: admin updated user", attrs...)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
}

// handleAdminDeleteUser removes a hub user record. It refuses to delete the
// hub admin, and refuses to delete a user who still owns hosted hives — those
// must be deleted (or reassigned) first so no namespace is orphaned. Deleting
// a user does not touch GitHub; it only removes the hub's local account
// record (login state, quota, encrypted token).
func (s *HubServer) handleAdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	if isHubAdmin(username) {
		writeJSONError(w, http.StatusForbidden, "cannot delete the hub admin")
		return
	}
	if strings.Contains(username, "..") || strings.Contains(username, "/") || strings.Contains(username, "\\") {
		writeJSONError(w, http.StatusBadRequest, "invalid username")
		return
	}
	u := loadSaaSUser(username)
	if u == nil {
		writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}
	var ownedHives []string
	for hiveID, role := range u.Hives {
		if role == "owner" {
			ownedHives = append(ownedHives, hiveID)
		}
	}
	if len(ownedHives) > 0 {
		writeJSONError(w, http.StatusConflict, fmt.Sprintf("user still owns %d hive(s); delete or reassign them first: %s",
			len(ownedHives), strings.Join(ownedHives, ", ")))
		return
	}
	path := filepath.Join(saasUsersDir, username+".json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		s.logger.Warn("admin delete user: remove failed", "target", username, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to delete user record")
		return
	}
	s.logger.Info("audit: admin deleted user", "target", username, "by", s.getAuthUser(r))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
}

// --- Cluster Helpers ---

// clusterIDForSaaSHive returns the cluster ID for a SaaS hive, defaulting to
// the default cluster when the field is empty (backward compatibility).
func clusterIDForSaaSHive(sh SaaSHive) string {
	if sh.ClusterID != "" {
		return sh.ClusterID
	}
	return defaultClusterID
}

// ensureClusterIDForClaim stamps a non-blank cluster_id onto a hive that is
// about to be persisted by a claim/assign. This is the data-integrity guard
// for the observed bug where CLAIMED hives lost cluster_id in their meta.json
// and then fell back to defaultClusterID (the hub-reachable cluster) in clusterForHive — which
// mis-routed App/host resolution for hives that actually run on the heartbeat-only cluster.
//
// Precedence, most-trusted first:
//  1. The hive's OWN non-blank ClusterID (the placeholder already belongs to a
//     cluster — always the most authoritative source; never override it).
//  2. poolFallback — the pool the claim was drawn from, when the caller knows
//     it (e.g. handleApproveProvision picks a pool by auth_method), but only
//     when it names a cluster the hub actually has.
//  3. defaultClusterID — last resort, matching clusterForHive's own fallback.
//
// The result is always non-blank, so with omitempty on the json tag it still
// serializes to a concrete "cluster_id" value rather than vanishing.
func (s *HubServer) ensureClusterIDForClaim(h *SaaSHive, poolFallback string) {
	if h.ClusterID != "" {
		return
	}
	if poolFallback != "" {
		if _, ok := s.clusters[poolFallback]; ok {
			h.ClusterID = poolFallback
			return
		}
	}
	h.ClusterID = defaultClusterID
}

// clusterNameForID returns the human-readable name for a cluster ID.
// Returns empty string when the cluster is not found.
func (s *HubServer) clusterNameForID(clusterID string) string {
	if c, ok := s.clusters[clusterID]; ok {
		return c.Name
	}
	return ""
}

// --- Cluster List API ---

// ClusterListEntry is the JSON response for the clusters list endpoint.
type ClusterListEntry struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	HasGPU bool   `json:"has_gpu"`
	Arch   string `json:"arch"`
	// GitHubHost is the bare hostname of the GitHub instance hives on this
	// cluster default to ("github.com" for public GitHub). Shown in the
	// create-hive modal so an admin can see which GitHub a hive will target.
	GitHubHost string `json:"github_host,omitempty"`
	// AppInstallURL is the GitHub App install link for THIS cluster's GitHub
	// host and app slug. A GitHub Enterprise cluster must never be handed a
	// public github.com link: the install request would land on the wrong
	// GitHub and the GHE org admin would never see it.
	AppInstallURL string `json:"app_install_url,omitempty"`
}

// clusterGitHubConfig projects a cluster's GitHub settings onto the
// config.GitHubConfig that owns URL construction, so the hub and the spoke
// build install links from exactly one implementation.
func clusterGitHubConfig(c *ClusterConfig) config.GitHubConfig {
	if c == nil {
		return config.GitHubConfig{}
	}
	base := c.GitHubBaseURL
	// The cluster stores "" for public GitHub; config.GitHubConfig uses the
	// same convention, so pass it through untouched. Carry the api_url too so the
	// derived config's HostLabel()/IsGHE()/AppInstallURL() resolve the forge
	// base-or-api: a GHE cluster that records only an api_url (blank base_url —
	// the common state) is still recognised as GHE, not mislabelled github.com.
	return config.GitHubConfig{BaseURL: base, APIURL: c.GitHubAPIURL, AppSlug: c.GitHubAppSlug}
}

// githubHostLabel renders a GitHub base URL as a bare hostname for display.
// Empty (public GitHub) becomes "github.com" rather than an empty chip.
func githubHostLabel(baseURL string) string {
	h := strings.TrimSpace(baseURL)
	if h == "" {
		return "github.com"
	}
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
	return strings.TrimRight(h, "/")
}

// GitHubHostLabel is the exported form of githubHostLabel, for the spoke to
// normalize its own configured GitHub base URL before reporting it over the
// heartbeat. Sharing one implementation keeps the value the spoke sends and
// the value the hub renders from drifting into two different spellings.
func GitHubHostLabel(baseURL string) string { return githubHostLabel(baseURL) }

func (s *HubServer) handleListClusters(w http.ResponseWriter, r *http.Request) {
	var entries []ClusterListEntry
	for _, c := range s.clusters {
		gh := clusterGitHubConfig(&c)
		entries = append(entries, ClusterListEntry{
			ID:            c.ID,
			Name:          c.Name,
			HasGPU:        c.HasGPU,
			Arch:          c.Arch,
			GitHubHost:    gh.HostLabel(),
			AppInstallURL: gh.AppInstallURL(),
		})
	}
	// Sort for deterministic API output.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].ID < entries[j].ID
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entries)
}

// --- Cluster Health ---

const clusterHealthCacheTTL = 30 * time.Second

// CPU and memory bar thresholds are NOT declared here. The hub serves the
// cluster-health panel raw percentages and the panel colours them with its own
// CLUSTER_CPU_WARN_PCT / CLUSTER_CPU_DANGER_PCT / CLUSTER_MEM_* constants, so a
// Go-side copy would be a second set of numbers that nothing reads and nobody
// updates together. The disk thresholds below are different: they are anchored
// to kubelet behaviour rather than taste and are pinned by a test, so they have
// a reason to exist on this side.
//
// Disk thresholds are anchored to kubelet's own behaviour rather than to
// round numbers, so a coloured bar means something concrete is about to
// happen on the node:
//
//	evictionHard: nodefs.available<10%  -> kubelet starts evicting pods at
//	                                       90% used, so that is the danger line.
//	imageGCHighThresholdPercent: 85     -> kubelet begins garbage-collecting
//	                                       images at 85% used. That is the
//	                                       first automatic reaction to disk
//	                                       filling up, which makes it the
//	                                       right moment to warn an operator
//	                                       while there is still headroom.
//
// clusterHealthDiskWarnPct is the disk usage percentage at which kubelet's
// image garbage collection kicks in (imageGCHighThresholdPercent).
const clusterHealthDiskWarnPct = 85

// clusterHealthDiskDangerPct is the disk usage percentage at which kubelet's
// hard eviction threshold fires (nodefs.available<10%).
const clusterHealthDiskDangerPct = 90

// millicoresPerCore converts cores to millicores.
const millicoresPerCore = 1000

// kiToBytes converts Ki units to bytes.
const kiToBytes = 1024

// miToBytes converts Mi units to bytes.
const miToBytes = 1024 * 1024

// giToBytes converts Gi units to bytes.
const giToBytes = 1024 * 1024 * 1024

// bytesPerMB converts bytes to megabytes.
const bytesPerMB = 1024 * 1024

// percentMultiplier converts a ratio to a percentage.
const percentMultiplier = 100

type ClusterHealthNode struct {
	Name          string `json:"name"`
	CPUCores      int    `json:"cpu_cores"`
	CPUUsedMillis int64  `json:"cpu_used_millicores"`
	CPUPercent    int    `json:"cpu_percent"`
	MemTotalMB    int64  `json:"mem_total_mb"`
	MemUsedMB     int64  `json:"mem_used_mb"`
	MemPercent    int    `json:"mem_percent"`
	DiskPressure  bool   `json:"disk_pressure"`
	// Disk fields describe the node filesystem (nodefs) that kubelet applies
	// its eviction thresholds to. They are pointers because live disk usage
	// comes from the kubelet stats/summary endpoint, which a hub may not be
	// able to reach; nil means "unknown" and must render as dashes rather
	// than as 0 (which would read as healthy).
	DiskTotalMB *int64 `json:"disk_total_mb,omitempty"`
	DiskUsedMB  *int64 `json:"disk_used_mb,omitempty"`
	DiskPercent *int   `json:"disk_percent,omitempty"`
	Pods        int    `json:"pods"`
	PodCapacity int    `json:"pod_capacity"`
	// HiveCount is the number of distinct hive-hosted-* namespaces with a
	// running pod on this node (namespaces, not pods, so a hive briefly
	// running two pods during a rollout is counted once).
	HiveCount  int      `json:"hive_count"`
	Conditions []string `json:"conditions"`
}

// hiveHostedNamespacePrefix is the namespace prefix used for SaaS-provisioned
// hives; pods in these namespaces identify hives running on a node.
const hiveHostedNamespacePrefix = "hive-hosted-"

type ClusterHealthSummary struct {
	TotalNodes    int `json:"total_nodes"`
	TotalCPUCores int `json:"total_cpu_cores"`
	TotalCPUPct   int `json:"total_cpu_percent"`
	TotalMemGB    int `json:"total_mem_gb"`
	TotalMemPct   int `json:"total_mem_percent"`
	// Disk totals cover only the nodes that reported live disk usage. They
	// are pointers so a cluster with no reachable kubelet stats endpoint
	// omits them entirely instead of reporting a misleading 0%.
	TotalDiskGB  *int `json:"total_disk_gb,omitempty"`
	TotalDiskPct *int `json:"total_disk_percent,omitempty"`
	HiveCount    int  `json:"hive_count"`
	// HiveCapacityRemaining estimates how many MORE hives the cluster can
	// hold: per Ready, schedulable node, the per-hive request footprint
	// (see hive_capacity.go) bin-packed into allocatable-minus-requested
	// capacity, summed across nodes. Pointer so it is omitted entirely when
	// pod request data was unavailable (nil = no data, 0 = cluster full).
	HiveCapacityRemaining *int `json:"hive_capacity_remaining,omitempty"`
}

// GPUSummary reports aggregate GPU counts for a cluster.
type GPUSummary struct {
	TotalGPUs       int `json:"total_gpus"`
	AllocatableGPUs int `json:"allocatable_gpus"`
}

// PerClusterHealth holds health data for a single cluster.
type PerClusterHealth struct {
	ID         string               `json:"id"`
	Name       string               `json:"name"`
	Nodes      []ClusterHealthNode  `json:"nodes"`
	Summary    ClusterHealthSummary `json:"summary"`
	GPUSummary *GPUSummary          `json:"gpu_summary,omitempty"`
	HiveCount  int                  `json:"hive_count"`
	Error      string               `json:"error,omitempty"`
	DataSource string               `json:"data_source,omitempty"` // "heartbeat" when data comes from spoke heartbeat instead of kubectl
	DataStale  bool                 `json:"data_stale,omitempty"`  // true when heartbeat data is older than heartbeatHealthStaleness
	DataAge    string               `json:"data_age,omitempty"`    // human-readable age or collection timestamp
	// StuckPods reports hive-namespace pods stuck Terminating — the residue of
	// nodes disappearing without draining (#5328 item 3). Nil means the hub
	// could not determine it (unreachable cluster, pull-only pool, failed
	// listing); a non-nil report with Total 0 means it looked and the cluster
	// is clean. Those must not render alike: 27 orphans accumulated for three
	// weeks precisely because nothing distinguished "none" from "nobody
	// checked". See orphaned_pod_visibility.go.
	StuckPods *StuckPodReport `json:"stuck_pods,omitempty"`
	// LeakedNamespaces reports hive-hosted-* namespaces the cluster holds that
	// this hub has no hive record for — provisioning namespaces that were
	// created and never torn down (#5768). Nil means the hub could not
	// determine it (unreachable cluster, pull-only pool, failed listing, or an
	// empty hive registry, which cannot be told apart from an unreadable one);
	// a non-nil report with Total 0 means it looked and the cluster is clean.
	// Those must not render alike, for the same reason StuckPods above draws
	// the distinction. See leaked_hosted_namespace.go.
	LeakedNamespaces *LeakedNamespaceReport `json:"leaked_namespaces,omitempty"`
	// WildcardTLS reports the health of the wildcard certificate this cluster
	// serves its spokes from (#5977). Present ONLY for clusters that opted in
	// with wildcard_tls_secret, because only those have spokes depending on it:
	// on an opted-in cluster provisioned Ingresses carry no tls: block of their
	// own, so this one certificate stands behind every hosted dashboard and its
	// renewal is a single point of failure for all of them. Nil means either
	// "this cluster does not use the wildcard" or "the hub could not look" —
	// the same unknown-is-not-healthy contract the two fields above carry. See
	// wildcard_tls_health.go.
	WildcardTLS *WildcardTLSReport `json:"wildcard_tls,omitempty"`
}

type ClusterHealthResponse struct {
	// Flat fields for backward compatibility (aggregate across all clusters).
	Nodes   []ClusterHealthNode  `json:"nodes"`
	Summary ClusterHealthSummary `json:"summary"`
	// Per-cluster breakdown.
	Clusters []PerClusterHealth `json:"clusters,omitempty"`
}

var (
	clusterHealthCache     *ClusterHealthResponse
	clusterHealthCacheTime time.Time
	clusterHealthCacheMu   sync.Mutex
)

func (s *HubServer) handleClusterHealth(w http.ResponseWriter, r *http.Request) {
	clusterHealthCacheMu.Lock()
	if clusterHealthCache != nil && time.Since(clusterHealthCacheTime) < clusterHealthCacheTTL {
		cached := clusterHealthCache
		clusterHealthCacheMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cached)
		return
	}
	clusterHealthCacheMu.Unlock()

	resp, err := buildClusterHealth(s)
	if err != nil {
		s.logger.Error("cluster health failed", "error", err)
		http.Error(w, `{"error":"failed to gather cluster health"}`, http.StatusInternalServerError)
		return
	}

	clusterHealthCacheMu.Lock()
	clusterHealthCache = resp
	clusterHealthCacheTime = time.Now()
	clusterHealthCacheMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

const (
	// defaultClusterHealthQueryTimeout limits how long we wait for in-cluster
	// health queries when the cluster config does not override it.
	defaultClusterHealthQueryTimeout = 30 * time.Second
	// remoteClusterHealthQueryTimeout gives remote clusters more time because
	// they traverse an external network path from the hub pod.
	remoteClusterHealthQueryTimeout = 60 * time.Second
)

// gpuResourceKey is the extended resource name for NVIDIA GPUs on Kubernetes nodes.
const gpuResourceKey = "nvidia.com/gpu"

func buildClusterHealth(s *HubServer) (*ClusterHealthResponse, error) {
	// Count hives per cluster, deduplicated by hive ID. A hosted hive appears
	// both as a SaaS record and as a registry entry with a ClusterID (they
	// share the same ID), so counting both sources naively doubles the count.
	hiveIDsByCluster := make(map[string]map[string]bool)
	allHiveIDs := make(map[string]bool)
	addHive := func(clusterID, hiveID string) {
		if hiveIDsByCluster[clusterID] == nil {
			hiveIDsByCluster[clusterID] = make(map[string]bool)
		}
		hiveIDsByCluster[clusterID][hiveID] = true
		allHiveIDs[hiveID] = true
	}
	saasHives, saasHivesReadable := listSaaSHivesWithReadStatus()
	for _, sh := range saasHives {
		addHive(clusterIDForSaaSHive(sh), sh.ID)
	}
	s.mu.RLock()
	for _, h := range s.registry.Hives {
		if h.ClusterID != "" {
			addHive(h.ClusterID, h.ID)
		} else {
			// Self-hosted hives without a cluster still count globally.
			allHiveIDs[h.ID] = true
		}
	}
	s.mu.RUnlock()
	hiveCounts := make(map[string]int, len(hiveIDsByCluster))
	for cid, ids := range hiveIDsByCluster {
		hiveCounts[cid] = len(ids)
	}
	totalHiveCount := len(allHiveIDs)

	// One registry snapshot for the whole health build, so two clusters queried
	// in parallel cannot disagree about which namespaces are accounted for.
	// Built from the UNION of the on-disk SaaS hive records and the in-memory
	// registry — the widest set of "the hub knows about this id" available —
	// because the leak predicate convicts a namespace for being absent from it,
	// and a narrower set would convict namespaces that are merely recorded
	// somewhere else. See leaked_hosted_namespace.go.
	var knownHostedNamespaces map[string]struct{}
	if saasHivesReadable {
		knownHostedNamespaces = hostedNamespacesForHiveIDs(allHiveIDs)
	} else if s.logger != nil {
		s.logger.Warn("leaked-namespace detection disabled: SaaS hive directory could not be read")
	}

	// Query all clusters in parallel.
	type clusterResult struct {
		health PerClusterHealth
		err    error
	}
	type clusterQuery struct {
		cluster ClusterConfig
		ch      chan clusterResult
	}
	results := make(map[string]clusterQuery)
	for _, c := range s.clusters {
		ch := make(chan clusterResult, 1)
		results[c.ID] = clusterQuery{cluster: c, ch: ch}
		go func(cluster ClusterConfig) {
			health, err := buildSingleClusterHealth(&cluster, hiveCounts[cluster.ID], knownHostedNamespaces, s.logger)
			ch <- clusterResult{health: health, err: err}
		}(c)
	}

	// Collect results with timeout.
	var allNodes []ClusterHealthNode
	var perCluster []PerClusterHealth
	var aggCPUCores int
	var aggCPUUsed int64
	var aggCPUAlloc int64
	var aggMemAlloc int64
	var aggMemUsed int64

	for cID, query := range results {
		timeout := clusterHealthQueryTimeoutFor(&query.cluster)
		select {
		case res := <-query.ch:
			if res.err != nil {
				if errors.Is(res.err, errClusterPullOnly) {
					s.logger.Info("cluster health: pull-only cluster, using the health its spokes report over the heartbeat", "cluster", cID)
				} else {
					s.logger.Warn("cluster health query failed", "cluster", cID, "error", res.err)
				}
				// Fall back to heartbeat-reported health if available.
				if hbHealth := s.getHeartbeatHealthForCluster(cID); hbHealth != nil {
					pch := convertHeartbeatToPerClusterHealth(cID, s.clusterNameForID(cID), hbHealth, hiveCounts[cID])
					perCluster = append(perCluster, pch)
					allNodes = append(allNodes, pch.Nodes...)
					aggCPUCores += pch.Summary.TotalCPUCores
					aggCPUUsed += int64(pch.Summary.TotalCPUPct) * int64(pch.Summary.TotalCPUCores) * millicoresPerCore / percentMultiplier
					aggCPUAlloc += int64(pch.Summary.TotalCPUCores) * millicoresPerCore
					aggMemAlloc += int64(pch.Summary.TotalMemGB) * giToBytes
					aggMemUsed += int64(pch.Summary.TotalMemPct) * int64(pch.Summary.TotalMemGB) * giToBytes / percentMultiplier
					s.logger.Info("cluster health: using heartbeat fallback", "cluster", cID)
					continue
				}
				perCluster = append(perCluster, PerClusterHealth{
					ID:    cID,
					Name:  s.clusterNameForID(cID),
					Error: res.err.Error(),
				})
				continue
			}
			pch := res.health
			pch.ID = cID
			pch.Name = s.clusterNameForID(cID)
			perCluster = append(perCluster, pch)
			allNodes = append(allNodes, pch.Nodes...)
			aggCPUCores += pch.Summary.TotalCPUCores
			aggCPUUsed += int64(pch.Summary.TotalCPUPct) * int64(pch.Summary.TotalCPUCores) * millicoresPerCore / percentMultiplier
			aggCPUAlloc += int64(pch.Summary.TotalCPUCores) * millicoresPerCore
			aggMemAlloc += int64(pch.Summary.TotalMemGB) * giToBytes
			aggMemUsed += int64(pch.Summary.TotalMemPct) * int64(pch.Summary.TotalMemGB) * giToBytes / percentMultiplier
		case <-time.After(timeout):
			s.logger.Warn("cluster health query timed out", "cluster", cID, "timeout", timeout.String())
			// Fall back to heartbeat-reported health if available.
			if hbHealth := s.getHeartbeatHealthForCluster(cID); hbHealth != nil {
				pch := convertHeartbeatToPerClusterHealth(cID, s.clusterNameForID(cID), hbHealth, hiveCounts[cID])
				perCluster = append(perCluster, pch)
				allNodes = append(allNodes, pch.Nodes...)
				aggCPUCores += pch.Summary.TotalCPUCores
				aggCPUUsed += int64(pch.Summary.TotalCPUPct) * int64(pch.Summary.TotalCPUCores) * millicoresPerCore / percentMultiplier
				aggCPUAlloc += int64(pch.Summary.TotalCPUCores) * millicoresPerCore
				aggMemAlloc += int64(pch.Summary.TotalMemGB) * giToBytes
				aggMemUsed += int64(pch.Summary.TotalMemPct) * int64(pch.Summary.TotalMemGB) * giToBytes / percentMultiplier
				s.logger.Info("cluster health: using heartbeat fallback after timeout", "cluster", cID)
				continue
			}
			perCluster = append(perCluster, PerClusterHealth{
				ID:    cID,
				Name:  s.clusterNameForID(cID),
				Error: "query timed out",
			})
		}
	}

	// Include heartbeat-only clusters that are NOT in s.clusters but do have
	// heartbeat-reported health data. This handles firewalled spokes whose
	// cluster isn't in the hub's clusters.json.
	clusterSeen := make(map[string]bool, len(results))
	for cID := range results {
		clusterSeen[cID] = true
	}
	s.heartbeatHealthMu.RLock()
	for cID, entry := range s.heartbeatHealth {
		if clusterSeen[cID] || entry == nil || entry.Report == nil {
			continue
		}
		pch := convertHeartbeatToPerClusterHealth(cID, s.clusterNameForID(cID), entry, hiveCounts[cID])
		perCluster = append(perCluster, pch)
		allNodes = append(allNodes, pch.Nodes...)
		aggCPUCores += pch.Summary.TotalCPUCores
		aggCPUUsed += int64(pch.Summary.TotalCPUPct) * int64(pch.Summary.TotalCPUCores) * millicoresPerCore / percentMultiplier
		aggCPUAlloc += int64(pch.Summary.TotalCPUCores) * millicoresPerCore
		aggMemAlloc += int64(pch.Summary.TotalMemGB) * giToBytes
		aggMemUsed += int64(pch.Summary.TotalMemPct) * int64(pch.Summary.TotalMemGB) * giToBytes / percentMultiplier
	}
	s.heartbeatHealthMu.RUnlock()

	// Compute aggregates only after heartbeat-only clusters are included; an
	// earlier pre-inclusion computation was dead (always overwritten here).
	aggCPUPct := 0
	if aggCPUAlloc > 0 {
		aggCPUPct = int(aggCPUUsed * percentMultiplier / aggCPUAlloc)
	}
	aggMemPct := 0
	if aggMemAlloc > 0 {
		aggMemPct = int(aggMemUsed * percentMultiplier / aggMemAlloc)
	}
	aggMemGB := int(aggMemAlloc / giToBytes)

	// Sort clusters by ID for deterministic output.
	sort.Slice(perCluster, func(i, j int) bool {
		return perCluster[i].ID < perCluster[j].ID
	})

	// Every collection path above appends its nodes to allNodes, so the fleet
	// disk total is derived from them directly. Nodes with no disk data are
	// skipped, so an unreachable cluster lowers coverage without skewing the
	// percentage; if no node anywhere reported, disk is omitted entirely.
	aggDiskGB, aggDiskPct := summarizeDisk(allNodes)

	return &ClusterHealthResponse{
		Nodes: allNodes,
		Summary: ClusterHealthSummary{
			TotalNodes:    len(allNodes),
			TotalCPUCores: aggCPUCores,
			TotalCPUPct:   aggCPUPct,
			TotalMemGB:    aggMemGB,
			TotalMemPct:   aggMemPct,
			TotalDiskGB:   aggDiskGB,
			TotalDiskPct:  aggDiskPct,
			HiveCount:     totalHiveCount,
		},
		Clusters: perCluster,
	}, nil
}

func clusterHealthQueryTimeoutFor(cluster *ClusterConfig) time.Duration {
	if cluster != nil && cluster.ClusterHealthTimeoutSeconds > 0 {
		return time.Duration(cluster.ClusterHealthTimeoutSeconds) * time.Second
	}
	if cluster != nil && !cluster.InCluster {
		return remoteClusterHealthQueryTimeout
	}
	return defaultClusterHealthQueryTimeout
}

// errClusterPullOnly marks a health query skipped because the hub cannot reach
// the cluster at all. It is an EXPECTED outcome, not a fault, so callers report
// it as such and use the spokes' own heartbeat-reported health instead.
var errClusterPullOnly = errors.New("cluster is pull-only: not reachable from the hub")

// buildSingleClusterHealth queries a single cluster for node health data.
// knownHostedNamespaces is the fleet-wide set of hosted namespace names the
// hub has a hive for, threaded in from buildClusterHealth rather than re-read
// here so every cluster in one health build judges leaks against the SAME
// registry snapshot. See hostedNamespacesForHiveIDs.
func buildSingleClusterHealth(cluster *ClusterConfig, hiveCount int, knownHostedNamespaces map[string]struct{}, logger *slog.Logger) (PerClusterHealth, error) {
	if cluster.PullOnly {
		// Node-level health comes from kubectl, which cannot run here. This is
		// not a new failure mode: the caller already falls back to the health
		// the spokes THEMSELVES report over the heartbeat, which is exactly the
		// right source for a pull-only pool. Returning the sentinel routes into
		// that path and keeps it out of the "query failed" warning.
		return PerClusterHealth{}, fmt.Errorf("%w: %s", errClusterPullOnly, cluster.ID)
	}
	timeout := clusterHealthQueryTimeoutFor(cluster)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Run kubectl top nodes
	topCmd := kubectlForClusterContext(ctx, cluster, "--request-timeout", timeout.String(), "top", "nodes", "--no-headers")
	topOut, err := topCmd.CombinedOutput()
	if err != nil {
		return PerClusterHealth{}, fmt.Errorf("kubectl top nodes on %s: exit status 1: %s", cluster.ID, string(topOut))
	}

	// Run kubectl get nodes -o json
	getCmd := kubectlForClusterContext(ctx, cluster, "--request-timeout", timeout.String(), "get", "nodes", "-o", "json")
	getOut, err := getCmd.CombinedOutput()
	if err != nil {
		return PerClusterHealth{}, fmt.Errorf("kubectl get nodes on %s: %w: %s", cluster.ID, err, string(getOut))
	}

	// Parse kubectl get nodes output
	var nodesJSON struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Unschedulable bool `json:"unschedulable"`
			} `json:"spec"`
			Status struct {
				Allocatable map[string]string `json:"allocatable"`
				Capacity    map[string]string `json:"capacity"`
				Conditions  []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(getOut, &nodesJSON); err != nil {
		return PerClusterHealth{}, fmt.Errorf("parse nodes JSON on %s: %w", cluster.ID, err)
	}

	// Build a map of node info from kubectl get.
	type nodeInfo struct {
		cpuAllocatable int64 // millicores
		memAllocatable int64 // bytes
		podCapacity    int
		diskPressure   bool
		ready          bool
		unschedulable  bool // cordoned; excluded from hive capacity estimates
		conditions     []string
		gpuCapacity    int
		gpuAllocatable int
	}
	nodeMap := make(map[string]*nodeInfo)
	var totalGPUCapacity, totalGPUAllocatable int
	for _, item := range nodesJSON.Items {
		ni := &nodeInfo{}
		// Allocatable (not raw capacity) is what the scheduler can place
		// pods against, so hive capacity math below uses these values.
		ni.cpuAllocatable = parseK8sCPU(item.Status.Allocatable["cpu"])
		ni.memAllocatable = parseK8sMemory(item.Status.Allocatable["memory"])
		ni.podCapacity = parseInt(item.Status.Capacity["pods"])
		ni.gpuCapacity = parseInt(item.Status.Capacity[gpuResourceKey])
		ni.gpuAllocatable = parseInt(item.Status.Allocatable[gpuResourceKey])
		ni.unschedulable = item.Spec.Unschedulable
		totalGPUCapacity += ni.gpuCapacity
		totalGPUAllocatable += ni.gpuAllocatable
		for _, cond := range item.Status.Conditions {
			if cond.Type == "DiskPressure" && cond.Status == "True" {
				ni.diskPressure = true
			}
			if cond.Type == "Ready" && cond.Status == "True" {
				ni.ready = true
				ni.conditions = append(ni.conditions, "Ready")
			} else if cond.Type == "Ready" && cond.Status != "True" {
				ni.conditions = append(ni.conditions, "NotReady")
			}
		}
		if len(ni.conditions) == 0 {
			ni.conditions = []string{"Unknown"}
		}
		nodeMap[item.Metadata.Name] = ni
	}

	// Parse kubectl top nodes output.
	// Format: NAME  CPU(cores)  CPU%  MEMORY(bytes)  MEMORY%
	var nodes []ClusterHealthNode
	lines := strings.Split(strings.TrimSpace(string(topOut)), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		const topFieldCount = 5
		if len(fields) < topFieldCount {
			continue
		}
		name := fields[0]
		cpuUsed := parseTopCPU(fields[1])
		memUsed := parseTopMemory(fields[3])

		ni, ok := nodeMap[name]
		if !ok {
			continue
		}

		cpuCores := int(ni.cpuAllocatable / millicoresPerCore)
		cpuPct := 0
		if ni.cpuAllocatable > 0 {
			cpuPct = int(cpuUsed * percentMultiplier / ni.cpuAllocatable)
		}

		memTotalMB := ni.memAllocatable / bytesPerMB
		memUsedMB := memUsed / bytesPerMB
		memPct := 0
		if ni.memAllocatable > 0 {
			memPct = int(memUsed * percentMultiplier / ni.memAllocatable)
		}

		nodes = append(nodes, ClusterHealthNode{
			Name:          name,
			CPUCores:      cpuCores,
			CPUUsedMillis: cpuUsed,
			CPUPercent:    cpuPct,
			MemTotalMB:    memTotalMB,
			MemUsedMB:     memUsedMB,
			MemPercent:    memPct,
			DiskPressure:  ni.diskPressure,
			Pods:          0, // populated below
			PodCapacity:   ni.podCapacity,
			Conditions:    ni.conditions,
		})
	}

	// Collect LIVE node filesystem usage from each kubelet's stats/summary
	// endpoint. This is best-effort per node: a node whose kubelet proxy is
	// unreachable simply keeps nil disk fields and renders as unknown, which
	// must never degrade the rest of this cluster's health data.
	for i := range nodes {
		rawStats, statsErr := kubectlForClusterContext(ctx, cluster,
			"--request-timeout", timeout.String(),
			"get", "--raw", nodeStatsSummaryPath(nodes[i].Name)).Output()
		if statsErr != nil {
			if logger != nil {
				logger.Debug("cluster health: node disk stats unavailable",
					"cluster", cluster.ID, "node", nodes[i].Name, "error", statsErr)
			}
			continue
		}
		if usage, ok := parseNodeStatsSummaryDisk(rawStats); ok {
			applyNodeDiskUsage(&nodes[i], usage)
		}
	}

	// Count running pods per node and sum their container resource REQUESTS
	// (requests, not usage — that is what the scheduler bin-packs against).
	// Listing only Running pods slightly undercounts requests (Pending pods
	// already assigned to a node are missed), so the capacity estimate below
	// can be marginally optimistic.
	var cpuRequestedPerNode, memRequestedPerNode map[string]int64
	podOut, _ := kubectlForCluster(cluster, "get", "pods", "--all-namespaces", "--field-selector=status.phase=Running", "-o", "json").Output()
	if len(podOut) > 0 {
		var podsJSON struct {
			Items []struct {
				Metadata struct {
					Namespace string `json:"namespace"`
				} `json:"metadata"`
				Spec struct {
					NodeName   string `json:"nodeName"`
					Containers []struct {
						Resources struct {
							Requests map[string]string `json:"requests"`
						} `json:"resources"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"items"`
		}
		if json.Unmarshal(podOut, &podsJSON) == nil {
			podCounts := make(map[string]int)
			cpuRequestedPerNode = make(map[string]int64)
			memRequestedPerNode = make(map[string]int64)
			// hiveNamespacesPerNode tracks distinct hive-hosted-* namespaces per
			// node so each hive is counted once even with multiple pods.
			hiveNamespacesPerNode := make(map[string]map[string]bool)
			for _, p := range podsJSON.Items {
				podCounts[p.Spec.NodeName]++
				for _, c := range p.Spec.Containers {
					cpuRequestedPerNode[p.Spec.NodeName] += parseK8sCPU(c.Resources.Requests["cpu"])
					memRequestedPerNode[p.Spec.NodeName] += parseK8sMemory(c.Resources.Requests["memory"])
				}
				if strings.HasPrefix(p.Metadata.Namespace, hiveHostedNamespacePrefix) {
					if hiveNamespacesPerNode[p.Spec.NodeName] == nil {
						hiveNamespacesPerNode[p.Spec.NodeName] = make(map[string]bool)
					}
					hiveNamespacesPerNode[p.Spec.NodeName][p.Metadata.Namespace] = true
				}
			}
			for i := range nodes {
				nodes[i].Pods = podCounts[nodes[i].Name]
				nodes[i].HiveCount = len(hiveNamespacesPerNode[nodes[i].Name])
			}
		}
	}

	// Estimate remaining hive capacity: bin-pack the per-hive request
	// footprint into each Ready, schedulable node's free (allocatable minus
	// requested) capacity. Only computed when the pod listing above parsed,
	// since without per-node requests the estimate would be meaningless.
	var hiveCapacityRemaining *int
	if cpuRequestedPerNode != nil {
		var totalSlots int64
		for name, ni := range nodeMap {
			totalSlots += hiveSlotsForNode(ni.cpuAllocatable, ni.memAllocatable,
				cpuRequestedPerNode[name], memRequestedPerNode[name], ni.ready, ni.unschedulable)
		}
		slots := int(totalSlots)
		hiveCapacityRemaining = &slots
		// Headroom alert: warn operators before the cluster fills. Fires when
		// fewer than 10% of total estimated slots (current hives + remaining)
		// are left. Cheap to emit here — this path is cached for 30s and only
		// runs on health page loads.
		if total := hiveCount + slots; total > 0 && logger != nil {
			if slots*100 < total*capacityHeadroomWarnPct {
				logger.Warn("cluster hive capacity headroom low",
					"cluster", cluster.ID, "hives", hiveCount,
					"slots_remaining", slots, "headroom_pct", slots*100/total)
			}
		}
	}

	// Build summary.
	var totalCPUCores int
	var totalCPUUsed int64
	var totalCPUAlloc int64
	var totalMemAlloc int64
	var totalMemUsed int64
	for _, n := range nodes {
		totalCPUCores += n.CPUCores
		totalCPUUsed += n.CPUUsedMillis
		if ni, ok := nodeMap[n.Name]; ok {
			totalCPUAlloc += ni.cpuAllocatable
			totalMemAlloc += ni.memAllocatable
		}
		totalMemUsed += n.MemUsedMB * bytesPerMB
	}

	totalCPUPct := 0
	if totalCPUAlloc > 0 {
		totalCPUPct = int(totalCPUUsed * percentMultiplier / totalCPUAlloc)
	}
	totalMemPct := 0
	if totalMemAlloc > 0 {
		totalMemPct = int(totalMemUsed * percentMultiplier / totalMemAlloc)
	}
	totalMemGB := int(totalMemAlloc / giToBytes)
	totalDiskGB, totalDiskPct := summarizeDisk(nodes)

	result := PerClusterHealth{
		Nodes: nodes,
		Summary: ClusterHealthSummary{
			TotalNodes:            len(nodes),
			TotalCPUCores:         totalCPUCores,
			TotalCPUPct:           totalCPUPct,
			TotalMemGB:            totalMemGB,
			TotalMemPct:           totalMemPct,
			TotalDiskGB:           totalDiskGB,
			TotalDiskPct:          totalDiskPct,
			HiveCount:             hiveCount,
			HiveCapacityRemaining: hiveCapacityRemaining,
		},
		HiveCount: hiveCount,
	}

	// Include GPU summary for clusters with GPUs.
	if totalGPUCapacity > 0 {
		result.GPUSummary = &GPUSummary{
			TotalGPUs:       totalGPUCapacity,
			AllocatableGPUs: totalGPUAllocatable,
		}
	}

	// Orphaned Terminating-pod count (#5328 item 3). READ-ONLY: one extra
	// `kubectl get pods` on a path that already lists pods. It needs its own
	// listing because the query above is field-selected to phase=Running and
	// therefore cannot see an orphan by construction.
	//
	// Best-effort: nil on failure, so a cluster the hub could not interrogate
	// reports UNKNOWN rather than a reassuring zero.
	if stuck := collectStuckPods(ctx, cluster, timeout, time.Now()); stuck != nil {
		result.StuckPods = stuck
		// Log when the fleet is actually accumulating orphans. The reaper
		// clears them, so a persistently non-zero count here means orphans are
		// being PRODUCED faster than they age past orphanedPodMinAge — which is
		// the upstream node-lifecycle fault (#5328 item 1), not a reaper
		// problem. Silence on zero keeps a healthy fleet quiet.
		if stuck.Total > 0 && logger != nil {
			logger.Warn("cluster has hive pods stuck terminating — check for ungraceful node loss",
				"cluster", cluster.ID,
				"stuck_pods", stuck.Total,
				"namespaces_affected", stuck.NamespacesAffected)
		}
	}

	// Leaked hosted-namespace count (#5768 ask 3). READ-ONLY: one extra
	// `kubectl get namespaces`. It cannot reuse any listing above — the pod
	// queries cannot see a namespace whose pods are gone, and the
	// registry-derived sweeps cannot see a namespace with no registry entry by
	// construction, which is exactly the leak class.
	//
	// Best-effort: nil on failure or on an empty registry, so a cluster the hub
	// could not interrogate reports UNKNOWN rather than a reassuring zero.
	if leaked := collectLeakedHostedNamespaces(ctx, cluster, timeout, knownHostedNamespaces, time.Now(), logger); leaked != nil {
		result.LeakedNamespaces = leaked
		// A non-zero count is a standing quota/PVC leak on a shared cluster, and
		// permanent noise in any surface that reads pod issues — the console
		// canary that found this read 76 stuck pods across these namespaces.
		// Nothing deletes them, so this stays warm until a human acts.
		if leaked.Total > 0 && logger != nil {
			logger.Warn("cluster holds hive-hosted namespaces with no hive record — leaked provisioning namespaces, nothing will reclaim them",
				"cluster", cluster.ID,
				"leaked_namespaces", leaked.Total)
		}
	}

	// Wildcard certificate health (#5977). READ-ONLY: one extra `kubectl get
	// secret`, and ONLY on clusters that opted in with wildcard_tls_secret —
	// everywhere else spokes still carry per-host certificates and there is
	// nothing here to be a single point of failure.
	//
	// This is also the only place the operator's opt-in assertion is ever
	// checked against the cluster. The provisioner cannot check it (it must
	// decide without a round-trip, and guessing wrong takes the cluster down),
	// so until now "flag set, secret absent" was silent until a user opened a
	// dashboard and got a self-signed certificate.
	if wildcard := collectWildcardTLSHealth(ctx, cluster, timeout, time.Now(), logger); wildcard != nil {
		result.WildcardTLS = wildcard
		// Every non-ok status is worth a line: unlike the two signals above,
		// which count things that accumulate, this one is binary and fleet-wide
		// — when it is wrong, every hosted dashboard on the cluster is already
		// serving a certificate that does not validate.
		if !wildcard.Healthy() && logger != nil {
			logger.Warn("wildcard TLS certificate needs attention — it serves EVERY wildcard-covered spoke on this cluster",
				"cluster", cluster.ID,
				"secret", wildcard.Secret,
				"status", wildcard.Status,
				"detail", wildcard.Detail,
				"not_after", wildcard.NotAfter)
		}
	}

	return result, nil
}

// getHeartbeatHealthForCluster retrieves the latest heartbeat-reported health
// for a cluster. Returns nil if no data exists or the data is too old.
func (s *HubServer) getHeartbeatHealthForCluster(clusterID string) *HeartbeatHealthEntry {
	s.heartbeatHealthMu.RLock()
	entry, ok := s.heartbeatHealth[clusterID]
	s.heartbeatHealthMu.RUnlock()
	if !ok || entry == nil || entry.Report == nil {
		return nil
	}
	return entry
}

// convertHeartbeatToPerClusterHealth converts heartbeat-reported health data
// into the hub's PerClusterHealth format for display. If the data is older
// than heartbeatHealthStaleness, it is marked with a staleness warning.
func convertHeartbeatToPerClusterHealth(clusterID, clusterName string, entry *HeartbeatHealthEntry, hiveCount int) PerClusterHealth {
	report := entry.Report

	// Convert HeartbeatNodeMetric to ClusterHealthNode.
	nodes := make([]ClusterHealthNode, len(report.Nodes))
	for i, n := range report.Nodes {
		nodes[i] = ClusterHealthNode{
			Name:          n.Name,
			CPUCores:      n.CPUCores,
			CPUUsedMillis: n.CPUUsedMillis,
			CPUPercent:    n.CPUPercent,
			MemTotalMB:    n.MemTotalMB,
			MemUsedMB:     n.MemUsedMB,
			MemPercent:    n.MemPercent,
			DiskPressure:  n.DiskPressure,
			DiskTotalMB:   n.DiskTotalMB,
			DiskUsedMB:    n.DiskUsedMB,
			DiskPercent:   n.DiskPercent,
			Pods:          n.Pods,
			PodCapacity:   n.PodCapacity,
			HiveCount:     n.HiveCount,
			Conditions:    n.Conditions,
		}
	}

	pch := PerClusterHealth{
		ID:    clusterID,
		Name:  clusterName,
		Nodes: nodes,
		Summary: ClusterHealthSummary{
			TotalNodes:    report.Summary.TotalNodes,
			TotalCPUCores: report.Summary.TotalCPUCores,
			TotalCPUPct:   report.Summary.TotalCPUPct,
			TotalMemGB:    report.Summary.TotalMemGB,
			TotalMemPct:   report.Summary.TotalMemPct,
			// nil for spokes that could not read kubelet disk stats.
			TotalDiskGB:  report.Summary.TotalDiskGB,
			TotalDiskPct: report.Summary.TotalDiskPct,
			HiveCount:    hiveCount,
			// nil for spokes running older builds that do not report it.
			HiveCapacityRemaining: report.Summary.HiveCapacityRemaining,
		},
		HiveCount:  hiveCount,
		DataSource: "heartbeat",
	}

	// Mark staleness if heartbeat data is too old.
	age := time.Since(entry.ReceivedAt)
	if age > heartbeatHealthStaleness {
		pch.DataStale = true
		pch.DataAge = fmt.Sprintf("%dm ago", int(age.Minutes()))
	} else if report.CollectedAt != "" {
		pch.DataAge = report.CollectedAt
	}

	// Convert GPU summary.
	if report.GPUSummary != nil {
		pch.GPUSummary = &GPUSummary{
			TotalGPUs:       report.GPUSummary.Total,
			AllocatableGPUs: report.GPUSummary.Total - report.GPUSummary.Allocated,
		}
	}

	return pch
}

// nodeDiskUsage holds live node filesystem usage for one node.
type nodeDiskUsage struct {
	usedBytes     int64
	capacityBytes int64
}

// nodeStatsSummaryPath builds the kubelet stats/summary proxy path for a node.
// This endpoint is the only source of LIVE disk usage: the node object's
// capacity/allocatable["ephemeral-storage"] reports the declared size only and
// says nothing about how full the filesystem actually is.
func nodeStatsSummaryPath(nodeName string) string {
	return "/api/v1/nodes/" + nodeName + "/proxy/stats/summary"
}

// parseNodeStatsSummaryDisk extracts node filesystem usage from a kubelet
// stats/summary response. node.fs is the nodefs that kubelet's
// evictionHard nodefs.available threshold applies to.
func parseNodeStatsSummaryDisk(raw []byte) (nodeDiskUsage, bool) {
	var summary struct {
		Node struct {
			FS struct {
				UsedBytes     *int64 `json:"usedBytes"`
				CapacityBytes *int64 `json:"capacityBytes"`
			} `json:"fs"`
		} `json:"node"`
	}
	if err := json.Unmarshal(raw, &summary); err != nil {
		return nodeDiskUsage{}, false
	}
	fs := summary.Node.FS
	// Both values are required: without capacity there is no percentage, and
	// a missing usedBytes must not be treated as zero usage.
	if fs.UsedBytes == nil || fs.CapacityBytes == nil || *fs.CapacityBytes <= 0 {
		return nodeDiskUsage{}, false
	}
	return nodeDiskUsage{usedBytes: *fs.UsedBytes, capacityBytes: *fs.CapacityBytes}, true
}

// applyNodeDiskUsage fills the disk fields on a health node from live usage.
// Nodes with no usable stats keep nil disk fields and render as unknown.
func applyNodeDiskUsage(n *ClusterHealthNode, d nodeDiskUsage) {
	totalMB := d.capacityBytes / bytesPerMB
	usedMB := d.usedBytes / bytesPerMB
	pct := int(d.usedBytes * percentMultiplier / d.capacityBytes)
	n.DiskTotalMB = &totalMB
	n.DiskUsedMB = &usedMB
	n.DiskPercent = &pct
}

// summarizeDisk aggregates per-node disk usage into cluster totals, counting
// only nodes that actually reported usage. Returns nil,nil when no node did,
// so the UI omits disk for that cluster rather than showing a false 0%.
func summarizeDisk(nodes []ClusterHealthNode) (*int, *int) {
	var totalBytes, usedBytes int64
	for _, n := range nodes {
		if n.DiskTotalMB == nil || n.DiskUsedMB == nil {
			continue
		}
		totalBytes += *n.DiskTotalMB * bytesPerMB
		usedBytes += *n.DiskUsedMB * bytesPerMB
	}
	if totalBytes <= 0 {
		return nil, nil
	}
	totalGB := int(totalBytes / giToBytes)
	pct := int(usedBytes * percentMultiplier / totalBytes)
	return &totalGB, &pct
}

// parseK8sCPU parses Kubernetes CPU resource strings (e.g. "4", "4000m", "5866711668n").
// Returns millicores.
func parseK8sCPU(s string) int64 {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "n") {
		v, _ := strconv.ParseInt(strings.TrimSuffix(s, "n"), 10, 64)
		const nanocoresPerMillicore = 1_000_000
		return v / nanocoresPerMillicore
	}
	if strings.HasSuffix(s, "m") {
		v := parseInt(strings.TrimSuffix(s, "m"))
		return int64(v)
	}
	v := parseInt(s)
	return int64(v) * millicoresPerCore
}

// parseK8sMemory parses Kubernetes memory resource strings (e.g. "16384Ki", "8Gi").
func parseK8sMemory(s string) int64 {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "Ki") {
		v := parseInt(strings.TrimSuffix(s, "Ki"))
		return int64(v) * kiToBytes
	}
	if strings.HasSuffix(s, "Mi") {
		v := parseInt(strings.TrimSuffix(s, "Mi"))
		return int64(v) * miToBytes
	}
	if strings.HasSuffix(s, "Gi") {
		v := parseInt(strings.TrimSuffix(s, "Gi"))
		return int64(v) * giToBytes
	}
	return int64(parseInt(s))
}

// parseTopCPU parses kubectl top CPU values (e.g. "1200m", "2").
func parseTopCPU(s string) int64 {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "m") {
		v := parseInt(strings.TrimSuffix(s, "m"))
		return int64(v)
	}
	v := parseInt(s)
	return int64(v) * millicoresPerCore
}

// parseTopMemory parses kubectl top memory values (e.g. "4096Mi", "8Gi").
func parseTopMemory(s string) int64 {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "Ki") {
		v := parseInt(strings.TrimSuffix(s, "Ki"))
		return int64(v) * kiToBytes
	}
	if strings.HasSuffix(s, "Mi") {
		v := parseInt(strings.TrimSuffix(s, "Mi"))
		return int64(v) * miToBytes
	}
	if strings.HasSuffix(s, "Gi") {
		v := parseInt(strings.TrimSuffix(s, "Gi"))
		return int64(v) * giToBytes
	}
	return int64(parseInt(s))
}

// parseInt parses an integer from a string, returning 0 on failure.
func parseInt(s string) int {
	var v int
	_, _ = fmt.Sscanf(s, "%d", &v)
	return v
}

type MyHiveEntry struct {
	RegistryEntry
	Role        string `json:"role"`
	ProvError   string `json:"provError,omitempty"`
	ProvStatus  string `json:"provStatus,omitempty"`
	AutoUpgrade bool   `json:"autoUpgrade"`
	// OwnerName is the resolved human display label for an opaque OIDC Owner
	// identity ("ibmid:5500…" → "Jane Doe"), stamped at serve time from the
	// stored user record. Empty when Owner is already the best label (GitHub
	// logins). Purely cosmetic: grouping labels and tooltips show it; every
	// key, filter and authorization check stays on the raw Owner.
	OwnerName string `json:"ownerName,omitempty"`
	// TrackedChannel is the release channel this hive's image is pinned to
	// ("stable", "candidate", "edge"), or "" for a plain-branch hive. Overlaid
	// at read time from the hub-owned SaaSHive record — deliberately NOT from
	// the registry, whose GitBranch the spoke rewrites every beat with the
	// image's baked-in branch (a channel retag of a v4 build heartbeats "v4").
	// When set, the dashboard's version pill and picker treat it as the
	// current selection (rendered via versionLabel as "stable (v4)") while
	// gitBranch keeps driving everything about the code actually running.
	TrackedChannel string `json:"trackedChannel,omitempty"`
	// AutoUpgradeMode is always sent NORMALIZED (never empty when autoUpgrade is
	// on) so the dashboard can render the effective mode without re-deriving the
	// legacy empty-means-instant rule in JavaScript.
	AutoUpgradeMode     string                 `json:"autoUpgradeMode,omitempty"`
	PendingRequestCount int                    `json:"pendingRequestCount,omitempty"`
	PendingRequests     []PendingAccessRequest `json:"pending_requests,omitempty"`

	// Access lists who can sign in to this hive and with what role, so My Hives
	// can show it on hover without a per-row API call. Same data as
	// GET /hives/{id}/access, and populated under the same authorization rule:
	// only for rows the caller owns (or admin). Nil on rows the caller merely
	// has delegated access to — knowing who else shares a hive is the owner's
	// information, not every reader's.
	Access []HiveAccessEntry `json:"access,omitempty"`

	// Assigning is true while a freshly-assigned placeholder's spoke has not yet
	// reported the real project via heartbeat: the meta.json already records the
	// real project (org/repos/ACMM set, status no longer "available") but the live
	// registry entry still shows the placeholder identity. It flips false — and the
	// dashboard spinner clears — once the spoke reconciles and the registry reports
	// the real project. AssigningTo is the target org so the row can say "Assigning
	// to <org>". This is exactly the condition projectConfigForHiveID keeps sending
	// its reconcile under.
	Assigning   bool   `json:"assigning,omitempty"`
	AssigningTo string `json:"assigningTo,omitempty"`

	// AssignedUnclaimed marks a placeholder wedged at Status=statusAssigned &&
	// !ClaimDelivered: a claim was stamped but the spoke never reported the
	// project back. Unlike Assigning — which is true only while a REACHABLE spoke
	// is actively reporting a DIFFERENT project — this is true even when the spoke
	// is offline or silent (the frozen-image / heartbeat-only case), which is
	// exactly the dead-end the Reset assignment action exists for. It gates that
	// admin-only action in the row menu.
	AssignedUnclaimed bool `json:"assignedUnclaimed,omitempty"`

	// Unassigned marks an unclaimed pool placeholder (statusAvailable or the
	// legacy available-* org fallback). Fleet hides these idle capacity rows by
	// default so real tenant hives drive the attention count.
	Unassigned bool `json:"unassigned,omitempty"`

	// AssignedAt is the RFC3339 timestamp the placeholder was last assigned/claimed
	// (SaaSHive.AssignedAt). It rides the row payload ONLY for a hive that is still
	// AssignedUnclaimed, so the dashboard can render a live "claim pending" counter
	// measuring how long the slot has been wedged. It is the same clock the
	// assign-stuck self-heal sweep measures against, so the row's amber "stuck"
	// threshold cannot drift from the reset timeout. Empty for every other row.
	AssignedAt string `json:"assignedAt,omitempty"`

	// AssignStuckSeconds is assignStuckResetTimeout expressed in whole seconds, so
	// the dashboard's "stuck / about to auto-reset" tint reuses the SAME threshold
	// the self-heal sweep enforces rather than hardcoding a duplicate in JS. Sent
	// only alongside AssignedAt (i.e. for an assigned-but-unclaimed row).
	AssignStuckSeconds int `json:"assignStuckSeconds,omitempty"`

	// Drift holds config-drift signals computed server-side against the fleet
	// norm (see drift.go). It rides this payload rather than a per-row API call
	// so the My Hives table can render the badge and the fleet-exceptions
	// summary from data it already has.
	Drift DriftReport `json:"drift"`

	// RecentEvents carries the newest few timeline events so the My Hives
	// status hover can show recent activity WITHOUT a per-row fetch.
	//
	// Embedding rather than lazy-fetching is deliberate. The hover is a
	// transient, high-frequency interaction: sweeping the pointer down a
	// 42-row table would fire 42 requests, each of which hits the filesystem
	// via loadSaaSHive in handleHiveTimeline. No debounce or TTL cache removes
	// that — scanning the list IS the normal way the table is read, so the
	// requests are the common case, not the edge case.
	//
	// The cost of embedding was measured rather than guessed: at
	// myHivesRecentEventCount events per row a typical fleet of 42 hives adds
	// ~14 KB, and ~50 KB in the pathological case where every event carries a
	// maxed-out timelineMaxDetailRunes detail. That is small beside what a row
	// already ships (health map, drift report, access list, agents,
	// leaderboard and the issue/PR spark histories), and the events are
	// already in memory hub-side, so serving them costs no extra I/O.
	//
	// Populated under the SAME authorization as Access — owner or admin only —
	// because it is the same per-hive operational detail handleHiveTimeline
	// guards. The full 200-event history stays behind that endpoint and its
	// modal; this is only the hover preview.
	RecentEvents []TimelineEvent `json:"recentEvents,omitempty"`

	// AdvisoryIssueActivity is the fleet row's advisory-digest freshness: the
	// newest successful digest update already reported to the hub, bucketed on
	// read with the same thresholds and gates as the advisory-stale verdict.
	// Placeholders and old/non-advisory spokes with no signal still get the
	// field with bucket "unknown" so every row renders an explicit n/a instead
	// of disappearing.
	AdvisoryIssueActivity AdvisoryIssueActivity `json:"advisoryIssueActivity"`

	// BudgetHealth is this hive's current governor budget-window usage, bucketed
	// for the fleet row. It includes the underlying spend/limit/window numbers so
	// the UI can explain the dot without reverse-engineering RegistryEntry.
	BudgetHealth BudgetHealth `json:"budgetHealth"`

	// GitHubAppHealth is this hive's GitHub App token/auth health for the fleet
	// row, bucketed server-side so every consumer shares the same thresholds and
	// problem semantics.
	GitHubAppHealth GitHubAppHealth `json:"githubAppHealth"`

	// AdvisoryStale is true when this hive SHOULD be posting advisory digests
	// but its digest has quietly gone stale — computed on read by advisoryStale()
	// so the browser never re-derives the threshold or the gating (advisory-mode
	// + app-can-write) and cannot drift from the Go rule. AdvisoryStaleReason is
	// the tooltip cause. Both stay zero/empty for hives that are not in advisory
	// mode, whose App cannot write, or that report an unknown timestamp — those
	// must never show the pill.
	AdvisoryStale       bool   `json:"advisoryStale,omitempty"`
	AdvisoryStaleReason string `json:"advisoryStaleReason,omitempty"`

	// AutoUpgradeBlocked is true when this hive has auto-upgrade ON but the hub
	// will REFUSE to arm it: upgradeCollectible() is false, so the spoke cannot
	// pull the instruction off its own heartbeat and triggerAutoUpgrades()
	// declines every cycle. AutoUpgradeBlockedReason is the operator-facing
	// cause from uncollectibleUpgradeReason() — the SAME string
	// noteUncollectibleUpgrade() writes to the timeline, and documented there as
	// free of credentials and kubeconfig paths, so it is safe as a tooltip.
	//
	// WHY THIS IS COMPUTED ON READ RATHER THAN RE-DERIVED IN JAVASCRIPT. The
	// fleet row used to render "Queued for auto-upgrade · 1pm ET" from
	// autoUpgradeMode alone, which consults nothing about eligibility. A hive
	// the hub had permanently refused therefore advertised a queued upgrade
	// while the timeline recorded the refusal — two surfaces, opposite stories,
	// and no way to tell "waiting for the window" from "will never fire". The
	// browser must not re-implement the predicate or its staleRemoveAge bound;
	// sending the evaluated decision is what keeps badge and hub in agreement.
	//
	// DELIBERATELY ONLY THE REFUSED GATE. The other gates in
	// triggerAutoUpgrades() (claim in flight, wave full, provisioning, the
	// schedule itself) are TRANSIENT — they clear on their own, so a badge
	// saying "queued" is eventually true. Uncollectible is the one state that
	// never resolves without operator action, which is why it is the one worth
	// naming distinctly.
	AutoUpgradeBlocked       bool   `json:"autoUpgradeBlocked,omitempty"`
	AutoUpgradeBlockedReason string `json:"autoUpgradeBlockedReason,omitempty"`

	// The inference-backend auth-failure signal (InferenceAuthError) is NOT
	// re-declared here: MyHiveEntry embeds RegistryEntry, which already carries
	// the spoke-reported InferenceAuthError verbatim, so the promoted field is
	// what both the alert evaluator (alertHiveFromEntry) and the JSON payload
	// read. The spoke owns the consecutive-failure threshold and the self-heal,
	// so there is nothing to compute on read the way AdvisoryStale is computed.
	CommitsBehindStableV4 *int `json:"commitsBehindStableV4,omitempty"`

	// InactiveAgents is how many of this hive's agents are RUNNING but not
	// doing any work — session gone, sitting on a login prompt, or producing
	// nothing while work is queued. Computed on read by
	// evaluateInactiveAgents() so the browser never re-derives the thresholds
	// or the paused/on-demand gating and cannot drift from the Go rule.
	//
	// Agents the operator deliberately PAUSED are excluded by that rule and
	// never counted here: a pause is a choice, not a fault, and a facet that
	// alarms on it would be wrong on every hive with a parked agent.
	// InactiveAgentsReason is the tooltip cause. Both stay zero/empty for
	// hives with nothing wrong, so the pill and the facet self-suppress.
	InactiveAgents       int    `json:"inactiveAgents,omitempty"`
	InactiveAgentsReason string `json:"inactiveAgentsReason,omitempty"`

	// AllAgentsQuiet is true when EVERY agent this hive reports is deliberately
	// quiet — paused or off-schedule. This is "hive not in use": nothing is
	// broken, nothing will be produced, and the same condition suppresses the
	// advisory-staleness pill (allAgentsQuietByDesign). Computed on read so the
	// browser never re-derives the pause/off-schedule rule; the fleet page
	// renders it as a distinct state chip rather than health or fault.
	AllAgentsQuiet bool `json:"allAgentsQuiet,omitempty"`

	// FleetRollup / AgentVerdicts carry the three-way divergence view — what the
	// governor EXPECTS running, what is ACTUALLY running, and what is ABLE to
	// fulfill its mission — computed on read from the per-agent heartbeat
	// signals + this hive's blocker fields. FleetRollup is the per-spoke header
	// ("expects N · M running · K able"); AgentVerdicts is the per-agent
	// drill-down. Both stay nil for a hive with no reported agents. Computed on
	// read (deriveAgentVerdict/rollupAgents) so the browser never re-derives the
	// state machine and cannot drift from the Go rule.
	FleetRollup   *agentFleetRollup  `json:"fleetRollup,omitempty"`
	AgentVerdicts []AgentVerdictJSON `json:"agentVerdicts,omitempty"`

	// AgentRosterMismatch is an additive warning when the spoke's reported
	// agent list no longer matches the ACMM pack roster for its level. It does
	// not change the red/green hive verdict; red production failures still
	// outrank this yellow configuration-drift signal.
	AgentRosterMismatch *agentRosterMismatch `json:"agentRosterMismatch,omitempty"`

	// HealthVerdict is the at-a-glance hive-health verdict (hive-health): does
	// this spoke have RECENT OUTPUT back to its work source, banded by ACMM
	// level? green/red/unknown with a WHY reason, computed on read from the same
	// rollup/app-health/queue/advisory/repo-activity signals the row already
	// carries. nil for placeholder rows (nothing to judge). Named distinctly
	// from the embedded RegistryEntry.Health (the raw spoke-reported blob) to
	// avoid shadowing it. See health_verdict.go.
	HealthVerdict *HealthVerdict `json:"healthVerdict,omitempty"`

	// URLUnreachable is true when this hive's PUBLIC dashboard URL failed to
	// serve on the last several probes — the link in this very table is dead.
	// Computed on read from the auth-audit loop's observations, so the browser
	// never re-derives the failure threshold. Stays zero/empty for a hive that
	// is serving, that is too new to have converged, or whose whole cluster is
	// out (an outage is one condition, not N broken hives) — those must never
	// show the pill.
	URLUnreachable       bool   `json:"urlUnreachable,omitempty"`
	URLUnreachableReason string `json:"urlUnreachableReason,omitempty"`
	// PrivateURL is true when the hub's public-network probe cannot reach the
	// dashboard URL, but the hive is freshly heartbeating and does not report a
	// self-check failure. It renders as an informational "private URL" chip,
	// not a critical dead-link chip.
	PrivateURL       bool   `json:"privateUrl,omitempty"`
	PrivateURLReason string `json:"privateUrlReason,omitempty"`

	// Quadrant is this hive's four-axis score — trust, efficiency,
	// satisfaction, productivity — computed on read and never persisted.
	//
	// It lives HERE rather than on RegistryEntry (where Journey sits) because
	// unlike every other derived field on a row, a quadrant is not a property
	// of the hive alone: the scores are percentiles against the other hives in
	// the SAME view. Two requests over different filters legitimately produce
	// different numbers for one hive, so caching it on the shared registry
	// entry would let one caller's filtered population leak into another's.
	//
	// Nil when the caller is not entitled to see it, or when the population is
	// too small to rank honestly — the browser renders nothing at all in that
	// case rather than an empty chart.
	Quadrant *Quadrant `json:"quadrant,omitempty"`
}

// myHivesRecentEventCount is how many timeline events ride the My Hives
// payload for the status hover. The hover panel already carries a status word,
// the per-check health lines, the relay line and the user list; three events
// is enough to answer "what just happened to this hive?" without turning a
// transient tooltip into a scrolling log. "See all" opens the full modal.
const myHivesRecentEventCount = 3

func (s *HubServer) handleMyHives(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	if username == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	user := ensureSaaSUser(username)

	s.mu.Lock()
	offlineEvents := s.markStaleHives()
	allHives := make([]RegistryEntry, len(s.registry.Hives))
	copy(allHives, s.registry.Hives)
	s.mu.Unlock()
	s.flushOfflineEvents(offlineEvents)

	var result []MyHiveEntry

	autoUpgradeMap := make(map[string]bool)
	// Normalized here (empty → instant) so every consumer sees the effective
	// mode rather than the raw legacy blank.
	autoUpgradeModeMap := make(map[string]string)
	saasByID := make(map[string]*SaaSHive)
	for _, sh := range listSaaSHives() {
		shCopy := sh
		autoUpgradeMap[sh.ID] = sh.AutoUpgrade
		autoUpgradeModeMap[sh.ID] = normalizeAutoUpgradeMode(sh.AutoUpgradeMode)
		saasByID[sh.ID] = &shCopy
	}

	// enrichFromSaaSMeta overlays SaaS meta.json fields onto an entry built from
	// a live registry hive (registry entries come from spoke heartbeats and carry
	// NO provStatus, so provStatus/migration/error must come from meta).
	//
	// ACMM level is the subtle case, and getting it wrong caused a regression:
	//   - For a CLAIMED / running hive, the spoke's LIVE heartbeat level is
	//     authoritative — it's what the hive is actually running. Many pre-claim
	//     meta.json records still carry a stale acmm_level: 0 even though the
	//     spoke runs at a real level, so unconditionally taking the meta level
	//     downgraded live hives to L0 on the dashboard.
	//   - For an unclaimed PLACEHOLDER (status: available), there is no
	//     meaningful live level (the slot reports L0), so the INTENDED level from
	//     meta ("L2 Advisory") is what should show, and it also gates the
	//     "Assign" menu (provStatus === 'available').
	// Rule: take the meta level only for placeholders, or as a fallback when the
	// live registry level is 0 (unknown) but meta has a real one. Otherwise the
	// live registry level wins.

	enrichFromSaaSMeta := func(entry *MyHiveEntry) {
		sh := saasByID[entry.ID]
		if sh == nil {
			return
		}
		entry.ProvStatus = sh.Status
		// The tracked release channel comes from meta, never the registry: the
		// spoke's heartbeat rewrites GitBranch with the image's baked-in branch
		// every beat, which is exactly how the channel selection was being
		// forgotten. Read-time overlay also means the pill flips to the channel
		// on the very next poll after the switch, without waiting for a beat.
		entry.TrackedChannel = sh.TrackedChannel
		// Overlay the hosted namespace at read time too, so a placeholder or a
		// hive whose live registry entry predates the field still shows
		// "hive-hosted-<id>" in My Hives. Derived from the SaaSHive record, same
		// as the heartbeat path — the value the operator needs for kubectl exec.
		entry.Namespace = hostedNamespaceForHive(sh)
		// A placeholder wedged between the two claim paths: it was given a real
		// identity (org/repo written) but the spoke never reported the project back.
		// Computed from meta (not the live registry) so it is true even for an
		// offline/silent spoke — the exact dead-end the Reset assignment action
		// targets.
		//
		// The predicate is deliberately broader than "Status == statusAssigned".
		// The current assign paths (handleApproveProvision / handleAssignHive) always
		// stamp Status=statusAssigned alongside the org/repo, but LEGACY/wedged
		// placeholders exist that carry a REAL org/primary_repo with Status left NULL
		// or "" (e.g. the live hosted-available-vllmd-13: org=z-innersource,
		// repo=AutoIPL, status=null) — precisely the wedge the escape hatch was built
		// to rescue. Keying only on statusAssigned HID the Reset item for exactly
		// those slots.
		//
		// A clean available slot is NOT bare — a seeded/reset placeholder carries the
		// synthetic "available-<id>" org (placeholderOrgPrefix) and an empty
		// primary_repo, which is how isPlaceholderEntry recognizes inventory. So "has
		// a real assigned identity" means a non-empty org that is NOT the placeholder
		// prefix, or any primary_repo. Treat a placeholder as assigned-unclaimed
		// whenever it is NOT a delivered claim (that is a LIVE hive) AND is NOT a
		// clean available slot but HAS been handed a real identity (Status==assigned,
		// or a real org / primary_repo written under any/no status).
		hasRealOrg := sh.Org != "" && !strings.HasPrefix(sh.Org, placeholderOrgPrefix)
		notClaimed := !sh.ClaimDelivered
		hasAssignedIdentity := hasRealOrg || sh.PrimaryRepo != ""
		isAssignedStatus := sh.Status == statusAssigned
		isCleanAvailable := sh.Status == statusAvailable && !hasAssignedIdentity
		entry.AssignedUnclaimed = notClaimed && !isCleanAvailable && (isAssignedStatus || hasAssignedIdentity)
		// Ride the assignment clock and the self-heal threshold ONLY on an
		// assigned-but-unclaimed row, so the dashboard can tick a "claim pending"
		// counter and tint it amber as it nears the auto-reset the sweep enforces.
		// Both stay zero/empty otherwise so the counter self-suppresses. Uses the
		// broadened AssignedUnclaimed above, so the counter also covers null-Status
		// wedges — the same rows the Reset action now reaches.
		if entry.AssignedUnclaimed {
			entry.AssignedAt = sh.AssignedAt
			entry.AssignStuckSeconds = int(assignStuckResetTimeout / time.Second)
		}
		if sh.Status == statusAvailable || (entry.ACMMLevel == 0 && sh.ACMMLevel > 0) {
			entry.ACMMLevel = sh.ACMMLevel
		}
		// Show the friendly vanity host for a claimed hive the instant it is
		// claimed, rather than waiting for the spoke to adopt+report it back over
		// the heartbeat. Until then entry.DashboardURL (spoke-reported) is still the
		// raw placeholder host, which is the placeholder-URL-persists bug in My
		// Hives / the row's Open link. Only overlays a validated meta vanity_url;
		// an unclaimed placeholder or a hive with no vanity keeps its own URL.
		if v := claimedVanityURL(sh); v != "" {
			entry.DashboardURL = v
		}
		switch sh.Status {
		case "provisioning":
			entry.GovernorMode = "PROVISIONING"
		case "error":
			entry.GovernorMode = "ERROR"
			entry.ProvError = sh.Error
		}

		// Assigning transient state: after a placeholder is assigned, the meta
		// records the real project but the spoke still reports the old placeholder
		// identity until it reconciles via heartbeat.
		//
		// projectConfigForHiveID returns non-nil whenever meta != what the spoke
		// reports — but that ALSO happens transiently during an UPGRADE (the spoke
		// pod restarts and its heartbeat momentarily reports an empty/stale org),
		// which falsely lit "Assigning to <org>" on already-claimed hives that were
		// merely upgrading. Guard against that:
		//   - never show Assigning while the hive is Upgrading (an upgrade is not
		//     an assignment), and
		//   - only show it when the spoke is genuinely reporting a DIFFERENT,
		//     non-empty project (an empty reported org means the spoke just
		//     restarted / hasn't beaten yet — not a fresh assignment).
		spokeReportsDifferentProject := entry.Org != "" && !strings.EqualFold(entry.Org, sh.Org)
		if !entry.Upgrading && spokeReportsDifferentProject &&
			// Empty curAPIURL deliberately: a GHE API-URL-only push is a config
			// repair, not an assignment, and must never light "Assigning to <org>".
			projectConfigForHiveID(entry.ID, entry.Org, entry.Repos, entry.PrimaryRepo, entry.ACMMLevel, entry.DashboardURL, "") != nil {
			entry.Assigning = true
			entry.AssigningTo = sh.Org
		}
	}

	// resolveGitHubHost fills in the Location-column GitHub host pill when the
	// spoke did not report one over the heartbeat.
	//
	// The spoke-reported value always wins: it is the hive's real runtime
	// GitHub, and it is the only source that is correct when a hive's GitHub
	// differs from its cluster's default (the enricom8 hive is the live
	// example — its host had to be corrected by hand). Heartbeat-only and
	// firewalled spokes upgrade slowly, so for those this falls back to the
	// hive's recorded host, then its cluster's default. Anything still empty
	// is left empty and rendered as "github.com" by the pill, so an old spoke
	// shows a sane default rather than a wrong host.
	resolveGitHubHost := func(entry *MyHiveEntry) {
		if entry.GitHubHost != "" {
			return // spoke-reported — authoritative
		}
		if sh := saasByID[entry.ID]; sh != nil {
			if sh.GitHubHost != "" {
				entry.GitHubHost = githubHostLabel(sh.GitHubHost)
				return
			}
			if c := s.clusterForHive(sh); c != nil && (c.GitHubBaseURL != "" || c.GitHubAPIURL != "") {
				// base-or-api so a GHE cluster recorded with only an api_url
				// (blank base_url — the common state) is recognised as GHE, not
				// mislabelled github.com.
				entry.GitHubHost = clusterGitHubConfig(c).HostLabel()
				return
			}
		}
		// No meta record: fall back to the cluster the registry entry reports.
		// s.clusters is read unlocked here to match every other reader in this
		// package (clusterForHive, clusterNameForID); it is effectively
		// immutable after load, and taking s.mu here would nest inside callers
		// that already hold it.
		if entry.ClusterID != "" {
			if c, ok := s.clusters[entry.ClusterID]; ok && (c.GitHubBaseURL != "" || c.GitHubAPIURL != "") {
				entry.GitHubHost = clusterGitHubConfig(&c).HostLabel()
			}
		}
	}

	isAdmin := isHubAdmin(username)
	for _, h := range allHives {
		if role, ok := user.Hives[h.ID]; ok {
			// A stale/demoted stored role must not hide owner-gated UI (the
			// Upgrade link, auto-upgrade controls) from the hive's TRUE owner.
			// Normalize to owner for the admin (as before) AND for the
			// canonical owner of this hive — owners are only elevated on their
			// OWN hives (#4081).
			if role != "owner" && canonicalEqual(h.Owner, username) {
				role = "owner"
				user.Hives[h.ID] = "owner" // heal the demoted stored role
			}
			if isAdmin && role != "owner" {
				role = "owner"
			}
			entry := MyHiveEntry{RegistryEntry: h, Role: role, AutoUpgrade: autoUpgradeMap[h.ID], AutoUpgradeMode: autoUpgradeModeMap[h.ID]}
			enrichFromSaaSMeta(&entry)
			result = append(result, entry)
			continue
		}
		if canonicalEqual(h.Owner, username) {
			entry := MyHiveEntry{RegistryEntry: h, Role: "owner", AutoUpgrade: autoUpgradeMap[h.ID], AutoUpgradeMode: autoUpgradeModeMap[h.ID]}
			enrichFromSaaSMeta(&entry)
			result = append(result, entry)
			user.Hives[h.ID] = "owner"
			continue
		}
		if isAdmin {
			entry := MyHiveEntry{RegistryEntry: h, Role: "owner", AutoUpgrade: autoUpgradeMap[h.ID], AutoUpgradeMode: autoUpgradeModeMap[h.ID]}
			enrichFromSaaSMeta(&entry)
			result = append(result, entry)
		}
	}

	seen := make(map[string]bool)
	for _, h := range result {
		seen[h.ID] = true
	}
	for hiveID, role := range user.Hives {
		if seen[hiveID] {
			continue
		}
		if strings.HasPrefix(hiveID, "hosted-") || strings.HasPrefix(hiveID, "saas-") {
			sh := loadSaaSHive(hiveID)
			if sh != nil {
				// Same owner normalization as the registry loop above: the
				// meta record's canonical owner outranks a demoted stored
				// role (#4081).
				if role != "owner" && canonicalEqual(sh.Owner, username) {
					role = "owner"
					user.Hives[hiveID] = "owner"
				}
				entry := MyHiveEntry{
					RegistryEntry: RegistryEntry{
						ID:          sh.ID,
						Name:        sh.Org + "/" + sh.PrimaryRepo,
						Org:         sh.Org,
						Repos:       sh.Repos,
						PrimaryRepo: sh.PrimaryRepo,
						ACMMLevel:   sh.ACMMLevel,
						HiveType:    "hosted",
						Namespace:   hostedNamespaceForHive(sh),
						ClusterID:   clusterIDForSaaSHive(*sh),
						ClusterName: s.clusterNameForID(clusterIDForSaaSHive(*sh)),
					},
					Role: role,
				}
				enrichFromSaaSMeta(&entry)
				result = append(result, entry)
				seen[sh.ID] = true
			}
		}
	}

	for _, sh := range listSaaSHives() {
		if (canonicalEqual(sh.Owner, username) || isAdmin) && !seen[sh.ID] {
			user.Hives[sh.ID] = "owner"
			entry := MyHiveEntry{
				RegistryEntry: RegistryEntry{
					ID:          sh.ID,
					Name:        sh.Org + "/" + sh.PrimaryRepo,
					Org:         sh.Org,
					Repos:       sh.Repos,
					PrimaryRepo: sh.PrimaryRepo,
					ACMMLevel:   sh.ACMMLevel,
					HiveType:    "hosted",
					Namespace:   hostedNamespaceForHive(&sh),
					ClusterID:   clusterIDForSaaSHive(sh),
					ClusterName: s.clusterNameForID(clusterIDForSaaSHive(sh)),
				},
				Role: "owner",
			}
			enrichFromSaaSMeta(&entry)
			result = append(result, entry)
			seen[sh.ID] = true
		}
	}

	if len(user.Hives) > 0 {
		if err := saveSaaSUser(user); err != nil {
			s.logger.Warn("handleMyHives: save failed", "user", username, "error", err)
		}
	}

	// Read the user roster ONCE for the access hover rather than per row —
	// listAllSaaSUsers hits the filesystem for every user record, and My Hives
	// can carry dozens of rows.
	var allSaaSUsers []SaaSUser
	for _, h := range result {
		if h.Role == "owner" || isAdmin {
			allSaaSUsers = listAllSaaSUsers()
			break
		}
	}

	saasCount := 0
	for i, h := range result {
		if strings.HasPrefix(h.ID, "hosted-") || strings.HasPrefix(h.ID, "saas-") {
			saasCount++
		}
		// Backfill the Location-column GitHub host for rows whose spoke is too
		// old to report one. Done here, over the assembled set, so no entry
		// construction site can be missed.
		resolveGitHubHost(&result[i])
		// The leaderboard carries per-USER task counts (who did what on this hive).
		// The admin Users engagement card cross-references it by github_username, so
		// it must reach the admin browser — but it is other people's activity, so a
		// non-admin owner must NOT receive it. Scrub it for everyone but admin.
		// (RegistryEntry.Leaderboard has no omitempty, so it would otherwise ship to
		// every my-hives consumer.)
		if !isAdmin {
			result[i].Leaderboard = nil
		}
		// Who-has-access is shown only to owners (and admin), matching
		// handleAccessList's rule. A read/read-write member is deliberately not
		// told who else shares the hive.
		if h.Role == "owner" || isAdmin {
			// Notes is admin-only CRM text; a non-admin owner gets name+Slack only.
			result[i].Access = accessForHive(h.ID, allSaaSUsers, isAdmin)
			// Recent activity for the status hover, same owner/admin rule as
			// the access list and as handleHiveTimeline itself. s.timeline is
			// nil in tests that construct a bare HubServer, and recent() is a
			// read under the store's own leaf mutex — no s.mu is held here.
			if s.timeline != nil {
				result[i].RecentEvents = s.timeline.recent(h.ID, myHivesRecentEventCount)
			}
		}
		if config.RoleAtLeast(h.Role, config.RoleReadWrite) || isAdmin {
			reqs := loadAccessRequests(h.ID)
			var pending []PendingAccessRequest
			for _, req := range reqs {
				if req.Status == "pending" {
					pending = append(pending, PendingAccessRequest{
						Username:    req.Username,
						RequestedAt: req.RequestedAt,
						Note:        req.Note,
					})
				}
			}
			pending = s.decoratePendingAccessRequests(pending)
			result[i].PendingRequestCount = len(pending)
			result[i].PendingRequests = pending
		}
		result[i].Unassigned = isPlaceholderEntry(result[i])
	}

	// Unassigned placeholder rows: auth-class check failures are the pool's
	// DESIGNED state, not degradation, so neutralise them before anything
	// downstream (drift, the fleet alerts, the browser's row dot and
	// failing-checks pill) reads Health. Runs after enrichment for the same
	// provStatus reason annotateDrift documents below, and before it so the
	// drift health signal and the row agree. See placeholder_health.go.
	sanitizePlaceholderRows(result)

	// Config drift, computed once over the caller's full visible set — the
	// fleet norm is derived from that set, so this must run AFTER every row has
	// been collected and enriched (a row still missing its provStatus would be
	// misread as a claimed hive and flagged for having no App).
	annotateDrift(result, getDisplaySHAs(), time.Now())

	// Dead-link pill, computed once over the full visible set for the same
	// reason as drift: the cluster-outage suppression is a property of the SET
	// (most of a cluster failing is an outage, not N broken hives), so it
	// cannot be decided one row at a time. Derived from the same alert list the
	// panel renders, so pill and panel always agree.
	{
		regs := make([]RegistryEntry, 0, len(result))
		for i := range result {
			regs = append(regs, result[i].RegistryEntry)
		}
		urlAlerts := s.urlUnreachableAlerts(regs, time.Now())
		for i := range result {
			if bad, reason := urlUnreachableFacet(urlAlerts, result[i].ID); bad {
				result[i].URLUnreachable = true
				result[i].URLUnreachableReason = reason
			}
			if private, reason := privateURLFacet(urlAlerts, result[i].ID); private {
				result[i].PrivateURL = true
				result[i].PrivateURLReason = reason
			}
		}
	}

	// Attach the user-journey stage to every row so the table can show who is
	// stalled where. Derived on read; never persisted on the registry entry.
	journeyNow := time.Now()
	for i := range result {
		if count, known := commitsBehindStableV4(result[i].GitHash, s.logger); known {
			result[i].CommitsBehindStableV4 = &count
		}

		st := s.journey.get(result[i].ID)
		status := JourneyStatusFor(&result[i].RegistryEntry, st, journeyNow)
		result[i].Journey = &status
		result[i].AdvisoryIssueActivity = advisoryFreshnessFor(result[i].RegistryEntry, journeyNow)
		result[i].BudgetHealth = budgetHealthFor(result[i].RegistryEntry)
		result[i].GitHubAppHealth = githubAppHealthFor(result[i].RegistryEntry, journeyNow)

		// Advisory-staleness pill, computed on read (same as Journey) so the
		// gating — advisory-mode participation, app-can-write, past-threshold —
		// lives ONLY in Go and the browser just renders the flag.
		if stale, reason := advisoryStaleFromFreshness(result[i].RegistryEntry, result[i].AdvisoryIssueActivity); stale {
			result[i].AdvisoryStale = true
			result[i].AdvisoryStaleReason = reason
		}

		// Auto-upgrade REFUSAL, computed on read for the same reason: the
		// predicate and its staleRemoveAge bound live ONLY in Go, so the fleet
		// badge cannot drift from what triggerAutoUpgrades() will actually do.
		// Gated on AutoUpgrade because the state only means anything for a hive
		// that has asked for auto-upgrades in the first place — a manual hive is
		// not "blocked", it is simply manual.
		if blocked, reason := autoUpgradeBlocked(result[i].AutoUpgrade, result[i].LastHeartbeat, journeyNow); blocked {
			result[i].AutoUpgradeBlocked = true
			result[i].AutoUpgradeBlockedReason = reason
		}

		// Running-but-inactive agents, computed on read for the same reason:
		// the thresholds and the paused/on-demand exclusions live ONLY in Go.
		//
		// The queue gate is the governor's own actionable backlog, which is
		// what makes the idle rule safe on a genuinely quiet hive: with no
		// issues and no PRs waiting, idle agents are CORRECT and nothing is
		// reported. The two unambiguous faults (dead session, login prompt)
		// are independent of it.
		queuedWork := result[i].ActionableIssues + result[i].ActionablePRs
		if rep := evaluateInactiveAgents(result[i].Agents, queuedWork, journeyNow); rep.Count > 0 {
			result[i].InactiveAgents = rep.Count
			result[i].InactiveAgentsReason = rep.Reason
		}

		// "Hive not in use": every reported agent deliberately quiet. Same
		// predicate that suppresses the advisory-stale pill, surfaced as its
		// own state so an entirely-parked hive reads as PARKED, not healthy
		// and not broken.
		result[i].AllAgentsQuiet = allAgentsQuietByDesign(result[i].RegistryEntry)

		// Fleet-divergence view: derive the three-way picture (expected vs
		// actual vs able) and the per-agent verdicts from the same per-agent
		// heartbeat signals plus this hive's blocker fields. Derived on read so
		// the browser never re-runs the state machine (shares classifyInactive‐
		// Agent with the block above, so the two can never disagree).
		//
		// SKIP placeholders/pool hives entirely: an unclaimed placeholder runs
		// its default agents against no real repo, so they legitimately report
		// "expected N · 0 able · N impotent" — a cascade of FALSE alarms that
		// would drown the one signal the view exists for. No verdicts → the
		// frontend has nothing to render for them and they carry no problem
		// count. (isPlaceholderEntry is the same authoritative test computeFleet‐
		// Stats and the alert layer use.)
		if len(result[i].Agents) > 0 && !isPlaceholderEntry(result[i]) {
			if sh := loadSaaSHive(result[i].ID); sh != nil {
				applyAgentRestartResetBaselines(result[i].Agents, sh.AgentRestartResets, journeyNow)
			}
			blockers := hiveBlockers{
				GitHubAppRequired:       result[i].GitHubAppRequired,
				GitHubAppPermIssue:      result[i].GitHubAppPermIssue,
				GitHubAppState:          result[i].GitHubAppState,
				RepoTargetMisconfigured: result[i].RepoTargetMisconfigured,
				RepoTargetIssue:         result[i].RepoTargetIssue,
				InferenceAuthError:      result[i].InferenceAuthError,
				ProviderLimitReason:     result[i].ProviderLimitReason,
				ProviderLimitHiveWide:   result[i].ProviderLimitHiveWide,
				ProviderLimitAgents:     result[i].ProviderLimitAgents,
				GatewayHealth:           result[i].GatewayHealth,
			}
			rollup := rollupAgents(result[i].Agents, blockers, queuedWork, journeyNow)
			result[i].FleetRollup = &rollup
			result[i].AgentVerdicts = buildAgentVerdicts(result[i].Agents, blockers, queuedWork, journeyNow)
			if rollup.RestartStorms > 0 {
				appendDriftSignal(&result[i].Drift, DriftKindAgentRestartStorm, DriftCritical,
					fmt.Sprintf("%d agent(s) restarted at least %d times in the last 24h",
						rollup.RestartStorms, agentRestartProblemThreshold()))
			}
			result[i].AgentRosterMismatch = computeAgentRosterMismatch(result[i].ACMMLevel, result[i].Agents)

			// Hive-health verdict: reuse the rollup + app-health + queue depth we
			// just computed. Only for real (non-placeholder) hives with reported
			// agents — a placeholder has nothing to produce.
			verdict := hiveHealthFor(result[i].RegistryEntry, rollup, result[i].GitHubAppHealth, queuedWork, journeyNow)
			// Digest-lag amber (#5577): the channel/behind-count divergence
			// info lives on MyHiveEntry (TrackedChannel is hub-owned, the
			// behind-count was computed just above), so this row of the
			// signature table is applied here rather than in hiveHealthFor.
			applyChannelLag(&verdict, result[i].TrackedChannel, result[i].CommitsBehindStableV4, result[i].Upgrading)
			// The App-broken hint's install URL is cluster-scoped (a GHE
			// cluster must never be handed a github.com link), so resolve it
			// from this hive's cluster config — the same single URL builder
			// the create-hive modal uses.
			if verdict.cause == causeAppBroken && verdict.Remediation != nil {
				if c, ok := s.clusters[result[i].ClusterID]; ok {
					gh := clusterGitHubConfig(&c)
					verdict.Remediation.Link = gh.AppInstallURL()
				}
			}
			result[i].HealthVerdict = &verdict
		}

		// Sparkline history dominated this payload: at 42 hives the two series
		// were ~755 KB of an 818 KB response (92%), yet they are drawn into a
		// 50 px-wide SVG. Downsample on the WIRE only — the registry keeps the
		// full 7-day series, so nothing is lost server-side and a future
		// full-resolution view can still fetch it per hive.
		result[i].IssueHistory = downsampleSpark(result[i].IssueHistory, sparkWirePoints)
		result[i].PRHistory = downsampleSpark(result[i].PRHistory, sparkWirePoints)
	}

	// Score the quadrant last, once every row is populated: the axes read
	// fields the loop above fills in, and the scores are percentiles against
	// this exact set of rows. Ranking against the whole registry instead would
	// let the header polygon disagree with the rows it summarises.
	fleetQuadrant := attachQuadrants(result, isAdmin, journeyNow)

	// Cosmetic owner labels: resolve opaque OIDC owner identities to their
	// stored display names so "Group by owner" headers and tooltips read like
	// people. Memoized — a fleet shares a handful of owners.
	ownerLabel := s.identityLabeler()
	for i := range result {
		if l := ownerLabel(result[i].Owner); l != result[i].Owner {
			result[i].OwnerName = l
		}
	}

	// Server-side scoping (filter/sort/pagination) happens LAST, after every
	// set-wide computation above (drift norm, alerts, outage suppression,
	// quadrant percentiles) has run over the caller's full visible set — the
	// page is a wire-level view, not a different fleet. No query params →
	// full set, exactly as before.
	hivesView := result
	query := parseMyHivesQuery(r.URL.Query())
	matched := len(result)
	if query.active() {
		hivesView, matched = applyMyHivesQuery(result, query)
	}

	resp := map[string]any{
		"hives": hivesView,
		// The fleet average backs the reference polygon drawn behind every
		// row's kite and the aggregate at the top of the dashboard. It is an
		// aggregate over many hives and identifies none of them, so unlike the
		// per-hive scores it is not gated on the caller's role.
		"fleet_quadrant": fleetQuadrant,
		// Summary counts over the FULL visible set (never the page) so
		// dashboard tiles stay truthful under any filter.
		"hives_summary":            myHivesSummary(result),
		"hives_total":              len(result),
		"hives_matched":            matched,
		"saas_quota":               user.SaaSQuota,
		"saas_used":                saasCount,
		"is_admin":                 isAdmin,
		"latest_sha":               getLatestSHA(),
		"stable_v4_sha":            getLatestSHAForBranch(stableReleaseBranch),
		"latest_shas":              getDisplaySHAs(),
		"latest_sha_messages":      getDisplaySHAMessages(),
		"latest_sha_image_status":  getImageStatuses(),
		"latest_sha_build_started": getImageBuildStartTimes(),
		"latest_sha_build_url":     getImageBuildURLs(),
		"commit_messages":          getCommitMessages(),
		"hub_git_hash":             s.hubGitHash,
		"hub_git_branch":           s.hubGitBranch,
		"tracked_branches":         s.trackedBranchList(),
		// Release channels are moving tags; the dashboard renders them as their
		// own "channel -> image" block above the per-branch rows, and offers
		// them as branch-switch targets. The association is resolved from
		// registry digests (cached), never hardcoded to a branch name.
		"release_channels":  ReleaseChannels(),
		"channel_targets":   getChannelTargets(getDisplaySHAs(), s.logger),
		"hub_auto_upgrade":  isHubAutoUpgrade(),
		"hub_upgrade_state": s.hubUpgradeState(),
		// Kill-switch state rides the top-level payload (NOT the hive-row
		// shape, so no HIVES_CACHE_VERSION bump): the dashboard shows a
		// prominent banner while anything is paused, and admins get toggles.
		"upgrade_pause": s.upgradePauseSnapshot(),
		"show_my_hives": true,
		// Fleet alerts ship WITH the hive list so the "Attention needed" panel
		// renders in the same paint as the rows it summarises — a second
		// round-trip would make the panel pop in after the list and shift it.
		// Scoped to the hives this caller can already see, so it never leaks
		// the existence of a hive they have no access to.
		"alerts": s.fleetAlerts(result),
	}

	myReq := loadProvisionRequest(username)
	if myReq != nil {
		resp["my_provision_request"] = myReq
	}

	if isAdmin {
		// Admin-only: enrichProvisionRequests attaches other users' hive
		// memberships and roles, so it must stay inside this branch. A non-admin
		// caller never receives provision_requests at all.
		resp["provision_requests"] = enrichProvisionRequests(listProvisionRequests())
		// Who is logged into their hive RIGHT NOW, for the green-dashed avatar
		// treatment. Presence data about other users → admin-only, same as the
		// engagement stats it complements.
		resp["live_hive_users"] = s.liveHiveUsernames()
		// The honest subset of live_hive_users: users whose browser reported
		// focused, recent-input presence. live minus engaged = idle open tabs.
		resp["live_engaged_users"] = s.engagedHiveUsernames()
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *HubServer) handleAccessStatus(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	if username == "" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"authenticated": false,
			"show_my_hives": false,
		})
		return
	}

	user := loadSaaSUser(username)
	if user == nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"authenticated": true,
			"show_my_hives": true,
			"hives":         map[string]string{},
		})
		return
	}

	s.mu.Lock()
	offlineEvents := s.markStaleHives()
	allHives := make([]RegistryEntry, len(s.registry.Hives))
	copy(allHives, s.registry.Hives)
	s.mu.Unlock()
	s.flushOfflineEvents(offlineEvents)

	type hiveAccessInfo struct {
		Role   string `json:"role"`
		Status string `json:"status"`
	}
	hiveAccess := make(map[string]hiveAccessInfo)

	isAdmin := isHubAdmin(username)
	for _, h := range allHives {
		if role, ok := user.Hives[h.ID]; ok {
			// Owner normalization mirroring handleMyHives: a stale/demoted
			// stored role must not mask the hive's TRUE owner (#4081).
			if role != "owner" && canonicalEqual(h.Owner, username) {
				role = "owner"
			}
			hiveAccess[h.ID] = hiveAccessInfo{Role: role, Status: "accepted"}
			continue
		}
		if canonicalEqual(h.Owner, username) {
			hiveAccess[h.ID] = hiveAccessInfo{Role: "owner", Status: "accepted"}
			continue
		}
		if isAdmin {
			hiveAccess[h.ID] = hiveAccessInfo{Role: "owner", Status: "accepted"}
			continue
		}
		reqs := loadAccessRequests(h.ID)
		for _, req := range reqs {
			if req.Username == username && req.Status == "pending" {
				hiveAccess[h.ID] = hiveAccessInfo{Status: "pending"}
				break
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"authenticated":            true,
		"show_my_hives":            true,
		"hives":                    hiveAccess,
		"latest_sha":               getLatestSHA(),
		"latest_shas":              getDisplaySHAs(),
		"latest_sha_messages":      getDisplaySHAMessages(),
		"latest_sha_image_status":  getImageStatuses(),
		"latest_sha_build_started": getImageBuildStartTimes(),
		"latest_sha_build_url":     getImageBuildURLs(),
		"commit_messages":          getCommitMessages(),
	})
}

func (s *HubServer) handleCreateHive(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	if username == "" {
		http.Error(w, `{"error":"not authenticated"}`, http.StatusUnauthorized)
		return
	}

	user := loadSaaSUser(username)
	if user == nil || user.Blocked {
		http.Error(w, `{"error":"account blocked or not found"}`, http.StatusForbidden)
		return
	}

	if user.SaaSQuota == 0 {
		http.Error(w, `{"error":"no hosted hive quota — contact the hub admin to request access"}`, http.StatusForbidden)
		return
	}

	const maxCreateHiveBodyBytes = 64 * 1024 // 64 KiB — includes app private key
	r.Body = http.MaxBytesReader(w, r.Body, maxCreateHiveBodyBytes)
	var req CreateHiveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	if host, org, reposFromOrg := normalizeProjectRef(req.Org); org != "" && (host != "" || len(reposFromOrg) > 0) {
		if host != "" {
			req.GitHubBaseURL = "https://" + host
			req.GitHubAPIURL = forgeAPIURLForHost("", host)
		}
		req.Org = org
		if len(reposFromOrg) > 0 {
			prefix := strings.Join(reposFromOrg, "/")
			if strings.TrimSpace(req.Repos) == "" {
				req.Repos = prefix
			}
			if strings.TrimSpace(req.PrimaryRepo) == "" {
				req.PrimaryRepo = prefix
			}
		}
	} else if originalFirstRepo := firstCSV(req.Repos); originalFirstRepo != "" {
		host, org, reposFromShifted := normalizeProjectRef(req.Org + "/" + originalFirstRepo)
		if host != "" && org != "" && strings.Contains(req.Org, ".") {
			req.GitHubBaseURL = "https://" + host
			req.GitHubAPIURL = forgeAPIURLForHost("", host)
			req.Org = org
			repos := replaceFirstCSV(req.Repos, strings.Join(reposFromShifted, "/"))
			req.Repos = repos
			if strings.TrimSpace(req.PrimaryRepo) == "" || strings.TrimSpace(req.PrimaryRepo) == originalFirstRepo {
				req.PrimaryRepo = firstCSV(repos)
			}
		}
	}

	reposForValidation := splitCSV(req.Repos)
	if len(reposForValidation) == 0 {
		reposForValidation = []string{""}
	}
	primaryForValidation := strings.TrimSpace(req.PrimaryRepo)
	if primaryForValidation == "" && len(reposForValidation) > 0 {
		primaryForValidation = reposForValidation[0]
	}
	if issue := config.ValidateProjectRepoTargets(req.Org, reposForValidation, primaryForValidation, repoTargetForgeHost(req.GitHubBaseURL)); issue != nil {
		writeJSONError(w, http.StatusBadRequest, issue.Message)
		return
	}
	if req.Org == "" || req.Repos == "" {
		http.Error(w, `{"error":"org and repos are required"}`, http.StatusBadRequest)
		return
	}
	if !isValidName(req.Org) {
		http.Error(w, `{"error":"invalid org name — alphanumeric, dashes, dots, underscores only"}`, http.StatusBadRequest)
		return
	}
	for _, r := range strings.Split(req.Repos, ",") {
		if !isValidRepoRef(strings.TrimSpace(r)) {
			http.Error(w, `{"error":"invalid repo name"}`, http.StatusBadRequest)
			return
		}
	}
	hasToken := req.GitHubToken != ""
	hasApp := req.AuthMethod == "app" && req.AppID != "" && req.InstallationID != "" && req.AppPrivateKey != ""
	hasAppLater := req.AuthMethod == "app" && req.AppID != "" && req.InstallationID == "" && req.AppPrivateKey == ""
	if !hasToken && !hasApp && !hasAppLater {
		http.Error(w, `{"error":"provide either a GitHub token or GitHub App credentials"}`, http.StatusBadRequest)
		return
	}
	if hasToken && !strings.HasPrefix(req.GitHubToken, "ghp_") && !strings.HasPrefix(req.GitHubToken, "github_pat_") {
		http.Error(w, `{"error":"token must start with ghp_ or github_pat_"}`, http.StatusBadRequest)
		return
	}
	if hasApp && !strings.HasPrefix(strings.TrimSpace(req.AppPrivateKey), "-----BEGIN") {
		http.Error(w, `{"error":"private key must be PEM format"}`, http.StatusBadRequest)
		return
	}

	if user.SaaSQuota > 0 && countUserHives(username) >= user.SaaSQuota {
		http.Error(w, fmt.Sprintf(`{"error":"quota reached — max %d SaaS hives"}`, user.SaaSQuota), http.StatusBadRequest)
		return
	}

	if maxSaaSHivesTotal > 0 && len(listSaaSHives()) >= maxSaaSHivesTotal {
		http.Error(w, `{"error":"hosted capacity reached — try again later"}`, http.StatusServiceUnavailable)
		return
	}

	repos := strings.Split(req.Repos, ",")
	for i := range repos {
		repos[i] = strings.TrimSpace(repos[i])
	}
	primaryRepo := req.PrimaryRepo
	if primaryRepo == "" && len(repos) > 0 {
		primaryRepo = repos[0]
	}
	acmm := req.ACMMLevel
	if acmm < 1 || acmm > 6 {
		acmm = 1
	}

	// Validate and default the target cluster.
	targetCluster := req.ClusterID
	if targetCluster == "" {
		targetCluster = defaultClusterID
	}
	if _, ok := s.clusters[targetCluster]; !ok {
		http.Error(w, `{"error":"unknown cluster_id"}`, http.StatusBadRequest)
		return
	}

	hiveID := generateHiveID(req.Org, primaryRepo)

	// Determine which cluster to provision on. Default to the hub-reachable cluster if unspecified.
	clusterID := req.ClusterID
	if clusterID == "" {
		clusterID = defaultClusterID
	}
	// Look up the cluster to get its domain for the subdomain.
	cluster, clusterFound := s.clusters[clusterID]
	if !clusterFound {
		http.Error(w, `{"error":"unknown cluster_id"}`, http.StatusBadRequest)
		return
	}
	// Per-cluster ceiling — checked after the global cap so the more specific
	// error wins only when the global gate passes.
	if full, n := clusterAtMaxHives(&cluster); full {
		max := effectiveMaxHives(&cluster)
		s.logger.Warn("provision rejected — cluster at max_hives",
			"cluster", cluster.ID, "count", n, "max_hives", max)
		http.Error(w, fmt.Sprintf(`{"error":"cluster %s is at capacity (%d/%d hives) — pick another cluster or raise max_hives"}`,
			cluster.ID, n, max), http.StatusServiceUnavailable)
		return
	}
	subdomain := hiveID + "." + cluster.Domain

	h := &SaaSHive{
		ID:          hiveID,
		Owner:       username,
		ProjectName: req.ProjectName,
		Org:         req.Org,
		Repos:       repos,
		PrimaryRepo: primaryRepo,
		ACMMLevel:   acmm,
		ClusterID:   targetCluster,
		Status:      "provisioning",
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		Subdomain:   subdomain,
		// Default to public when the request omits is_public — matches
		// the pre-#1604 template that hardcoded is_public: true. Owners
		// can toggle visibility later from My Hives.
		IsPublic: req.IsPublic == nil || *req.IsPublic,
		// Per-hive GitHub host override (empty = cluster default). Lets a
		// public-GitHub org provision correctly on a GHE-defaulted cluster.
		GitHubBaseURL: req.GitHubBaseURL,
		GitHubAPIURL:  req.GitHubAPIURL,
	}

	if err := saveSaaSHive(h); err != nil {
		http.Error(w, `{"error":"failed to save hive metadata"}`, http.StatusInternalServerError)
		return
	}

	user.Hives[hiveID] = "owner"
	if err := saveSaaSUser(user); err != nil {
		s.logger.Warn("handleCreateHive: owner grant save failed", "hive_id", hiveID, "user", user.GitHubUsername, "error", err)
	}

	provisionHiveRecord := *h
	provisionHiveRecord.Repos = append([]string(nil), h.Repos...)
	provisionReq := req
	// Queued, not spawned: execution is bounded hub-wide and per cluster so a
	// provisioning burst cannot stampede kubectl/OCI (see provision_queue.go).
	enqueueProvision(targetCluster, func() {
		h := &provisionHiveRecord
		cluster := s.clusterForHive(h)
		if cluster == nil {
			h.Status = "error"
			h.Error = "no cluster config available"
			if saveErr := saveSaaSHive(h); saveErr != nil {
				s.logger.Warn("failed to persist hive error status", "hive_id", hiveID, "error", saveErr)
			}
			s.logger.Error("no cluster config for provisioning", "hive_id", hiveID, "cluster_id", h.ClusterID)
			return
		}
		if err := provisionHive(h, &provisionReq, cluster, s.appKeysByAppID(), s.logger); err != nil {
			h.Status = "error"
			h.Error = err.Error()
			if saveErr := saveSaaSHive(h); saveErr != nil {
				s.logger.Warn("failed to persist hive error status", "hive_id", hiveID, "error", saveErr)
			}
			s.logger.Warn("hosted hive provision failed", "hive_id", hiveID, "error", err)
			return
		}
		h.Status = "provisioning"
		if saveErr := saveSaaSHive(h); saveErr != nil {
			s.logger.Warn("failed to persist hive provisioning status", "hive_id", hiveID, "error", saveErr)
		}
	})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":        hiveID,
		"status":    "provisioning",
		"subdomain": h.Subdomain,
	})
}

func (s *HubServer) handleHiveStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	username := s.getAuthUser(r)
	h := loadSaaSHive(id)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	user := loadSaaSUser(username)
	if user == nil {
		http.Error(w, `{"error":"access denied"}`, http.StatusForbidden)
		return
	}
	// Owner-only, matching v2's tightening (operator decision 2026-08-13):
	// effective owners (creator, granted owner, hub admin — userIsHiveOwner)
	// read status; granted read/read-write roles do NOT. The full SaaSHive
	// record includes operational metadata beyond what a read grant implies.
	if !userIsHiveOwner(username, h) {
		http.Error(w, `{"error":"access denied"}`, http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h)
}

// handleOpenHive is the SSO handoff entry point: a hub-authenticated user hits
// this to open a spoke dashboard without a second GitHub login. It confirms the
// user may access the hive, mints a short-lived HMAC token bound to {user, role,
// hiveID} with the shared hub secret, and 302-redirects to the spoke's
// <dashboardURL>/sso?token=… . The spoke verifies the token and its own
// authorized_users allowlist before minting a session (see dashboard.handleSSO).
//
// If SSO can't be used (no hub secret, or the spoke reported no dashboard URL),
// it falls back to redirecting straight to the dashboard URL (or the hub-reachable-cluster
// host), preserving today's behavior — the spoke will then prompt for login.
func (s *HubServer) handleOpenHive(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if strings.Contains(id, "..") || strings.Contains(id, "/") {
		http.Error(w, `{"error":"invalid hive id"}`, http.StatusBadRequest)
		return
	}
	// /open is the link people actually paste into Slack, so serve the Hive
	// preview card to crawlers here rather than letting them follow the 303 to
	// /login. Short-circuiting is the robust option: an unfurler that caps or
	// skips redirects would otherwise never reach the card, and one that follows
	// the chain unauthenticated lands on GitHub's OAuth page and scrapes GitHub's
	// Open Graph tags — which is exactly the bug.
	if isLinkPreviewCrawler(r) {
		writeLinkPreview(w)
		return
	}
	username := s.getAuthUser(r)
	if username == "" {
		// Not logged in — this is a browser navigation, so send the user through
		// the hub login and return them to THIS /open URL afterward, so the SSO
		// handoff completes and they land logged-in on the spoke (instead of the
		// raw {"error":"not authenticated"} JSON dead end).
		self := "/api/saas/hives/" + url.PathEscape(id) + "/open"
		http.Redirect(w, r, "/login?redirect="+url.QueryEscape(self), http.StatusSeeOther)
		return
	}

	// Resolve the spoke's base URL: prefer the heartbeat-reported dashboard URL
	// (correct for firewalled spokes), fall back to the hub-reachable-cluster host pattern.
	base := ""
	s.mu.RLock()
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == id {
			base = s.registry.Hives[i].DashboardURL
			break
		}
	}
	s.mu.RUnlock()
	// For a claimed hive, hand the SSO handoff off to its vanity host rather than
	// the raw placeholder host the spoke may still be reporting — the vanity URL
	// is the validated, user-facing host (and the one the spoke will settle on).
	// Only a validated meta vanity_url is used; unclaimed placeholders are left on
	// their working placeholder host.
	if v := claimedVanityURL(loadSaaSHive(id)); v != "" {
		base = v
	}
	if base == "" && (strings.HasPrefix(id, "hosted-") || strings.HasPrefix(id, "saas-")) {
		base = s.placeholderHostURL(id)
	}
	if base == "" {
		http.Error(w, `{"error":"hive has no reachable dashboard URL yet"}`, http.StatusConflict)
		return
	}
	base = strings.TrimRight(base, "/")

	// Access gate: only the owner, an authorized user, or the hub admin may open
	// the spoke. The role we pass is advisory — the spoke re-checks its own
	// allowlist and uses that role authoritatively.
	// Every branch below either assigns role or rejects the request, so no
	// initializer is needed (and ineffassign flags one as dead).
	var role string
	if isHubAdmin(username) {
		role = saasRoleOwner
	} else {
		user := loadSaaSUser(username)
		if user == nil {
			http.Error(w, `{"error":"access denied"}`, http.StatusForbidden)
			return
		}
		if h := loadSaaSHive(id); h != nil && canonicalEqual(h.Owner, username) {
			role = saasRoleOwner
		} else if r, ok := user.Hives[id]; ok {
			role = r
		} else {
			http.Error(w, `{"error":"access denied"}`, http.StatusForbidden)
			return
		}
	}

	// Mint the handoff token. Without a hub secret we can't sign one, so fall
	// back to a plain dashboard redirect (spoke will prompt for login).
	//
	// Ed25519-only: every v4 spoke verifies SSO handoff tokens with an Ed25519
	// PUBLIC key (see MintSSOToken/VerifySSOToken in sso.go). There is no
	// per-hive branching or legacy symmetric fallback here — the hub always
	// mints with its Ed25519 signing seed, and a spoke that cannot verify
	// Ed25519 tokens simply falls through to the plain dashboard redirect below.
	if s.hubSecret != "" {
		if tok := MintSSOToken(s.ssoSigningSeed(), username, role, id, time.Now()); tok != "" {
			http.Redirect(w, r, base+"/sso?token="+url.QueryEscape(tok), http.StatusSeeOther)
			return
		}
	}
	http.Redirect(w, r, base+"/", http.StatusSeeOther)
}

func (s *HubServer) handleDeleteHive(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	id := r.PathValue("id")
	if strings.Contains(id, "..") || strings.Contains(id, "/") {
		http.Error(w, `{"error":"invalid hive id"}`, http.StatusBadRequest)
		return
	}

	h := loadSaaSHive(id)
	if h == nil {
		// SaaS meta.json already gone — still clean up the in-memory registry so
		// the hive disappears from the listing immediately, and best-effort purge
		// any leftover record dir / timeline file so a partial prior delete cannot
		// leave a status:"available" husk that resurrects in the listing.
		s.removeRegistryEntry(id, username)
		removeHiveRecord(id, s.logger)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": deleteStatusDeleted})
		return
	}
	if !userIsHiveOwner(username, h) {
		http.Error(w, `{"error":"only the owner can delete this hive"}`, http.StatusForbidden)
		return
	}

	// Full de-provisioning: namespace, PV, OCI export, OCI file system, disk record, user cleanup.
	//
	// The registry entry is removed unconditionally, even when the cluster
	// teardown cannot run or only partially succeeds. A hive whose namespace is
	// already gone (or whose cluster config has been removed) would otherwise be
	// permanently un-deletable: the handler used to return 500 before reaching
	// removeRegistryEntry, stranding a ghost row in "My Hives" that no amount of
	// re-clicking Delete could clear. Leaving a stranded row is strictly worse
	// than leaving cloud resources behind, because the row is what the user sees
	// and it is the only thing they can act on. Partial failures are reported in
	// the response so the user knows manual cleanup may still be needed.
	cluster := s.clusterForHive(h)
	if cluster != nil {
		deprovisionHive(h, cluster, s.logger)
	} else {
		s.logger.Error("no cluster config for deprovision; removing registry entry anyway",
			"hive_id", id, "cluster_id", h.ClusterID)
	}
	// Durably purge the hub-side record. This is what stops the "resurrection":
	// deprovisionHive removes the hive directory only when a cluster config is
	// available (the else branch above skips it), and NEITHER path removes the
	// timeline file. Without this, a delete with no cluster config left the
	// hives/<id>/ dir — status:"available" — behind, so the hive reappeared in
	// the unassigned/available list forever. removeHiveRecord is idempotent, so
	// calling it after a successful deprovision (which already removed the dir)
	// is harmless and still cleans up the timeline file deprovision never touched.
	//
	// NOTE: this is the genuine, user-initiated delete path. It is deliberately
	// NOT the placeholder-recycle path — resetting a slot to status:"available"
	// (handleResetAssignment / sweepStuckAssignments in saas_reset_assignment.go)
	// rewrites meta.json in place and KEEPS the record, and never reaches this
	// handler. So purging the record here cannot break placeholder recycling.
	removeHiveRecord(id, s.logger)
	s.removeRegistryEntry(id, username)

	s.logger.Info("audit: hosted hive deleted", "hive_id", id, "by", username,
		"deprovisioned", cluster != nil)
	w.Header().Set("Content-Type", "application/json")
	if cluster == nil {
		// deleteStatusPartial tells the UI the registry row is gone but cloud
		// resources may survive and need manual cleanup.
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":  deleteStatusPartial,
			"warning": "removed from the hub registry, but no cluster config was available to delete the namespace, PV, or OCI storage — these may need manual cleanup",
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"status": deleteStatusDeleted})
}

// Delete outcome statuses returned by handleDeleteHive.
const (
	// deleteStatusDeleted means the registry entry was removed and the cluster
	// teardown ran.
	deleteStatusDeleted = "deleted"
	// deleteStatusPartial means the registry entry was removed but the cluster
	// teardown could not run; cloud resources may need manual cleanup.
	deleteStatusPartial = "partially_deleted"
)

var hubAutoUpgradePath = "/data/saas/hub-auto-upgrade"

// The hub's own auto-upgrade is deliberately NOT given the daily schedule that
// per-hive auto-upgrade has. The two look symmetric but carry opposite risk:
//
//   - A spoke hive is a workload. Restarting it interrupts running agents, so
//     deferring to an after-hours window is a clear win — that is exactly the
//     "don't disturb a working hive" motivation for the daily mode.
//   - The hub is the control plane. It is what DELIVERS every spoke upgrade,
//     serves the dashboard, and receives every heartbeat. Holding a hub fix for
//     up to 24 hours means holding back fixes to the upgrade machinery itself,
//     including any fix to this scheduler. A hub restart is also cheap: it is a
//     single stateless pod whose state lives on the PVC, and spokes tolerate a
//     missed heartbeat cycle by design.
//
// Deferring hub upgrades would therefore add real risk (a known-bad hub stays
// up all day) to avoid a disruption the hub does not really suffer. It stays a
// plain on/off toggle. Revisit only if hub restarts are ever shown to disrupt
// in-flight spoke work.
func isHubAutoUpgrade() bool {
	data, err := os.ReadFile(hubAutoUpgradePath)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == "true"
}

func (s *HubServer) handleHubAutoUpgrade(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AutoUpgrade bool `json:"auto_upgrade"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
		return
	}
	val := "false"
	if body.AutoUpgrade {
		val = "true"
	}
	if err := os.WriteFile(hubAutoUpgradePath, []byte(val), 0644); err != nil {
		s.logger.Error("hub auto-upgrade toggle save failed", "enabled", body.AutoUpgrade, "error", err)
		http.Error(w, `{"error":"failed to save preference"}`, http.StatusInternalServerError)
		return
	}
	s.logger.Info("audit: hub auto-upgrade toggled", "enabled", body.AutoUpgrade, "by", s.getAuthUser(r))

	// If enabling and hub is behind, trigger immediately. The kill switch does
	// NOT block saving the preference — only the immediate rollout: with hub
	// upgrades paused the poller stays suppressed too, and the preference takes
	// effect when an admin resumes.
	if body.AutoUpgrade {
		if sw, paused := s.hubUpgradesPaused(); paused {
			s.logger.Info("hub auto-upgrade initial trigger suppressed — hub upgrades are paused",
				"paused_by", sw.By, "paused_at", sw.At)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"ok":true,"auto_upgrade":%t}`, body.AutoUpgrade)
			return
		}
		latestSHA := getLatestHubSHAForBranch(s.hubGitBranch)
		if latestSHA != "" && !sameCommit(latestSHA, s.hubGitHash) {
			s.logger.Info("audit: hub auto-upgrade initial trigger", "from", s.hubGitHash, "to", latestSHA)
			// Route through rolloutHubToSHA so this shares the hub-image gate and
			// the SHA-pin (avoids a stale cached v2-latest) with the poller path.
			if err := s.rolloutHubToSHA(latestSHA); err != nil {
				s.logger.Warn("hub auto-upgrade skipped", "to", latestSHA, "reason", err)
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"ok":true,"auto_upgrade":%t}`, body.AutoUpgrade)
}

// rolloutHubToSHA upgrades the hub deployment to a specific v2 SHA. It first
// verifies the hub's OWN image (ghcrRepoHub) exists for that SHA on GHCR — a
// separate build job from the spoke image, so a failed hive-hub build must not
// trigger a doomed rollout — then pins the deployment to the immutable
// hive-hub:<sha> tag via `kubectl set image`. Pinning (rather than
// `rollout restart` of the mutable v2-latest tag) forces the node to pull that
// exact image and stops a stale cached v2-latest digest from coming back up
// (the "rolled but still on the old hash" failure). On success it records the
// in-flight state so the dashboard shows "Upgrading". Returns an error the
// caller surfaces; safe to call from both the poller and the admin handler.
func (s *HubServer) rolloutHubToSHA(sha string) error {
	if sha == "" {
		return fmt.Errorf("empty target SHA")
	}
	// Shape-check the tag BEFORE it can reach a live Deployment. Writing an
	// unresolvable tag (the `target1` incident — a test fixture SHA that escaped
	// to the real cluster) leaves the new ReplicaSet in ImagePullBackOff while
	// the old one keeps serving: the hub stays "up" but silently runs stale
	// code. Refusing here leaves the last good image running and makes the
	// failure loud instead of invisible.
	if err := validateImageTag(sha); err != nil {
		s.logger.Error("hub self-upgrade REFUSED: invalid image tag",
			"tag", sha,
			"deployment", hubDeploymentName,
			"namespace", hubNamespace,
			"error", err)
		s.setHubUpgradeFault(fmt.Sprintf("refused invalid image tag %q: %v", sha, err))
		return err
	}
	if !hubImageExists(sha, s.logger) {
		return fmt.Errorf("hub image %s:%s not published yet", ghcrRepoHub, sha)
	}
	image := fmt.Sprintf("ghcr.io/%s:%s", ghcrRepoHub, sha)
	cmd := kubectlForCluster(s.hubCluster(), "set", "image",
		"deployment/"+hubDeploymentName, hubContainerName+"="+image, "-n", hubNamespace)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("kubectl set image failed: %s", strings.TrimSpace(string(out)))
	}
	s.hubUpgradeMu.Lock()
	s.lastHubUpgradeTrigger = time.Now()
	s.hubUpgradeTarget = sha
	s.hubUpgradeFault = "" // a fresh, validated rollout clears any prior refusal
	s.hubUpgradeMu.Unlock()
	// Watch the rollout land. Detect-and-report only: an auto-rollback would
	// race the upgrade poller (which re-triggers the same target every cycle),
	// so a rollback could fight the upgrade loop and flap the deployment. See
	// watchHubRollout.
	go s.watchHubRollout(sha, image)
	return nil
}

// hubRolloutWatchTimeout bounds how long we wait for a self-upgrade we just
// triggered to become Ready before flagging it as stuck. The hub's own image is
// a few hundred MB and a cold node has to pull it, so this is generous enough
// to avoid false alarms while still catching an ImagePullBackOff — which
// back-off-retries indefinitely and would otherwise never surface.
const hubRolloutWatchTimeout = 5 * time.Minute

// hubRolloutPollInterval is how often watchHubRollout re-checks rollout status.
const hubRolloutPollInterval = 15 * time.Second

// watchHubRollout confirms a self-upgrade actually became Ready, and records a
// loud, user-visible fault if it did not.
//
// It deliberately does NOT roll back. The auto-upgrade poller re-evaluates the
// same target every cycle, so an automatic rollback would immediately be undone
// and re-applied, flapping the deployment during an already-degraded window.
// Detect and report is the safe half of the loop: the operator sees the stuck
// upgrade in the UI and in the logs, and decides.
func (s *HubServer) watchHubRollout(sha, image string) {
	deadline := time.Now().Add(hubRolloutWatchTimeout)
	for time.Now().Before(deadline) {
		time.Sleep(hubRolloutPollInterval)
		// `rollout status --timeout=0s` returns non-zero while a rollout is
		// still progressing and zero once it has fully succeeded.
		cmd := kubectlForCluster(s.hubCluster(), "rollout", "status",
			"deployment/"+hubDeploymentName, "-n", hubNamespace, "--timeout=0s")
		if err := cmd.Run(); err == nil {
			s.hubUpgradeMu.Lock()
			s.hubUpgradeFault = ""
			s.hubUpgradeMu.Unlock()
			s.logger.Info("hub self-upgrade rollout completed", "sha", sha, "image", image)
			return
		}
	}
	// Still not Ready. Pull the pod-level reason so the log names the actual
	// cause (ImagePullBackOff / ErrImagePull / CrashLoopBackOff) rather than
	// just "timed out".
	reason := s.hubRolloutFailureReason()
	s.logger.Error("hub self-upgrade STUCK: new ReplicaSet not Ready — hub is still serving the OLD image",
		"sha", sha,
		"image", image,
		"deployment", hubDeploymentName,
		"namespace", hubNamespace,
		"waited", hubRolloutWatchTimeout.String(),
		"reason", reason)
	s.setHubUpgradeFault(fmt.Sprintf("upgrade to %s stuck after %s: %s (still serving the previous image)",
		image, hubRolloutWatchTimeout, reason))
}

// hubRolloutFailureReason best-effort extracts why the hub's pods are not
// Ready, so the stuck-rollout log/status names the real cause.
func (s *HubServer) hubRolloutFailureReason() string {
	const reasonJSONPath = `{range .items[*].status.containerStatuses[*]}{.state.waiting.reason}{" "}{.state.waiting.message}{"\n"}{end}`
	cmd := kubectlForCluster(s.hubCluster(), "get", "pods",
		"-n", hubNamespace, "-l", "app="+hubDeploymentName,
		"-o", "jsonpath="+reasonJSONPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "unknown (could not read pod status)"
	}
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return "unknown (no waiting container state reported)"
}

// setHubUpgradeFault records a self-upgrade failure for the dashboard so a
// stuck or refused upgrade is visible in the UI, not only in `kubectl describe`.
func (s *HubServer) setHubUpgradeFault(msg string) {
	s.hubUpgradeMu.Lock()
	s.hubUpgradeFault = msg
	s.hubUpgradeMu.Unlock()
}

// HubUpgradeFault returns the current self-upgrade fault message, or "" when
// the last upgrade attempt was healthy.
func (s *HubServer) HubUpgradeFault() string {
	s.hubUpgradeMu.Lock()
	defer s.hubUpgradeMu.Unlock()
	return s.hubUpgradeFault
}

// hubUpgradeState reports the hub's own upgrade status for the dashboard badge:
//   - "current"   — running the latest v2 SHA
//   - "upgrading" — a rollout we triggered is in flight (within the debounce
//     window after the trigger, before the new pod reports the new hash)
//   - "queued"    — behind latest, auto-upgrade ON, no rollout in flight yet
//     (the poller will trigger one shortly)
//   - "behind"    — behind latest, auto-upgrade OFF (admin must click Upgrade)
//   - "failed"    — an upgrade was REFUSED (malformed image tag) or its rollout
//     never became Ready. The hub is behind and cannot self-heal;
//     an operator must look. HubUpgradeFault() carries the reason.
//   - "unknown"   — latest SHA not resolved yet
//
// This is the field the badge needs: previously the frontend could only tell an
// admin-clicked rollout ("upgrading") from everything else ("queued"), so an
// AUTO rollout in progress showed a misleading "queued".
func (s *HubServer) hubUpgradeState() string {
	// Gate the badge on the HUB image, matching what rolloutHubToSHA can
	// actually roll to — otherwise the UI shows "behind"/"queued" against a
	// target the hub is incapable of reaching.
	latest := getLatestHubSHAForBranch(s.hubGitBranch)
	if latest == "" {
		return "unknown"
	}
	if sameCommit(latest, s.hubGitHash) {
		return "current"
	}
	s.hubUpgradeMu.Lock()
	inFlight := s.hubUpgradeTarget != "" &&
		time.Since(s.lastHubUpgradeTrigger) < hubUpgradeDebounce
	fault := s.hubUpgradeFault
	s.hubUpgradeMu.Unlock()
	// A refused or stuck upgrade outranks "upgrading"/"queued": the hub is
	// behind AND cannot get there on its own, which needs an operator. Without
	// this the badge showed a reassuring "queued" while the rollout was wedged.
	if fault != "" {
		return hubUpgradeStateFailed
	}
	if inFlight {
		return "upgrading"
	}
	if isHubAutoUpgrade() {
		return "queued"
	}
	return "behind"
}

func (s *HubServer) handleHubSelfUpgrade(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	// Admin kill switch: while hub upgrades are paused, a manual trigger is
	// refused loudly — never queued — so the operator learns the state and who
	// set it instead of waiting on a rollout that will not come.
	if sw, paused := s.hubUpgradesPaused(); paused {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": upgradePauseRefusal("hub", sw)})
		return
	}
	target := getLatestHubSHAForBranch(s.hubGitBranch)
	s.logger.Info("audit: hub self-upgrade triggered", "by", username, "to", target)
	if err := s.rolloutHubToSHA(target); err != nil {
		s.logger.Warn("hub self-upgrade failed", "error", err)
		http.Error(w, `{"error":"hub upgrade failed — check logs"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"upgrading"}`))
}

func (s *HubServer) handleUpgradeHive(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if isSameOriginAsHub(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	}
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	id := r.PathValue("id")
	username := s.getAuthUser(r)
	if username == "" {
		username, _ = s.trustedSpokeUpgradeUser(r, id)
	}
	h := loadSaaSHive(id)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	if !userIsHiveOwner(username, h) {
		http.Error(w, `{"error":"only the owner can upgrade"}`, http.StatusForbidden)
		return
	}
	// Admin kill switch: refuse loudly rather than arm anything — a request
	// accepted here would either restart the pod now or sit silently in
	// heartbeatUpgrade, both of which the pause exists to prevent.
	if sw, paused := s.spokeUpgradesPaused(); paused {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": upgradePauseRefusal("spoke", sw)})
		return
	}
	cluster := s.clusterForHive(h)
	if cluster == nil {
		http.Error(w, `{"error":"no cluster config for this hive"}`, http.StatusInternalServerError)
		return
	}

	// PULL ONLY — no kubectl push. The manual Upgrade button now does exactly
	// what auto-upgrade does: record the target and arm the heartbeat. The
	// spoke collects it on its next beat and patches its own Deployment.
	//
	// UX CONSEQUENCE, DELIBERATE: the click no longer produces an immediate
	// pod roll. Delivery is bounded by the heartbeat interval, so "clicked
	// Upgrade, nothing visible yet" is now normal and must not be mistaken for
	// the wedge this PR fixes. The hive is latched Upgrading with a target the
	// moment the request returns, which is what the dashboard renders.

	// Do not ARM an upgrade this hive cannot COLLECT — the same predicate
	// triggerAutoUpgrades() applies, for the same reason. Delivery is PULL on
	// BOTH paths: the hub only records a target and arms the heartbeat, and the
	// spoke patches its own Deployment when it next beats. A hive that never
	// heartbeats (or is silent past staleRemoveAge) therefore never collects
	// the instruction, while Upgrading=true latches on the hub and the
	// stale-upgrade sweep re-arms it every staleUpgradeTimeout — an unbounded
	// loop the orphan sweep's retry budget cannot break, because such a hive
	// fails evaluateOrphanedUpgrade()'s liveness test. See pullonly_upgrade.go.
	//
	// Without this, the manual button was strictly WORSE than the auto path it
	// diverged from: auto-upgrade refuses and records the refusal on the
	// timeline, whereas the click reported {"status":"upgrading"} and a success
	// toast for an upgrade that could never land. Worse, the asymmetry read as
	// a workaround — the same spoke auto-upgrade had declined would accept a
	// manual click, appearing to fix the problem while only hiding it.
	//
	// lastHeartbeat comes from the REGISTRY entry, which is the only record
	// that carries it; SaaSHive (the loadSaaSHive record `h` above) has no such
	// field. This is the identical source triggerAutoUpgrades() reads.
	//
	// Refused with 409, matching the pause-switch refusal above. The reason is
	// operator-facing by construction and documented to carry no kubeconfig
	// paths or credentials, so it is safe in the body.
	s.mu.Lock()
	var latestSHA, lastHeartbeat string
	var found bool
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == id {
			found = true
			lastHeartbeat = s.registry.Hives[i].LastHeartbeat
			branch := s.upgradeBranchOrDefault(s.registry.Hives[i].GitBranch)
			latestSHA = getLatestSHAForBranch(branch)
			break
		}
	}
	// A hive with no registry entry has never checked in at all, so it is
	// uncollectible for exactly the reason the empty-heartbeat case is.
	if !found || !upgradeCollectible(lastHeartbeat, time.Now()) {
		s.mu.Unlock()
		reason := uncollectibleUpgradeReason(lastHeartbeat)
		s.logger.Warn("manual upgrade not armed — hive cannot collect the instruction",
			"hive_id", id, "by", username, "cluster", cluster.ID,
			"would_have_targeted", latestSHA, "last_heartbeat", orDash(lastHeartbeat),
			"reason", reason)
		s.noteUncollectibleUpgrade(id, latestSHA, reason)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": reason})
		return
	}
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == id {
			s.beginUpgrade(i, latestSHA)
			break
		}
	}
	if latestSHA != "" {
		// Arm delivery: the spoke self-restarts onto the target when its next
		// heartbeat carries UpgradeTo. The stale-upgrade sweep re-arms this if
		// the spoke misses it, so the request cannot be lost.
		if s.heartbeatUpgrade == nil {
			s.heartbeatUpgrade = make(map[string]string)
		}
		s.heartbeatUpgrade[id] = latestSHA
	}
	s.mu.Unlock()

	if latestSHA == "" {
		// No build target is known for this hive's branch, so there is nothing
		// the heartbeat could carry. This is the only hard-failure case left.
		s.logger.Warn("upgrade failed: no build target known for this hive's branch",
			"hive", id, "cluster", cluster.ID)
		http.Error(w, `{"error":"upgrade failed — no build target known for this hive's branch"}`, http.StatusBadGateway)
		return
	}

	// Always "heartbeat" now: the push path is retired, so every upgrade is
	// collected by the spoke on its next beat. The UI uses this to say "queued"
	// rather than implying an immediate roll.
	const mode = "heartbeat"
	// Armed successfully, so the uncollectible condition has genuinely cleared:
	// drop the de-duplication memory (as the auto path does on its own successful
	// arm) so a LATER refusal for this same target is reported afresh rather than
	// suppressed by a stale entry.
	s.forgetUncollectibleUpgrade(id)
	s.logger.Info("audit: hosted hive upgrade requested",
		"hive_id", id, "by", username, "cluster", cluster.ID, "mode", mode)
	s.recordTimeline(id, TimelineUpgradeStarted, "upgrade requested from the hub dashboard ("+mode+")", username)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"upgrading","mode":"` + mode + `"}`))
}

// branchToTag converts a git branch name into a valid Docker image tag.
// Branch names may contain '/' (e.g. feat/x) which is illegal in a tag; the
// docker.yml build sanitizes the same way (feat/x -> feat-x-latest), so the
// hub must match to find the image.
func branchToTag(branch string) string {
	return strings.ReplaceAll(branch, "/", "-")
}

func (s *HubServer) handleSwitchBranch(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if isSameOriginAsHub(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	}
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	id := r.PathValue("id")
	username := s.getAuthUser(r)
	h := loadSaaSHive(id)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	if !userIsHiveOwner(username, h) {
		http.Error(w, `{"error":"only the owner can switch branches"}`, http.StatusForbidden)
		return
	}
	// Admin kill switch: a branch/channel switch while spoke upgrades are
	// paused gets an explicit 409 naming who paused and when — never a silent
	// queue into heartbeatSwitchTag.
	if sw, paused := s.spokeUpgradesPaused(); paused {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": upgradePauseRefusal("spoke", sw)})
		return
	}
	var body struct {
		Branch string `json:"branch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Branch == "" {
		http.Error(w, `{"error":"branch is required"}`, http.StatusBadRequest)
		return
	}
	// A release channel is a moving TAG, not a git ref, so the branch-existence
	// checks below would reject it ("stable" is not a branch on the hive repo).
	// Accept it here and let the shared publish check further down be the real
	// gate — the tag still has to exist on GHCR before we point a hive at it.
	isChannel := isReleaseChannel(body.Branch)
	validBranch := isChannel
	for _, b := range s.trackedBranchList() {
		if b == body.Branch {
			validBranch = true
			break
		}
	}
	// Bootstrap case: trackedBranchList only includes branches already
	// assigned to some hive, so the FIRST hive moved to a new dev branch
	// would never validate. Accept any branch that actually exists on the
	// hive repo — a live one-shot check (fetchBranchSHA populates the SHA
	// cache as a side effect, so the branch is tracked from here on).
	if !validBranch {
		fetchBranchSHA(s.logger, body.Branch)
		if getLatestSHAForBranch(body.Branch) != "" {
			validBranch = true
		}
	}
	if !validBranch {
		http.Error(w, `{"error":"unknown branch (does not exist on the hive repo)"}`, http.StatusBadRequest)
		return
	}
	cluster := s.clusterForHive(h)
	if cluster == nil {
		http.Error(w, `{"error":"no cluster config for this hive"}`, http.StatusInternalServerError)
		return
	}
	ns := "hive-hosted-" + id
	// A channel IS the tag ("stable"); a branch's moving tag is "<branch>-latest".
	imageTag := upgradeTargetTag(body.Branch)
	image := "ghcr.io/hivecommons/hive:" + imageTag
	// Refuse a branch name that sanitizes into something that is not a valid
	// channel tag, rather than stranding the spoke on ImagePullBackOff behind a
	// still-serving old ReplicaSet.
	if err := validateImageTag(imageTag); err != nil {
		s.logger.Error("branch switch REFUSED: invalid image tag",
			"hive", id, "branch", body.Branch, "tag", imageTag, "error", err)
		http.Error(w, `{"error":"branch does not map to a valid image tag"}`, http.StatusBadRequest)
		return
	}
	// Shape is not existence. A DEPRECATED branch (v3 was retired while hives
	// were still pointed at it) keeps a perfectly well-formed "<branch>-latest"
	// tag that CI no longer publishes, so validateImageTag passes and the pull
	// then fails with an opaque "manifest unknown". Kubernetes keeps the old
	// ReplicaSet serving, so the hive looks alive while silently running stale
	// code. Verify the tag is actually pullable before writing it.
	if !spokeImageExists(imageTag, s.logger) {
		s.logger.Error("branch switch REFUSED: image tag not published on GHCR",
			"hive", id, "branch", body.Branch, "tag", imageTag,
			"hint", "branch may be deprecated or its CI image build never completed")
		http.Error(w, `{"error":"no published image for that branch (deprecated branch, or its image build has not completed)"}`, http.StatusBadRequest)
		return
	}
	// Persist WHAT the operator selected before delivering it, on the hub-owned
	// hive record. The registry cannot remember a channel selection: the spoke
	// heartbeats the image's baked-in branch (a "stable" retag of a v4 build
	// reports git_branch="v4") and overwrites GitBranch every beat, so within
	// one beat of the switch the dashboard's pill fell back to the branch. Set
	// on a channel switch, cleared on a plain-branch switch, written by no
	// other path — heartbeats never touch it. Done after every validation gate
	// above (so a refused switch records nothing) and before the two delivery
	// paths below (so kubectl-vs-heartbeat delivery cannot diverge on it). A
	// save failure only downgrades the pill to the reported branch; it must
	// not block the switch itself.
	if isChannel {
		h.TrackedChannel = body.Branch
	} else {
		h.TrackedChannel = ""
	}
	if err := saveSaaSHive(h); err != nil {
		s.logger.Warn("branch switch: failed to persist tracked channel — the version pill will fall back to the spoke-reported branch",
			"hive", id, "target", body.Branch, "error", err)
	}
	// "*=" updates every container including init containers (copy-config,
	// init-permissions) — pinning only "hive" left inits on the old branch tag.
	cmd := kubectlForCluster(cluster, "set", "image", "deployment/hive", "*="+image, "-n", ns)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// The hub can't reach this hive's cluster over kubectl (e.g. the heartbeat-only cluster
		// from the hub-reachable-cluster hub). Fall back to the heartbeat path: record the
		// target tag; the spoke — which has in-cluster RBAC (hive-self-upgrade
		// role) to patch its own deployment — applies it on its next
		// heartbeat. This is the ONLY path that works for unreachable
		// clusters, so it's not an error.
		s.logger.Warn("branch switch kubectl failed, using heartbeat fallback",
			"hive", id, "branch", body.Branch, "output", string(out))
		s.mu.Lock()
		s.heartbeatSwitchTag[id] = imageTag
		for i := range s.registry.Hives {
			if s.registry.Hives[i].ID == id {
				s.beginUpgrade(i, imageTag)
				break
			}
		}
		s.mu.Unlock()
		s.logger.Info("audit: hive branch switch queued via heartbeat", "hive_id", id, "branch", body.Branch, "image", image, "by", username)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "switching", "branch": body.Branch, "image": image, "via": "heartbeat"})
		return
	}
	// Restart the deployment to pull the new image
	restartCmd := kubectlForCluster(cluster, "rollout", "restart", "deployment/hive", "-n", ns)
	restartOut, restartErr := restartCmd.CombinedOutput()
	if restartErr != nil {
		s.logger.Warn("rollout restart after branch switch failed", "hive", id, "output", string(restartOut))
	}
	s.logger.Info("audit: hive branch switched", "hive_id", id, "branch", body.Branch, "image", image, "by", username)
	s.recordTimeline(id, TimelineBranchChanged,
		fmt.Sprintf("branch switch to %s requested (image %s)", body.Branch, image), username)
	s.mu.Lock()
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == id {
			s.beginUpgrade(i, imageTag)
			break
		}
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": "switching",
		"branch": body.Branch,
		"image":  image,
	})
}

func (s *HubServer) handleToggleVisibility(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if isSameOriginAsHub(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "PUT, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	}
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	id := r.PathValue("id")
	username := s.getAuthUser(r)

	h := loadSaaSHive(id)
	if h == nil {
		http.Error(w, `{"error":"hive not found — only hosted hives can be toggled from here"}`, http.StatusNotFound)
		return
	}
	if !userIsHiveOwner(username, h) {
		http.Error(w, `{"error":"only the owner can change visibility"}`, http.StatusForbidden)
		return
	}

	var body struct {
		IsPublic bool `json:"is_public"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	// The hub's registry and SaaS store are the source of truth for the
	// dashboard, so persist the change here first — the same pattern used
	// by handleToggleAutoUpgrade. Pushing the new value to the spoke hive
	// is a best-effort, asynchronous notification: the spoke re-reports
	// its is_public value on every heartbeat, so a transient failure to
	// reach it (pod restarting, rollout in progress, etc.) must not block
	// or fail the user-facing toggle.
	h.IsPublic = body.IsPublic
	if err := saveSaaSHive(h); err != nil {
		http.Error(w, `{"error":"failed to save"}`, http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	for i, reg := range s.registry.Hives {
		if reg.ID == id {
			s.registry.Hives[i].IsPublic = body.IsPublic
			break
		}
	}
	s.mu.Unlock()

	s.logger.Info("audit: visibility toggled", "hive_id", id, "is_public", body.IsPublic, "by", username)

	go s.pushVisibilityToSpoke(id, body.IsPublic)

	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"ok":true,"is_public":%t}`, body.IsPublic)
}

// maxHiveDisplayNameLen bounds a hive's operator-set display name (ProjectName).
// It matches maxContactNameLen's order of magnitude — a human-readable label,
// not a document — and guards both the persisted meta.json and the vanity-host
// label derived from it (hiveNameHostLabel truncates to a DNS label anyway, so
// this only stops an absurdly long value from ever being stored).
const maxHiveDisplayNameLen = 100

// handleRenameHive renames a hive by rewriting its persisted ProjectName. The
// operator explicitly accepted that ProjectName is load-bearing — it feeds the
// namespace identity annotation and (at claim time) the vanity host — so this
// is a rename of the hive's real identity, NOT a separate display_name field.
//
// AUTHORIZATION mirrors handleToggleVisibility exactly: requireAuth on the
// route, then an owner-or-admin check here that is the true security boundary
// (a non-owner is rejected 403 regardless of what the UI shows).
//
// Derived-surface handling on rename:
//   - Namespace identity is RE-STAMPED here (idempotent, best-effort) so the
//     hive.kubestellar.io/display-name annotation tracks the new name.
//   - The vanity host is NOT recomputed. It is set-once at claim time (the
//     assign path only mints one when VanityURL is empty) and doubles as the
//     "placeholder is claimed" marker; minting a fresh random host on every
//     rename would churn URLs and orphan routes. So the vanity host keeps its
//     original label — a known, documented staleness, not a silent one.
func (s *HubServer) handleRenameHive(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if isSameOriginAsHub(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "PUT, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	}
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	id := r.PathValue("id")
	username := s.getAuthUser(r)

	h := loadSaaSHive(id)
	if h == nil {
		http.Error(w, `{"error":"hive not found — only hosted hives can be renamed from here"}`, http.StatusNotFound)
		return
	}
	if !userIsHiveOwner(username, h) {
		http.Error(w, `{"error":"only the owner can rename this hive"}`, http.StatusForbidden)
		return
	}

	var body struct {
		ProjectName string `json:"project_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	// Trim and cap. A blank value is a legitimate "clear the custom name" — the
	// dashboard then falls back to today's org/repo-derived label (hiveLabel).
	name := strings.TrimSpace(body.ProjectName)
	if len(name) > maxHiveDisplayNameLen {
		http.Error(w, `{"error":"name too long"}`, http.StatusBadRequest)
		return
	}
	name = sanitizeField(name)

	h.ProjectName = name
	if err := saveSaaSHive(h); err != nil {
		http.Error(w, `{"error":"failed to save"}`, http.StatusInternalServerError)
		return
	}

	// Overlay the new name onto the in-memory registry immediately so the change
	// is visible before the next heartbeat re-overlays it from the SaaS store.
	s.mu.Lock()
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == id {
			s.registry.Hives[i].ProjectName = name
			break
		}
	}
	s.mu.Unlock()

	// Re-stamp namespace identity so the display-name annotation tracks the
	// rename. Best-effort — see stampHostedNamespaceIdentity's doc comment; a
	// failed cosmetic patch must not fail the rename.
	stampHostedNamespaceIdentity(s.clusterForHive(h), hostedNamespaceForHive(h), h.ProjectName, h.Org, h.ID, s.logger)

	s.logger.Info("audit: hive renamed", "hive_id", id, "project_name", name, "by", username)
	s.recordTimeline(id, TimelineRenamed, fmt.Sprintf("hive renamed to %q", name), username)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "project_name": name})
}

func (s *HubServer) handleResetAgentRestarts(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	agentName := sanitizeHeartbeatField(r.PathValue("agent"))
	username := s.getAuthUser(r)
	if agentName == "" || !isValidName(agentName) {
		http.Error(w, `{"error":"invalid agent name"}`, http.StatusBadRequest)
		return
	}
	h := loadSaaSHive(id)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	if !userIsHiveOwner(username, h) {
		http.Error(w, `{"error":"only the owner can reset agent restarts"}`, http.StatusForbidden)
		return
	}

	total := 0
	s.mu.RLock()
	for _, reg := range s.registry.Hives {
		if reg.ID != id {
			continue
		}
		for _, a := range reg.Agents {
			if a.Name == agentName {
				total = a.Restarts.Total
				break
			}
		}
		break
	}
	s.mu.RUnlock()

	if h.AgentRestartResets == nil {
		h.AgentRestartResets = make(map[string]AgentRestartReset)
	}
	reset := AgentRestartReset{
		ResetAt:       time.Now().UTC().Format(time.RFC3339),
		By:            username,
		TotalBaseline: total,
		Pending:       true,
	}
	h.AgentRestartResets[agentName] = reset
	if err := saveSaaSHive(h); err != nil {
		http.Error(w, `{"error":"failed to save"}`, http.StatusInternalServerError)
		return
	}
	s.logger.Info("audit: agent restart counter reset", "hive_id", id, "agent", agentName, "by", username, "total_baseline", total)
	s.recordTimeline(id, TimelineRestarted, fmt.Sprintf("agent %s restart counter reset", agentName), username)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "agent": agentName, "reset_at": reset.ResetAt, "by": username})
}

func pendingAgentRestartResetsForHeartbeat(hiveID string) []string {
	h := loadSaaSHive(hiveID)
	if h == nil || len(h.AgentRestartResets) == 0 {
		return nil
	}
	var names []string
	changed := false
	for name, reset := range h.AgentRestartResets {
		if !reset.Pending {
			continue
		}
		names = append(names, name)
		reset.Pending = false
		reset.TotalBaseline = 0
		h.AgentRestartResets[name] = reset
		changed = true
	}
	if changed {
		_ = saveSaaSHive(h)
	}
	sort.Strings(names)
	return names
}

func applyAgentRestartResetBaselines(agents []AgentSummary, resets map[string]AgentRestartReset, now time.Time) {
	if len(resets) == 0 {
		return
	}
	cutoff := now.Add(-24 * time.Hour)
	for i := range agents {
		reset, ok := resets[agents[i].Name]
		if !ok {
			continue
		}
		agents[i].Restarts.ResetAt = reset.ResetAt
		agents[i].Restarts.ResetBy = reset.By
		resetAt, err := time.Parse(time.RFC3339, reset.ResetAt)
		if err != nil || resetAt.Before(cutoff) {
			continue
		}
		delta := agents[i].Restarts.Total - reset.TotalBaseline
		if delta < 0 {
			delta = 0
		}
		agents[i].Restarts.Last24h = delta
	}
}

func appendDriftSignal(report *DriftReport, kind string, sev DriftSeverity, reason string) {
	if report == nil {
		return
	}
	report.Signals = append(report.Signals, DriftSignal{Kind: kind, Severity: sev, Reason: reason})
	report.Count = len(report.Signals)
	if driftSeverityRank[sev] > driftSeverityRank[report.WorstSeverity] {
		report.WorstSeverity = sev
	}
}

// pushVisibilityToSpoke best-effort notifies a hosted hive's own governor
// config of a visibility change made from the hub dashboard. It never
// affects the outcome of the toggle request — the hub's registry/SaaS
// store already reflect the change; this just keeps the spoke's local
// config in sync so it doesn't overwrite the hub's value on its next
// heartbeat. Failures are logged, not surfaced to the user.
func (s *HubServer) pushVisibilityToSpoke(id string, isPublic bool) {
	const goAPIPort = 3002
	const visibilityPushTimeout = 10 * time.Second
	ns := "hive-hosted-" + id
	svcURL := fmt.Sprintf("http://hive.%s.svc.cluster.local:%d/api/config/governor/hub", ns, goAPIPort)
	payload := fmt.Sprintf(`{"is_public":%t}`, isPublic)
	req, err := http.NewRequest("PUT", svcURL, strings.NewReader(payload))
	if err != nil {
		s.logger.Warn("visibility spoke push: failed to create request", "hive", id, "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: visibilityPushTimeout}
	spokeResp, err := client.Do(req)
	if err != nil {
		s.logger.Warn("visibility spoke push failed, will resync on next heartbeat", "hive", id, "error", err)
		return
	}
	defer func() { _ = spokeResp.Body.Close() }()
	if spokeResp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(spokeResp.Body)
		s.logger.Warn("visibility spoke push rejected", "hive", id, "status", spokeResp.StatusCode, "body", string(respBody))
	}
}

func (s *HubServer) handleToggleAutoUpgrade(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if isSameOriginAsHub(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "PUT, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	}
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	id := r.PathValue("id")
	username := s.getAuthUser(r)
	h := loadSaaSHive(id)
	if h == nil {
		// Hive may exist in registry via heartbeat but have no SaaS entry yet.
		// Create a minimal entry so the auto-upgrade preference can be stored
		// and delivered via heartbeat response.
		s.mu.RLock()
		var regEntry *RegistryEntry
		for i := range s.registry.Hives {
			if s.registry.Hives[i].ID == id {
				regEntry = &s.registry.Hives[i]
				break
			}
		}
		s.mu.RUnlock()
		if regEntry == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"error":"hive not found"}`)
			return
		}
		h = &SaaSHive{
			ID:    id,
			Owner: regEntry.Owner,
			Org:   regEntry.Org,
			Repos: regEntry.Repos,
		}
	}
	if !userIsHiveOwner(username, h) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"error":"only the owner can change auto-upgrade"}`)
		return
	}
	// The mode rides on the EXISTING endpoint rather than a second one: it is
	// the same preference, the same owner-or-admin authorization checked above,
	// and the same persistence. A separate endpoint would let the two settings
	// drift apart across two requests. Older clients that send only
	// auto_upgrade omit the field, which reads as "" = instant, preserving
	// their behaviour exactly.
	var body struct {
		AutoUpgrade bool   `json:"auto_upgrade"`
		Mode        string `json:"auto_upgrade_mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":"invalid request body"}`)
		return
	}
	// Reject unknown modes instead of defaulting — a typo must not silently
	// change how often a hive restarts.
	if !isValidAutoUpgradeMode(body.Mode) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":"invalid auto_upgrade_mode (expected \"instant\", \"daily\" or \"weekly\")"}`)
		return
	}
	h.AutoUpgrade = body.AutoUpgrade
	h.AutoUpgradeMode = body.Mode
	// Switching modes clears the day's fire record. Otherwise a hive flipped to
	// daily after an instant upgrade earlier today would inherit a stale date
	// and skip tonight's window.
	h.AutoUpgradeLastFired = ""
	if err := saveSaaSHive(h); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, `{"error":"failed to save"}`)
		return
	}
	s.logger.Info("audit: auto-upgrade toggled", "hive_id", id, "auto_upgrade", body.AutoUpgrade, "mode", normalizeAutoUpgradeMode(body.Mode), "by", username)

	// If enabling auto-upgrade and hive is behind, trigger immediately via kubectl
	// for hosted hives. For heartbeat-connected hives, the upgrade instruction
	// is delivered via the heartbeat response.
	// Daily mode deliberately does NOT kick an upgrade here: the whole point of
	// choosing it is that turning auto-upgrade on should not immediately
	// restart a working hive. It will roll at the next daily ET window
	// (autoUpgradeDailyHour, currently 13:00 / 1pm ET). The
	// operator who wants it now still has the explicit Upgrade button, which
	// goes through handleUpgradeHive and is never gated by mode.
	// The kill switch does not block saving the PREFERENCE (auto-upgrade
	// on/off is configuration, not delivery) — only the immediate trigger
	// below. While paused, triggerAutoUpgrades stays suppressed anyway, and
	// the hive upgrades after an admin resumes.
	spokePauseSw, spokesPaused := s.spokeUpgradesPaused()
	if body.AutoUpgrade && spokesPaused {
		s.logger.Info("auto-upgrade initial trigger suppressed — spoke upgrades are paused",
			"hive_id", id, "paused_by", spokePauseSw.By, "paused_at", spokePauseSw.At)
	}
	if body.AutoUpgrade && !spokesPaused && normalizeAutoUpgradeMode(body.Mode) == AutoUpgradeModeInstant {
		s.mu.RLock()
		var currentSHA, branch string
		for _, reg := range s.registry.Hives {
			if reg.ID == id {
				currentSHA = reg.GitHash
				branch = reg.GitBranch
				break
			}
		}
		s.mu.RUnlock()
		branch = s.upgradeBranchOrDefault(branch)
		latestSHA := getLatestSHAForBranch(branch)
		if latestSHA != "" && currentSHA != "" && !sameCommit(currentSHA, latestSHA) {
			s.logger.Info("audit: auto-upgrade initial trigger", "hive_id", id, "from", currentSHA, "to", latestSHA)
			s.mu.Lock()
			for i := range s.registry.Hives {
				if s.registry.Hives[i].ID == id {
					s.beginUpgrade(i, latestSHA)
					break
				}
			}
			s.mu.Unlock()
			hiveCluster := s.clusterForHive(h)
			if hiveCluster != nil {
				ns := "hive-hosted-" + id
				cmd := kubectlForCluster(hiveCluster, "rollout", "restart", "deployment/hive", "-n", ns)
				if out, err := cmd.CombinedOutput(); err != nil {
					s.logger.Warn("auto-upgrade initial trigger failed (will retry via heartbeat)", "hive", id, "cluster", hiveCluster.ID, "output", string(out))
				}
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"ok":true,"auto_upgrade":%t,"auto_upgrade_mode":%q}`, body.AutoUpgrade, normalizeAutoUpgradeMode(body.Mode))
}

// branchSHAInfo holds a short SHA and the first line of its commit message.
type branchSHAInfo struct {
	SHA     string `json:"sha"`
	Message string `json:"message"`
}

// Container-image build status values for a branch head commit, exposed to
// the dashboard as latest_sha_image_status.
const (
	imageStatusReady    = "ready"    // image tag verified on GHCR
	imageStatusBuilding = "building" // docker workflow queued/in progress, or image not yet visible
	imageStatusFailed   = "failed"   // docker workflow completed unsuccessfully
	imageStatusStale    = "stale"    // still non-ready past the configured build window

	imageBuildStaleAfterEnv     = "HIVE_IMAGE_BUILD_STALE_AFTER"
	defaultImageBuildStaleAfter = 90 * time.Minute
)

// branchHeadInfo tracks the branch HEAD commit, which may be ahead of the
// image-verified SHA in latestSHAByBranch while its container image builds.
type branchHeadInfo struct {
	SHA         string
	Message     string
	ImageStatus string
	// BuildStartedAt is when THIS SHA first entered the "building" status, so the
	// dashboard can show an elapsed build timer. It is stamped once on the
	// ready→building (or new-SHA→building) transition and preserved across polls
	// while the same SHA keeps building; it is zeroed when the image finishes
	// (ready/failed) or a new SHA takes over. Zero means "not building / unknown".
	BuildStartedAt time.Time
	// BuildURL is the GitHub Actions run URL for the docker workflow run that
	// established a terminal/non-ready state, when GitHub reported one.
	BuildURL string
}

// githubAPIBase and ghcrBase are the GitHub/GHCR origins used by the SHA-poll
// fetch helpers. They are vars (not consts) so tests can point those helpers at
// a local httptest server; production never reassigns them.
var (
	githubAPIBase = "https://api.github.com"
	ghcrBase      = "https://ghcr.io"
)

// GHCR image repositories built by .github/workflows/docker.yml. Each is a
// SEPARATE build job tagged with the short git SHA, so one can succeed while
// another fails for the same commit. The hub runs ghcrRepoHub; spokes run
// ghcrRepoSpoke. The hub self-upgrade MUST verify ghcrRepoHub (not ghcrRepoSpoke)
// before targeting a SHA — otherwise a failed hive-hub build leaves the hub
// chasing a SHA whose hub image was never pushed, and the rollout falls back to
// a stale cached image.
const (
	ghcrRepoSpoke = "hivecommons/hive"
	ghcrRepoHub   = "hivecommons/hive-hub"

	// hubDeploymentName / hubContainerName / hubNamespace identify the hub's own
	// Kubernetes objects for self-upgrade. NOTE the container is named "hub", not
	// "hive-hub" — a `kubectl set image` that names the wrong container silently
	// no-ops the target (or errors), so pin the image against hubContainerName.
	hubDeploymentName = "hive-hub"
	hubContainerName  = "hub"
	hubNamespace      = "hive-hub"

	// hubUpgradeStateFailed is the hubUpgradeState() value meaning the hub is
	// behind latest AND its last upgrade attempt was refused or wedged, so it
	// cannot reach latest without operator action.
	hubUpgradeStateFailed = "failed"
)

// hubImageExists reports whether the hub's own container image is published on
// GHCR for the given SHA. A var (not a plain call) so tests can stub the GHCR
// round-trip; production checks ghcrRepoHub over the network.
var hubImageExists = func(sha string, logger *slog.Logger) bool {
	client := &http.Client{Timeout: 10 * time.Second}
	return ghcrTagExists(client, ghcrRepoHub, sha, logger)
}

// spokeImageExists is the ghcrRepoSpoke counterpart of hubImageExists: it
// reports whether a SPOKE tag (a "<branch>-latest" channel tag or a SHA) is
// actually published. Separate from hubImageExists because the two repos are
// independent build jobs — the hub image for a SHA can exist while the spoke
// image for that same SHA does not. A var so tests can stub the GHCR round-trip.
var spokeImageExists = func(tag string, logger *slog.Logger) bool {
	client := &http.Client{Timeout: 10 * time.Second}
	return ghcrTagExists(client, ghcrRepoSpoke, tag, logger)
}

var (
	latestSHAMu sync.RWMutex
	// latestSHAByBranch only ever advances to SHAs whose container image is
	// verified pullable on GHCR — it drives upgrade targets.
	latestSHAByBranch = map[string]branchSHAInfo{}
	// latestHubSHAByBranch is the same idea for the HUB's own image, which is a
	// separate build from the spoke's (ghcrRepoHub vs ghcrRepoSpoke) and can
	// succeed or fail independently for the same commit. Tracking one shared
	// value meant the hub's upgrade target was gated on the SPOKE image: when a
	// spoke build failed but the hub build succeeded, that SHA never entered
	// latestSHAByBranch, so the hub could never roll to it and reported
	// "current" against a stale target.
	latestHubSHAByBranch = map[string]branchSHAInfo{}
	// headSHAByBranch advances to the branch HEAD immediately so the
	// dashboard can show the newest commit with a build-status indicator.
	headSHAByBranch = map[string]branchHeadInfo{}
	// commitMsgBySHA caches the first line of each commit message, keyed by short SHA.
	commitMsgBySHA = map[string]string{}
)

// trackedBranches lists the legacy always-tracked branches that still produce
// Docker images via CI. Personal dev branches (e.g. mk) are tracked
// dynamically: see HubServer.trackedBranchList. v3 is retired and must not be
// offered solely because old image tags or persisted SHA cache entries linger.
var trackedBranches = []string{"v2"}

var retiredBranches = map[string]struct{}{
	"v3": {},
}

// trackedBranchList returns the static CI branches plus every branch some
// registered hive is assigned to, so a personal dev branch gets SHA polling,
// Latest-images display, branch-switch validation, and auto-upgrade without
// a hub code change per branch. Static branches keep their order and come
// first. Caller must not hold s.mu.
func (s *HubServer) trackedBranchList() []string {
	seen := make(map[string]bool, len(trackedBranches))
	out := make([]string, 0, len(trackedBranches))
	add := func(b string) {
		if _, retired := retiredBranches[b]; retired {
			return
		}
		if b != "" && !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	for _, b := range trackedBranches {
		add(b)
	}
	// Branches already assigned to a hive.
	s.mu.RLock()
	for _, h := range s.registry.Hives {
		add(h.GitBranch)
	}
	s.mu.RUnlock()
	// Any branch that has a published <branch>-latest image on GHCR, even
	// with no hive assigned yet — so the UI lists every assignable branch
	// and a user can switch to one before any hive uses it.
	for _, b := range discoveredImageBranches() {
		add(b)
	}
	return out
}

var (
	imageBranchMu       sync.RWMutex
	imageBranchCache    []string
	imageBranchCachedAt time.Time
)

const imageBranchCacheTTL = 5 * time.Minute

// discoveredImageBranches returns branch names inferred from published
// ghcr.io/hivecommons/hive:<branch>-latest tags (cached). A tag with a '-'
// that our sanitizer would have produced can't be reversed unambiguously, so
// we only surface tags that round-trip: the tag minus the "-latest" suffix.
// Slashless branches (v2, mk) round-trip exactly; slashed branches
// (feat/x → feat-x-latest) surface as "feat-x", which is still a valid
// switch target because switch-branch/branchToTag normalize both to the same
// image tag.
func discoveredImageBranches() []string {
	imageBranchMu.RLock()
	if time.Since(imageBranchCachedAt) < imageBranchCacheTTL && imageBranchCache != nil {
		cp := append([]string(nil), imageBranchCache...)
		imageBranchMu.RUnlock()
		return cp
	}
	imageBranchMu.RUnlock()

	const listTimeout = 8 * time.Second
	client := &http.Client{Timeout: listTimeout}
	imageBranches := listLatestImageBranches(client)

	// A merged branch is deleted but its <branch>-latest image lingers on
	// GHCR, so the image list alone would offer dead branches in the
	// switcher. Keep only image branches whose branch still EXISTS on the
	// repo. Branch names are compared in their sanitized (tag) form because
	// the image tag is branchToTag(branch); e.g. real "feat/x" ⇒ image
	// "feat-x-latest" ⇒ we must match it back to the live "feat/x".
	live := map[string]struct{}{}
	for _, b := range listRepoBranches(client) {
		live[branchToTag(b)] = struct{}{}
	}
	branches := imageBranches
	if len(live) > 0 { // only filter when the branch list fetch succeeded
		branches = branches[:0]
		for _, b := range imageBranches {
			if _, ok := live[b]; ok {
				branches = append(branches, b)
			}
		}
	}

	imageBranchMu.Lock()
	imageBranchCache = branches
	imageBranchCachedAt = time.Now()
	imageBranchMu.Unlock()
	return branches
}

// listRepoBranches returns the names of branches on hivecommons/hive via the
// GitHub API (paginated). Best-effort: returns nil on any failure so callers
// treat "unknown" as "don't filter" rather than hiding valid branches.
func listRepoBranches(client *http.Client) []string {
	var names []string
	url := githubAPIBase + "/repos/hivecommons/hive/branches?per_page=100"
	const maxPages = 10
	for page := 0; url != "" && page < maxPages; page++ {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := client.Do(req)
		if err != nil {
			return nil
		}
		var body []struct {
			Name string `json:"name"`
		}
		decErr := json.NewDecoder(resp.Body).Decode(&body)
		link := resp.Header.Get("Link")
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || decErr != nil {
			return nil
		}
		for _, b := range body {
			names = append(names, b.Name)
		}
		url = nextGitHubLink(link)
	}
	return names
}

// nextGitHubLink extracts the rel="next" URL from a GitHub Link header (an
// absolute URL), or "".
func nextGitHubLink(link string) string {
	for _, part := range strings.Split(link, ",") {
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		if i, j := strings.Index(part, "<"), strings.Index(part, ">"); i >= 0 && j > i {
			return part[i+1 : j]
		}
	}
	return ""
}

// listLatestImageBranches queries the GHCR tag list for
// ghcr.io/hivecommons/hive and returns the branch name of every "<x>-latest"
// tag (the "<x>" part).
func listLatestImageBranches(client *http.Client) []string {
	tokenResp, err := client.Get(ghcrBase + "/token?scope=repository:hivecommons/hive:pull")
	if err != nil {
		return nil
	}
	defer func() { _ = tokenResp.Body.Close() }()
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(tokenResp.Body).Decode(&tok); err != nil {
		return nil
	}

	// The registry tag list is paginated via RFC5988 Link headers. Untagged
	// build digests aside, the repo can hold thousands of SHA tags, so the
	// "<branch>-latest" tags we want may live on a later page — follow Link
	// until exhausted (bounded) rather than reading only the first page.
	branchSet := map[string]struct{}{}
	next := ghcrBase + "/v2/hivecommons/hive/tags/list?n=1000"
	const maxPages = 20 // bound: up to ~20k tags
	for page := 0; next != "" && page < maxPages; page++ {
		req, _ := http.NewRequest("GET", next, nil)
		req.Header.Set("Authorization", "Bearer "+tok.Token)
		resp, err := client.Do(req)
		if err != nil {
			break
		}
		var body struct {
			Tags []string `json:"tags"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&body)
		link := resp.Header.Get("Link")
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || decodeErr != nil {
			break
		}
		for _, t := range body.Tags {
			if strings.HasSuffix(t, "-latest") {
				branchSet[strings.TrimSuffix(t, "-latest")] = struct{}{}
			}
		}
		next = nextLinkURL(link)
	}
	branches := make([]string, 0, len(branchSet))
	for b := range branchSet {
		branches = append(branches, b)
	}
	return branches
}

// nextLinkURL extracts the rel="next" URL from a registry Link header, or "".
// GHCR returns a relative path (e.g. </v2/.../tags/list?last=...&n=1000>),
// which we resolve against the registry host.
func nextLinkURL(link string) string {
	for _, part := range strings.Split(link, ",") {
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		start := strings.Index(part, "<")
		end := strings.Index(part, ">")
		if start < 0 || end < 0 || end <= start {
			return ""
		}
		u := part[start+1 : end]
		if strings.HasPrefix(u, "/") {
			return ghcrBase + u
		}
		return u
	}
	return ""
}

// provisionWG tracks async hive-provisioning goroutines so tests (which swap
// the package-level saas*Dir path variables) can wait for them to drain
// before mutating shared state.
var provisionWG sync.WaitGroup

const latestSHAPollInterval = 2 * time.Minute

func getLatestSHA() string {
	return getLatestSHAForBranch("v2")
}

func getLatestSHAForBranch(branch string) string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	return latestSHAByBranch[branch].SHA
}

// getLatestHubSHAForBranch returns the newest SHA on this branch whose HUB
// image is verified pullable. Use this — never getLatestSHAForBranch — for the
// hub's own upgrade target, since the hub and spoke images are separate builds
// that can fail independently for the same commit.
func getLatestHubSHAForBranch(branch string) string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	return latestHubSHAByBranch[branch].SHA
}

// getLatestHubSHAs returns a branch→SHA map of the newest commit per branch
// whose HUB image is verified pullable. Exposed alongside getLatestSHAs so an
// operator can see the two independent build results side by side: the hub and
// spoke images are separate builds that can succeed in either order (or fail
// independently) for the same commit, which is why the caches are separate.
func getLatestHubSHAs() map[string]string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	cp := make(map[string]string, len(latestHubSHAByBranch))
	for k, v := range latestHubSHAByBranch {
		cp[k] = v.SHA
	}
	return cp
}

// getHeadSHAs returns a branch→HEAD-SHA map: the newest commit the poller has
// seen on each branch, regardless of whether its images exist yet. Comparing
// this against getLatestSHAs tells an operator WHY the advertised SHA is behind
// HEAD — see getImageStatuses for the distinguishing signal.
func getHeadSHAs() map[string]string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	cp := make(map[string]string, len(headSHAByBranch))
	for k, v := range headSHAByBranch {
		if v.SHA != "" {
			cp[k] = v.SHA
		}
	}
	return cp
}

// getLatestSHAs returns a branch→SHA map (backward-compatible string values).
func getLatestSHAs() map[string]string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	cp := make(map[string]string, len(latestSHAByBranch))
	for k, v := range latestSHAByBranch {
		cp[k] = v.SHA
	}
	return cp
}

// getLatestSHAMessages returns a branch→commit-message map for tooltip display.
func getLatestSHAMessages() map[string]string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	cp := make(map[string]string, len(latestSHAByBranch))
	for k, v := range latestSHAByBranch {
		cp[k] = v.Message
	}
	return cp
}

// getCommitMessages returns a short-SHA→commit-message map for tooltip display.
func getCommitMessages() map[string]string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	cp := make(map[string]string, len(commitMsgBySHA))
	for k, v := range commitMsgBySHA {
		cp[k] = v
	}
	return cp
}

// getBranchHead returns the tracked HEAD info for a branch (zero value if
// no head fetch has succeeded since startup).
func getBranchHead(branch string) branchHeadInfo {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	return headSHAByBranch[branch]
}

// setBranchHead records the branch HEAD and its image build status, keeping
// the previous commit message when the new fetch didn't include one.
func setBranchHead(branch, sha, msg, status string) {
	setBranchHeadDetails(branch, sha, msg, status, "")
}

func setBranchHeadDetails(branch, sha, msg, status, buildURL string) {
	latestSHAMu.Lock()
	defer latestSHAMu.Unlock()
	prev := headSHAByBranch[branch]
	if msg == "" && prev.SHA == sha {
		msg = prev.Message
	}
	if buildURL == "" && prev.SHA == sha && status != imageStatusReady {
		buildURL = prev.BuildURL
	}
	// Stamp the build-start time on the first poll that sees this SHA building,
	// and carry it forward on every subsequent poll while the same SHA is still
	// building/stale — so the dashboard's elapsed timer counts from when the build
	// actually started, not from each poll. Clear it once the image is
	// ready/failed or a different SHA takes over.
	var buildStartedAt time.Time
	if status == imageStatusBuilding || status == imageStatusStale {
		if prev.SHA == sha && !prev.BuildStartedAt.IsZero() {
			buildStartedAt = prev.BuildStartedAt // same build, keep the original start
		} else {
			buildStartedAt = time.Now() // newly observed building SHA
		}
	}
	headSHAByBranch[branch] = branchHeadInfo{SHA: sha, Message: msg, ImageStatus: status, BuildStartedAt: buildStartedAt, BuildURL: buildURL}
	if msg != "" {
		commitMsgBySHA[sha] = msg
	}
}

// getDisplaySHAs returns the branch→SHA map shown under "Latest images":
// the branch HEAD when known (its image may still be building), falling back
// to the last image-verified SHA (e.g. right after a hub restart, before the
// first head fetch succeeds).
func getDisplaySHAs() map[string]string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	cp := make(map[string]string, len(latestSHAByBranch)+len(headSHAByBranch))
	for k, v := range latestSHAByBranch {
		cp[k] = v.SHA
	}
	for k, v := range headSHAByBranch {
		if v.SHA != "" {
			cp[k] = v.SHA
		}
	}
	return cp
}

// getDisplaySHAMessages returns branch→commit-message for the SHAs returned
// by getDisplaySHAs.
func getDisplaySHAMessages() map[string]string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	cp := make(map[string]string, len(latestSHAByBranch)+len(headSHAByBranch))
	for k, v := range latestSHAByBranch {
		cp[k] = v.Message
	}
	for k, v := range headSHAByBranch {
		if v.SHA != "" {
			cp[k] = v.Message
		}
	}
	return cp
}

// getImageStatuses returns branch→image build status for the SHAs returned
// by getDisplaySHAs. Branches known only from the image-verified cache are
// ready by definition.
func getImageStatuses() map[string]string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	cp := make(map[string]string, len(latestSHAByBranch)+len(headSHAByBranch))
	for k := range latestSHAByBranch {
		cp[k] = imageStatusReady
	}
	now := time.Now()
	for k, v := range headSHAByBranch {
		if v.SHA != "" && v.ImageStatus != "" {
			cp[k] = buildStatusWithStaleness(v.ImageStatus, v.BuildStartedAt, now)
		}
	}
	return cp
}

// getImageBuildURLs returns branch→docker workflow run URL for non-ready head
// states when GitHub reported a run.
func getImageBuildURLs() map[string]string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	cp := make(map[string]string, len(headSHAByBranch))
	now := time.Now()
	for k, v := range headSHAByBranch {
		status := buildStatusWithStaleness(v.ImageStatus, v.BuildStartedAt, now)
		if v.SHA != "" && v.BuildURL != "" && status != "" && status != imageStatusReady {
			cp[k] = v.BuildURL
		}
	}
	return cp
}

// imageBuildStaleAfter is the maximum time the UI may call a non-ready head
// "building" before surfacing it as stale/unknown.
func imageBuildStaleAfter() time.Duration {
	raw := strings.TrimSpace(os.Getenv(imageBuildStaleAfterEnv))
	if raw == "" {
		return defaultImageBuildStaleAfter
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return defaultImageBuildStaleAfter
	}
	return d
}

// buildStatusWithStaleness applies the stale cap at read/poll time without
// rewriting terminal failed/ready states.
func buildStatusWithStaleness(status string, started time.Time, now time.Time) string {
	if status == imageStatusBuilding && !started.IsZero() && now.Sub(started) > imageBuildStaleAfter() {
		return imageStatusStale
	}
	return status
}

// getImageBuildStartTimes returns branch→unix-millis of when each currently
// "building" head first started building, for the dashboard's elapsed build
// timer. Only branches that are actively building have an entry; ready/failed/
// stale/unknown branches are omitted. Millis match the JS side (Date.now()).
func getImageBuildStartTimes() map[string]int64 {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	cp := make(map[string]int64, len(headSHAByBranch))
	now := time.Now()
	for k, v := range headSHAByBranch {
		if v.SHA != "" && v.ImageStatus == imageStatusBuilding && !v.BuildStartedAt.IsZero() &&
			buildStatusWithStaleness(v.ImageStatus, v.BuildStartedAt, now) == imageStatusBuilding {
			cp[k] = v.BuildStartedAt.UnixMilli()
		}
	}
	return cp
}

// latestSHAsPath persists the last-known-good branch→SHA cache across hub
// restarts (PVC-backed, like the rest of /data/saas). The hub restarts on
// every auto-upgrade; without this file the cache starts empty, and if the
// unauthenticated GitHub branches API is rate-limited for one branch at
// startup, that branch silently disappears from "Latest images" until a
// later poll succeeds (up to an hour under rate limiting).
var latestSHAsPath = "/data/saas/latest-shas.json"

// snapshotBranchSHAs returns a copy of the full branch→info cache.
func snapshotBranchSHAs() map[string]branchSHAInfo {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	cp := make(map[string]branchSHAInfo, len(latestSHAByBranch))
	for k, v := range latestSHAByBranch {
		cp[k] = v
	}
	return cp
}

// loadPersistedSHAs restores the last-known-good SHA cache from disk so a
// freshly restarted hub serves the previous values while live fetches are
// failing or rate-limited. Branches no longer in trackedBranches are dropped;
// branches already populated by a live fetch are never overwritten.
func loadPersistedSHAs(logger *slog.Logger, branches []string) {
	data, err := os.ReadFile(latestSHAsPath)
	if err != nil {
		return // first run or no PVC — nothing to restore
	}
	var persisted map[string]branchSHAInfo
	if err := json.Unmarshal(data, &persisted); err != nil {
		logger.Warn("SHA poll: persisted SHA cache unreadable, ignoring", "path", latestSHAsPath, "error", err)
		return
	}
	latestSHAMu.Lock()
	defer latestSHAMu.Unlock()
	for _, branch := range branches {
		info, ok := persisted[branch]
		if !ok || info.SHA == "" {
			continue
		}
		if _, exists := latestSHAByBranch[branch]; exists {
			continue // live fetch already populated this branch
		}
		latestSHAByBranch[branch] = info
		if info.Message != "" {
			commitMsgBySHA[info.SHA] = info.Message
		}
		logger.Info("SHA poll: restored last-known SHA from disk", "branch", branch, "sha", info.SHA)
	}
}

// persistLatestSHAs writes the current SHA cache to disk (atomic tmp+rename,
// same pattern as the other /data/saas state files).
func persistLatestSHAs(logger *slog.Logger) {
	snapshot := snapshotBranchSHAs()
	if len(snapshot) == 0 {
		return // never overwrite a good file with an empty cache
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		logger.Warn("SHA poll: persist marshal failed", "error", err)
		return
	}
	// Best-effort: a failed mkdir surfaces via the WriteFile error below.
	_ = os.MkdirAll(filepath.Dir(latestSHAsPath), 0o755)
	tmpPath := latestSHAsPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		logger.Warn("SHA poll: persist write failed", "path", latestSHAsPath, "error", err)
		return
	}
	if err := os.Rename(tmpPath, latestSHAsPath); err != nil {
		logger.Warn("SHA poll: persist rename failed", "path", latestSHAsPath, "error", err)
	}
}

// StartLatestSHAPoller polls GitHub/GHCR for the latest branch SHAs until ctx
// is cancelled. It takes a context so the loop can be stopped for a clean
// shutdown (and so tests can stop the goroutine rather than leaking it past the
// test, which otherwise races the package-level saas path state that per-test
// temp-dir setup rewrites — same class as startProvisionWatcher).
func (s *HubServer) StartLatestSHAPoller(ctx context.Context) {
	// Serve last-known-good SHAs immediately; live fetches below refresh them.
	loadPersistedSHAs(s.logger, s.trackedBranchList())
	prevInfos := snapshotBranchSHAs()
	fetchAllBranchSHAs(s.logger, s.trackedBranchList())
	if cur := snapshotBranchSHAs(); !maps.Equal(cur, prevInfos) {
		persistLatestSHAs(s.logger)
	}
	// On first poll, check if any auto-upgrade hives are behind
	s.triggerAutoUpgrades()
	// Clear Upgrading flags orphaned by spoke pods that vanished mid-upgrade.
	// Runs alongside — not inside — triggerAutoUpgrades because that function
	// only ever considers hives with AutoUpgrade enabled, while the flag is set
	// by the admin and bulk upgrade paths for any hive. Throttled internally to
	// orphanedUpgradeSweepInterval — corrective work, not a hot path.
	s.sweepOrphanedUpgradesIfDue()
	// Auto-reset any placeholder wedged at assigned && !claim_delivered past the
	// timeout, so an assigned-but-unclaimed slot can never dead-end silently.
	// Throttled internally to stuckAssignmentSweepInterval — a wedge only
	// becomes actionable after assignStuckResetTimeout, far longer than a tick.
	s.sweepStuckAssignmentsIfDue()
	// Repair pre-#1222 NET_ADMIN securityContext drift so the F5 fatal-egress
	// image (#2664) can't crash-loop drifted hives. Throttled internally to
	// netAdminReconcileInterval — this poller ticks far more often than the
	// static drift needs re-checking. See netadmin_reconcile.go / issue #2674.
	s.reconcileNetAdminIfDue()
	// Ensure the five per-hive security env vars are present and correct on
	// every hosted spoke. Nothing else re-asserts them after provision time, so
	// without this the fleet's key posture survives only as an out-of-band
	// manual patch. Throttled internally to perHiveEnvReconcileInterval and
	// rate-limited to perHiveEnvMaxPatchesPerCycle patches per cycle, because
	// each patch rolls that hive's pod. See perhive_env_reconcile.go.
	s.reconcilePerHiveEnvIfDue()
	// Force-delete hive-namespace pods stuck in Terminating past
	// orphanedPodMinAge with no finalizers and a non-Running phase — the
	// residue of nodes disappearing without draining (#5328). Throttled
	// internally to orphanedPodReapInterval and capped per cycle; nothing else
	// ever removes these, so without this they accumulate indefinitely (32
	// measured across 16 namespaces, oldest three weeks). See
	// orphaned_pod_reaper.go.
	s.reapOrphanedPodsIfDue()
	// Drop master generations whose verify window has closed, and warn when one
	// is closing while spokes still carry it. Throttled internally to
	// generationRetireInterval. This lane PERSISTS the drop and ALERTS; it is
	// not what enforces expiry — acceptableGenerations already refuses an
	// expired generation at every verify, on the wall clock, whether or not
	// this ever runs. See hub_generations_retire.go.
	s.retireExpiredGenerationsIfDue()
	// Persist and audit the revocation of expired Manage Access grants (#4150).
	// Throttled internally to accessExpirySweepInterval. This lane PERSISTS the
	// prune and stamps the timeline event; it is not what enforces expiry —
	// loadSaaSUser already drops an expired grant at every read, on the wall
	// clock, whether or not this ever runs. See access_expiry.go.
	s.sweepExpiredAccessIfDue()
	// Keep each cluster's placeholder pool at its configured watermark so
	// approvals never dead-end on "no available placeholder". Throttled
	// internally to poolReplenishInterval; disabled per cluster unless
	// pool_target is set. See pool_replenisher.go.
	s.replenishPoolsIfDue()
	// Record the per-release image-pulls snapshots (external-adoption chart). The
	// call is internally guarded to snapshot only when a release line's SHA
	// advances, so ticking it alongside the frequent SHA poll is cheap — no
	// separate scheduler. See image_pulls.go.
	s.maybeSnapshotImagePulls(ctx, time.Now())
	ticker := time.NewTicker(latestSHAPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		s.pollLatestSHAsTick(ctx, time.Now())
	}
}

// pollLatestSHAsTick is the body of StartLatestSHAPoller's ticker loop,
// extracted so it can be invoked directly by tests without waiting on the
// live ticker: re-fetches every tracked branch SHA, persists on change, drives
// the throttled reconciliation lanes, and runs the hub auto-upgrade check.
// now is injected so tests control the clock the same way
// maybeSnapshotImagePulls already does.
func (s *HubServer) pollLatestSHAsTick(ctx context.Context, now time.Time) {
	oldSHAs := getLatestSHAs()
	oldInfos := snapshotBranchSHAs()
	// Re-resolve each tick so branches from newly registered hives are
	// picked up without a hub restart.
	fetchAllBranchSHAs(s.logger, s.trackedBranchList())
	newSHAs := getLatestSHAs()
	if !maps.Equal(snapshotBranchSHAs(), oldInfos) {
		persistLatestSHAs(s.logger)
	}
	// Always check for pending auto-upgrades (retries failed/missed hives).
	s.triggerAutoUpgrades()
	s.sweepOrphanedUpgradesIfDue()
	s.sweepStuckAssignmentsIfDue()
	s.reconcileNetAdminIfDue()
	s.reconcilePerHiveEnvIfDue()
	s.reapOrphanedPodsIfDue()
	s.retireExpiredGenerationsIfDue()
	s.sweepExpiredAccessIfDue()
	s.replenishPoolsIfDue()
	s.maybeSnapshotImagePulls(ctx, now)
	changed := false
	for branch, sha := range newSHAs {
		if sha != "" && sha != oldSHAs[branch] {
			changed = true
			break
		}
	}
	_ = changed
	// Hub auto-upgrade — checked EVERY cycle, not only when the SHA just
	// changed. Previously this lived inside `if changed {}`, so if the hub
	// missed the one poll where v2's SHA flipped (busy, mid-restart, or the
	// SHA moved between polls), it stayed "queued" forever and never retried,
	// while spokes retry every cycle via triggerAutoUpgrades() above. Mirror
	// that: whenever auto-upgrade is on and the hub is behind latest v2, trigger
	// a rollout restart. A debounce prevents re-restarting every 2min while a
	// restart is already rolling out (the new pod reports the new hash, which
	// clears the condition, but the poll can fire before the rollout lands).
	// Use the hub's OWN branch, not a hardcoded "v2": hubUpgradeState() and
	// handleHubSelfUpgrade() both read s.hubGitBranch, so hardcoding here
	// made the badge and the poller disagree the moment a hub ran on v3.
	hubBranchSHA := getLatestHubSHAForBranch(s.hubGitBranch)
	s.hubUpgradeMu.Lock()
	debounced := time.Since(s.lastHubUpgradeTrigger) > hubUpgradeDebounce
	s.hubUpgradeMu.Unlock()
	// Admin kill switch: while hub upgrades are paused the hub stays on its
	// current build regardless of new tags — the auto-trigger below never
	// fires. Checked every cycle (like the trigger itself), so flipping the
	// switch takes effect on the next poll without a restart.
	if hubPauseSw, hubPaused := s.hubUpgradesPaused(); hubPaused {
		if isHubAutoUpgrade() && hubBranchSHA != "" && !sameCommit(hubBranchSHA, s.hubGitHash) {
			s.logger.Debug("hub auto-upgrade suppressed — hub upgrades are paused",
				"behind", hubBranchSHA, "paused_by", hubPauseSw.By, "paused_at", hubPauseSw.At)
		}
	} else if isHubAutoUpgrade() && hubBranchSHA != "" && !sameCommit(hubBranchSHA, s.hubGitHash) && debounced {
		s.logger.Info("audit: hub auto-upgrade triggered", "from", s.hubGitHash, "to", hubBranchSHA)
		// rolloutHubToSHA verifies the hub image exists (skips a doomed roll
		// when the hive-hub build for this SHA failed) and pins the SHA so a
		// stale cached v2-latest can't come back up. It records the in-flight
		// state on success so the dashboard shows "Upgrading", not "queued".
		if err := s.rolloutHubToSHA(hubBranchSHA); err != nil {
			s.logger.Warn("hub auto-upgrade skipped", "to", hubBranchSHA, "reason", err)
		}
	}
}

// clusterRecentlyUnreachable reports whether the hub failed to reach this
// cluster recently enough that another kubectl attempt would just burn the
// timeout again. Callers should go straight to the heartbeat fallback instead.
func (s *HubServer) clusterRecentlyUnreachable(clusterID string) bool {
	if clusterID == "" {
		return false
	}
	// A pull-only cluster is unreachable BY DECLARATION and permanently, so it
	// never has to be learned the expensive way. This breaker exists because
	// discovering unreachability costs a full dial timeout per hive per cycle —
	// ~90s a call, with a pool of hives serialising into tens of minutes of
	// blocking. Saying so in clusters.json means that price is never paid even
	// once, and every caller that already consults this breaker is covered
	// without needing its own pull-only check.
	if c, ok := s.clusters[clusterID]; ok && c.PullOnly {
		return true
	}
	s.clusterUnreachableMu.Lock()
	defer s.clusterUnreachableMu.Unlock()
	until, ok := s.clusterUnreachableUntil[clusterID]
	return ok && time.Now().Before(until)
}

// markClusterUnreachable starts (or extends) the kubectl suppression window for
// a cluster the hub just failed to dial.
func (s *HubServer) markClusterUnreachable(clusterID string) {
	if clusterID == "" {
		return
	}
	s.clusterUnreachableMu.Lock()
	defer s.clusterUnreachableMu.Unlock()
	if s.clusterUnreachableUntil == nil {
		s.clusterUnreachableUntil = make(map[string]time.Time)
	}
	s.clusterUnreachableUntil[clusterID] = time.Now().Add(clusterUnreachableTTL)
}

// markClusterReachable clears any suppression after a kubectl call succeeds, so
// a cluster that recovers is used immediately rather than waiting out the TTL.
func (s *HubServer) markClusterReachable(clusterID string) {
	if clusterID == "" {
		return
	}
	s.clusterUnreachableMu.Lock()
	defer s.clusterUnreachableMu.Unlock()
	delete(s.clusterUnreachableUntil, clusterID)
}

// PUSH PATH RETIRED. rolloutRestartHive() used to live here and issued
// `kubectl rollout restart deployment/hive` into a spoke's cluster. Every
// caller has been converted to the pull model — the hub records a target and
// the spoke collects it on its own outbound heartbeat — so the function is
// gone rather than left dormant.
//
// WHY DELETED RATHER THAN FLAG-DISABLED. A disabled flag would keep the hub's
// need for write-capable kubeconfigs alive in the code, which is precisely the
// standing blast radius this change exists to remove: compromise the hub and
// you inherit write access into every reachable spoke cluster. A flag also
// leaves two live delivery models with no stated direction, and the dormant
// one inevitably rots — it would be the untested path on the day someone
// flipped it back. Deleting it makes the direction unambiguous and lets the
// operator drop or downgrade those kubeconfigs to read-only.
//
// The cost is real and accepted: delivery is now bounded by the heartbeat
// interval instead of being immediate. Nothing is lost in reliability — the
// heartbeat was already the only path that worked for firewalled clusters, and
// the spoke-side self-upgrade is mature (retry budget, per-target attempt
// marker, terminal failure reported back to the hub).

func (s *HubServer) triggerAutoUpgrades() {
	// Admin kill switch: while spoke upgrades are paused this entire reconciler
	// is a no-op — it must start no new upgrades, issue no kubectl restarts,
	// and (critically) not re-arm heartbeatUpgrade through its stale-recovery
	// path, which otherwise re-delivers in-flight targets every cycle. The
	// registry latches and armed targets are left untouched so resuming picks
	// up exactly where the fleet paused.
	if sw, paused := s.spokeUpgradesPaused(); paused {
		s.logger.Debug("auto-upgrade reconciler suppressed — spoke upgrades are paused",
			"paused_by", sw.By, "paused_at", sw.At)
		return
	}
	hives := listSaaSHives()
	// Upgrade waves: bound how many hives may be UPGRADING per cluster at
	// once. A merge used to roll every behind hive simultaneously — observed
	// live as a fleet-wide restart inside minutes, an image-pull + PVC IO
	// storm. Count the in-flight upgrades per cluster first; the arming gate
	// below starts new upgrades only while a cluster is under its wave size.
	// Recovery/latch-clearing paths are deliberately NOT bounded (corrective,
	// not disruptive), and wait-healthy is implicit: Upgrading clears when a
	// spoke reports the target reached, freeing wave slots for the next tick.
	upgradingByCluster := make(map[string]int)
	s.mu.RLock()
	upgradingIDs := make(map[string]bool)
	for _, reg := range s.registry.Hives {
		if reg.Upgrading {
			upgradingIDs[reg.ID] = true
		}
	}
	s.mu.RUnlock()
	for i := range hives {
		if upgradingIDs[hives[i].ID] {
			upgradingByCluster[clusterIDForHive(&hives[i])]++
		}
	}
	waveSize := upgradeWaveSize()
	for _, h := range hives {
		s.mu.RLock()
		var currentSHA, branch, upgradeTarget, imageRef, lastHeartbeat string
		var alreadyUpgrading bool
		var upgradeStartedAt time.Time
		for _, reg := range s.registry.Hives {
			if reg.ID == h.ID {
				currentSHA = reg.GitHash
				branch = reg.GitBranch
				alreadyUpgrading = reg.Upgrading
				upgradeTarget = reg.UpgradeTarget
				upgradeStartedAt = reg.UpgradeStartedAt
				imageRef = reg.ImageRef
				lastHeartbeat = reg.LastHeartbeat
				break
			}
		}
		s.mu.RUnlock()
		// Resolve against the hub's own branch when the hive has not reported
		// one, never a hardcoded "v2" — see upgradeBranchOrDefault. A hardcoded
		// v2 is what armed 0b78dc0 (a v2-only commit) at placeholders on a v4
		// hub.
		branch = s.upgradeBranchOrDefault(branch)
		if alreadyUpgrading {
			// Floating-tag convergence. A hive whose Deployment tracks a MUTABLE
			// tag (…-latest) has no stable target commit: a restart re-pulls the
			// tag and lands on whatever CI last published, so its reported GitHash
			// chases an ever-moving branch HEAD and can never equal the SPECIFIC
			// commit the hub armed. Left to the stale-recovery path below, that
			// hive is re-armed and rolled every staleUpgradeTimeout forever —
			// restarting the spoke pod each cycle for no benefit. Once such a hive
			// reports it is running the image-verified latest for its branch, it IS
			// up to date; clear the latch here instead of advancing the target.
			// Commit-pinned hives are unaffected — their tag resolves to exactly
			// one build, so the specific-target check still governs them.
			//
			// "Latest for its branch" is resolved THROUGH the tracked tag
			// (#5994). A :stable spoke's ceiling is the digest :stable carries,
			// not branch HEAD, so judging it against HEAD kept it latched for
			// the whole soak window — the 41 spokes measured stuck this way —
			// while it had already converged on everything its tag offers.
			registryLatestSHA := s.reachableUpgradeTarget(branch, imageRef, h.TrackedChannel).SHA
			if imageTagIsMutable(imageRef) && registryLatestSHA != "" &&
				sameCommit(currentSHA, registryLatestSHA) {
				s.mu.Lock()
				for i := range s.registry.Hives {
					if s.registry.Hives[i].ID == h.ID {
						s.clearUpgradeLatch(i)
						break
					}
				}
				delete(s.heartbeatUpgrade, h.ID)
				s.mu.Unlock()
				s.logger.Info("clearing upgrade latch — floating-tag hive is at latest",
					"hive", h.ID, "branch", branch, "sha", currentSHA, "image_ref", imageRef)
				continue
			}
			// Target reached or SURPASSED. The equality checks above cannot see
			// a spoke that landed AHEAD of the armed target: a floating-tag
			// re-pull delivers whatever CI last published, so when the branch
			// advanced between arming and pulling — and again before this cycle
			// — the reported hash equals neither the target nor the current
			// latest. Without this ancestry check the stale-recovery below
			// re-arms the ORIGINAL stale pin forever (manual upgrades never
			// advance their target), re-stamping the registry latch and
			// re-instructing a commit the spoke can never report — the
			// vllmd-13 wedge. Cache-only + background resolve, so this loop
			// never blocks on the network; an unresolved pair clears on a
			// later cycle.
			if upgradeTarget != "" && currentSHA != "" &&
				commitAtOrAheadOfTarget(currentSHA, upgradeTarget, s.logger) {
				s.mu.Lock()
				for i := range s.registry.Hives {
					if s.registry.Hives[i].ID == h.ID {
						s.clearUpgradeLatch(i)
						break
					}
				}
				delete(s.heartbeatUpgrade, h.ID)
				s.mu.Unlock()
				s.logger.Info("clearing upgrade latch — hive is at or ahead of its armed target",
					"hive", h.ID, "branch", branch, "sha", currentSHA, "target", upgradeTarget)
				continue
			}
			// Latched-upgrade recovery runs for EVERY hive, deliberately BEFORE
			// the AutoUpgrade gate below (#2476). The registry latch
			// (Upgrading/UpgradeTarget/UpgradeStartedAt) is durable, but
			// heartbeatUpgrade — the map that actually delivers the instruction
			// — is in-memory; when this recovery lived behind
			// `if !h.AutoUpgrade { continue }`, a hub restart orphaned every
			// manually upgraded hive forever: latched "Upgrading" in the
			// registry, zero instructions on the wire.
			upgradeAge := time.Since(upgradeStartedAt)
			// Zero UpgradeStartedAt means the timestamp was lost (heartbeats
			// used to wipe it on rebuild) — treat as stale so already-stuck
			// hives self-heal instead of upgrading forever.
			isStale := upgradeStartedAt.IsZero() || upgradeAge > staleUpgradeTimeout

			if isStale {
				// UNCOLLECTIBLE: abandon, do not re-arm. This is the branch that
				// produced the measured wedge — 26 hives re-arming, stale_minutes
				// climbing past 146 and still rising — because re-arming an
				// instruction nothing will ever pick up reproduces the identical
				// no-op every staleUpgradeTimeout, forever, while beginUpgrade()
				// preserves the original start clock so the elapsed only ever
				// grows. The retry budget in sweepOrphanedUpgrades() cannot bound
				// it: these hives never heartbeated, so evaluateOrphanedUpgrade()
				// bails on the liveness test and the budget is never spent.
				//
				// Clearing the latch outright is what stops staleness
				// accumulating. The hive stays on its old SHA — truthful and
				// visible — instead of misreporting as perpetually "Upgrading"
				// while reading offline. Nothing is lost: the instruction was
				// never being collected, so dropping it forfeits nothing. If the
				// spoke later starts heartbeating, the normal arming path picks
				// it up on the next poll.
				if !upgradeCollectible(lastHeartbeat, time.Now()) {
					reason := uncollectibleUpgradeReason(lastHeartbeat)
					s.logger.Warn("abandoning stale upgrade — hive cannot collect it, not re-arming",
						"hive", h.ID, "stale_minutes", int(upgradeAge.Minutes()),
						"target", upgradeTarget, "last_heartbeat", orDash(lastHeartbeat),
						"reason", reason)
					s.mu.Lock()
					for i := range s.registry.Hives {
						if s.registry.Hives[i].ID == h.ID {
							s.clearUpgradeLatch(i)
							break
						}
					}
					delete(s.heartbeatUpgrade, h.ID)
					s.mu.Unlock()
					s.noteUncollectibleUpgrade(h.ID, upgradeTarget, reason)
					continue
				}
				// Upgrade has been stuck longer than staleUpgradeTimeout.
				// Recover it. Two things can be wrong: (a) the target SHA
				// contains a crashing bug a newer commit fixes — advance the
				// target to latest; (b) the kubectl rollout never reached the
				// spoke (e.g. the hub can't route to the hive's cluster API),
				// so the upgrade was never actually delivered.
				//
				// The heartbeat fallback (heartbeatUpgrade → the spoke
				// self-restarts on its next heartbeat) is the ONLY path that
				// works when kubectl can't reach the cluster, so re-arm it
				// unconditionally for a stale upgrade — not only when the
				// target advances. Previously, when the target already equalled
				// latest, this branch was skipped entirely and the hive stayed
				// latched-upgrading forever behind an unreachable kubectl.
				recoverTarget := upgradeTarget
				if h.AutoUpgrade {
					// Target advancement is an auto-upgrade behaviour: those
					// hives always chase latest. A manual upgrade keeps exactly
					// the target that was requested — with AutoUpgrade off,
					// silently delivering a newer build than the one the owner
					// clicked would override their setting.
					if latestSHA := getLatestSHAForBranch(branch); latestSHA != "" && latestSHA != upgradeTarget {
						recoverTarget = latestSHA
					}
				}
				if recoverTarget != upgradeTarget {
					s.logger.Warn("advancing upgrade target for stale upgrade",
						"hive", h.ID, "stale_minutes", int(upgradeAge.Minutes()),
						"old_target", upgradeTarget, "new_target", recoverTarget)
				} else {
					s.logger.Warn("re-arming heartbeat fallback for stale upgrade",
						"hive", h.ID, "stale_minutes", int(upgradeAge.Minutes()),
						"target", recoverTarget)
				}
				if recoverTarget != "" {
					s.mu.Lock()
					for i := range s.registry.Hives {
						if s.registry.Hives[i].ID == h.ID {
							// Route through beginUpgrade so the start time
							// SURVIVES a same-target re-arm (the crash-loop retry
							// case): re-arming delivery for the same target must
							// not reset the elapsed clock, or a thrashing upgrade
							// never crosses staleUpgradeTimeout and the stuck-
							// upgrade alert never fires. A genuinely advanced
							// target (recoverTarget != old UpgradeTarget) is a new
							// upgrade and DOES get a fresh clock. A lost/zero start
							// is re-stamped either way, which self-heals the
							// ibm-alchemy zero-timestamp wedge.
							s.beginUpgrade(i, recoverTarget)
							break
						}
					}
					s.heartbeatUpgrade[h.ID] = recoverTarget
					s.mu.Unlock()
					// PULL ONLY — the heartbeat armed above IS the delivery, as
					// the previous comment here already conceded ("the heartbeat
					// fallback armed above is what actually delivers the
					// upgrade"). The kubectl push that followed it was pure
					// latency optimisation and is deliberately gone; see the
					// push-path retirement note in pullonly_upgrade.go.
					continue
				}
			}

			// Not stale — keep the original target so the hive can satisfy it.
			// Re-populate the heartbeatUpgrade map in case the hub restarted.
			//
			// SAME COLLECTIBILITY GATE AS THE STALE BRANCH ABOVE. Arming is
			// arming: re-populating the map for a hive that cannot collect
			// reproduces the wedge the stale branch just abandoned, only
			// sooner. Because this branch runs on EVERY poll while the hive is
			// latched, it re-arms roughly every 2 minutes, whereas abandonment
			// waits out staleUpgradeTimeout — so without this check the fix
			// merely races the timeout and the uncollectible hive stays armed.
			// The predicate is upgradeCollectible(), reused rather than
			// restated, so there is one definition of "can collect".
			if upgradeTarget != "" && !upgradeCollectible(lastHeartbeat, time.Now()) {
				s.logger.Debug("not re-arming in-progress upgrade — hive cannot collect it",
					"hive", h.ID, "target", upgradeTarget,
					"last_heartbeat", orDash(lastHeartbeat))
				continue
			}
			hiveCluster := s.clusterForHive(&h)
			if hiveCluster != nil && !hiveCluster.InCluster {
				if upgradeTarget != "" {
					s.mu.Lock()
					s.heartbeatUpgrade[h.ID] = upgradeTarget
					s.mu.Unlock()
				}
			}
			s.logger.Debug("skipping target advance — upgrade still in progress",
				"hive", h.ID, "current", currentSHA)
			continue
		}
		// Everything below STARTS a new upgrade, which only auto-upgrade hives
		// opt into. The recovery above must stay ahead of this gate — see #2476.
		if !h.AutoUpgrade {
			continue
		}
		// Claim-in-flight latch (#95). A placeholder that has just been ASSIGNED
		// but whose claim has not yet been delivered (Status==assigned &&
		// !ClaimDelivered) is mid-wiring: the spoke is receiving its org/repos/
		// ACMM over successive heartbeats. Rolling its pod onto a new image now
		// can wedge it — the classic EPM dead-end (task #94). DEFER (do not
		// cancel) the auto-upgrade until the claim lands. This is self-limiting:
		// it releases the moment ClaimDelivered flips true, and if the claim
		// never completes, sweepStuckAssignments returns the slot to available
		// after assignStuckResetTimeout — either way this latch clears and the
		// next cycle upgrades normally. A hard image pin is unaffected: pins are
		// delivered via UpgradeTarget through the recovery path ABOVE this gate,
		// not started here, so a pin still wins.
		if assignmentInFlight(&h) {
			s.logger.Debug("auto-upgrade deferred — claim in flight",
				"hive_id", h.ID, "status", h.Status, "assigned_at", h.AssignedAt)
			continue
		}
		// Scheduling gate. Instant-mode hives (and every legacy record, whose
		// mode is empty) pass straight through, so this changes nothing for the
		// existing fleet. Daily-mode hives are held until the first cycle at or
		// after autoUpgradeDailyHour ET and released only once per ET day.
		// Evaluated BEFORE any of the work below so a held hive costs nothing.
		decision := shouldAutoUpgradeNow(h.AutoUpgradeMode, h.AutoUpgradeLastFired, time.Now())
		if !decision.Allowed {
			s.logger.Debug("auto-upgrade held by schedule",
				"hive_id", h.ID, "mode", h.AutoUpgradeMode, "reason", decision.Reason)
			continue
		}
		// Skip hives that are actively provisioning or in error state.
		// Empty status means the hive predates the provisioning system — treat as eligible.
		if h.Status == "provisioning" || h.Status == "error" {
			continue
		}
		if currentSHA == "" {
			continue
		}
		// Target what this spoke's TAG can actually deliver (#5994). Branch HEAD
		// is the right answer only for a spoke tracking that branch's moving
		// tag; a spoke on :stable can reach exactly the digest :stable carries,
		// and instructing anything else is an instruction it will spend its
		// entire retry budget failing to satisfy.
		reach := s.reachableUpgradeTarget(branch, imageRef, h.TrackedChannel)
		if !reach.Resolved {
			// Channel-tracking spoke whose channel would not resolve. Refuse
			// LOUDLY and instruct nothing. Falling back to branch HEAD here is
			// exactly the bug, and it would be the worst-evidenced fallback
			// available: we know the spoke's ceiling is not HEAD, and we do not
			// know what it is. The next cycle retries.
			s.logger.Warn("auto-upgrade held — the spoke's release channel did not resolve to a commit",
				"hive_id", h.ID, "channel", reach.Channel, "branch", branch,
				"current", currentSHA, "image_ref", imageRef)
			continue
		}
		latestSHA := reach.SHA
		if latestSHA == "" || sameCommit(currentSHA, latestSHA) {
			continue
		}
		// A channel is a moving POINTER, not a monotonic branch, so a spoke can
		// legitimately sit AHEAD of the one it tracks — it was rolled while the
		// channel was further along, or it tracked :candidate until a moment
		// ago. Targeting the channel SHA there would instruct a DOWNGRADE: a
		// failure mode this path never had while it only ever chased HEAD.
		if reach.Channel != "" && commitAtOrAheadOfTarget(currentSHA, latestSHA, s.logger) {
			s.logger.Debug("auto-upgrade skipped — hive is at or ahead of its release channel",
				"hive_id", h.ID, "channel", reach.Channel,
				"current", currentSHA, "channel_sha", latestSHA)
			continue
		}
		// Merge-driven debounce (#5391). Reached ONLY on the automatic
		// chase-latest path: everything that starts an upgrade for an operator
		// — a manual "Upgrade now" (upgradeHiveHandler), a bulk upgrade
		// (saas_bulk.go), and a hard image pin (delivered as UpgradeTarget
		// through the stale-recovery branch ABOVE the `if !h.AutoUpgrade` gate)
		// — arms s.heartbeatUpgrade directly and never enters this loop body.
		// So an operator's upgrade and a pin stay IMMEDIATE by construction,
		// and only the merge-frequency-driven roll is held.
		//
		// Placed after every eligibility gate above so a hive that would not
		// upgrade anyway never arms a window, and before the wave gate and the
		// fire-date persistence below so a debounced hive costs no wave slot and
		// keeps its daily/weekly window open.
		debounce := shouldDebounceAutoUpgrade(
			autoUpgradeDebounceState{
				Target:       h.AutoUpgradePendingTarget,
				ArmedAt:      h.AutoUpgradePendingSince,
				FirstArmedAt: h.AutoUpgradePendingFirst,
				Collapsed:    h.AutoUpgradeCollapsed,
			},
			latestSHA, autoUpgradeDebounceInterval(), autoUpgradeMaxHold(), time.Now())
		if !debounce.Allowed {
			// Persist the (possibly just-replaced) pending target so a hub
			// restart inside the window resumes it rather than dropping it.
			s.persistUpgradeDebounceState(&h, debounce.State)
			s.logger.Info("auto-upgrade debounced — holding for a quiet branch",
				"hive_id", h.ID, "branch", branch,
				"target", debounce.State.Target, "current", currentSHA,
				"collapsed", debounce.State.Collapsed,
				"debounce", autoUpgradeDebounceInterval(),
				"reason", debounce.Reason)
			continue
		}
		if debounce.Collapsed > 0 {
			// Report the collapse. Silent batching would trade one invisible
			// problem for another: without this line, N merges producing one
			// roll is indistinguishable from N-1 upgrades having been lost.
			s.logger.Info("auto-upgrade debounce collapsed a merge burst into one roll",
				"hive_id", h.ID, "branch", branch,
				"merges_collapsed", debounce.Collapsed+1,
				"final_target", latestSHA, "current", currentSHA,
				"debounce", autoUpgradeDebounceInterval())
		}
		hiveCluster := s.clusterForHive(&h)
		if hiveCluster == nil {
			s.logger.Warn("auto-upgrade skipped — no cluster config", "hive_id", h.ID, "cluster_id", h.ClusterID)
			continue
		}
		// Do not ARM an upgrade this hive cannot COLLECT. Delivery is PULL: the
		// spoke reads UpgradeTo off its own outbound heartbeat response and then
		// patches its own Deployment with its own ServiceAccount. A hive that
		// never heartbeats therefore never picks the instruction up, while
		// Upgrading=true latches on the hub: the stale-recovery branch above
		// re-arms every staleUpgradeTimeout, beginUpgrade() preserves the
		// original start clock, and the elapsed grows without bound. The orphan
		// sweep's retry budget cannot rescue it — a hive that never heartbeated
		// fails evaluateOrphanedUpgrade()'s liveness test, so the budget is never
		// spent and exhaustion never converts it to a visible failure. See
		// pullonly_upgrade.go for the full measured loop.
		//
		// This is deliberately NOT gated on cluster reachability. The hub's
		// kubectl path is only a fast-path optimisation, so a pull-only cluster
		// is irrelevant here; gating on it would silently disable auto-upgrade
		// for the 40+ pull-only spokes that heartbeat perfectly well.
		//
		// Refused LOUDLY, never silently: a hive with auto_upgrade=true that
		// simply never upgrades is indistinguishable from one already at latest,
		// which is how this stayed unnoticed.
		if !upgradeCollectible(lastHeartbeat, time.Now()) {
			reason := uncollectibleUpgradeReason(lastHeartbeat)
			s.logger.Warn("auto-upgrade not armed — hive cannot collect the instruction",
				"hive_id", h.ID, "cluster", hiveCluster.ID, "branch", branch,
				"from", currentSHA, "would_have_targeted", latestSHA,
				"last_heartbeat", orDash(lastHeartbeat), "reason", reason)
			s.noteUncollectibleUpgrade(h.ID, latestSHA, reason)
			continue
		}
		// Wave gate — evaluated AFTER every eligibility check so a slot is
		// only ever spent on a hive that would actually arm, and BEFORE the
		// fire-date persistence so a deferred daily/weekly hive keeps its
		// window open and simply boards a later wave this same day.
		if waveSize > 0 && upgradingByCluster[hiveCluster.ID] >= waveSize {
			s.logger.Debug("auto-upgrade deferred — cluster upgrade wave is full",
				"hive_id", h.ID, "cluster", hiveCluster.ID,
				"in_flight", upgradingByCluster[hiveCluster.ID], "wave_size", waveSize)
			continue
		}
		upgradingByCluster[hiveCluster.ID]++
		// Record the day's fire BEFORE kicking the rollout. Persisting first
		// means a hub crash between here and the restart cannot cause a second
		// upgrade for the same ET day; at worst the hive waits for tomorrow's
		// window, which is the conservative direction for a "don't disturb it"
		// mode. Only daily-mode hives carry a fire date (decision.FireDate is
		// empty for instant), and a save failure is logged but not fatal — the
		// upgrade itself still proceeds.
		if decision.FireDate != "" {
			stored := loadSaaSHive(h.ID)
			if stored == nil {
				stored = &h
			}
			stored.AutoUpgradeLastFired = decision.FireDate
			if err := saveSaaSHive(stored); err != nil {
				s.logger.Warn("failed to persist auto-upgrade fire date — a hub restart today could re-fire",
					"hive_id", h.ID, "date", decision.FireDate, "error", err)
			}
		}
		// Clear the debounce record now the roll is actually going out. Cleared
		// HERE, after every gate that could still `continue`, so a hive turned
		// away by the wave gate keeps its pending target and simply boards a
		// later wave instead of re-arming a fresh window each cycle. Clearing
		// before the rollout (like the fire date above) means a hub crash in
		// between costs at most a re-armed window, never a duplicate roll.
		s.persistUpgradeDebounceState(&h, autoUpgradeDebounceState{})
		// The hive is deliverable again — drop any suppressed-refusal memory so a
		// future undeliverable episode is reported afresh rather than swallowed.
		s.forgetUncollectibleUpgrade(h.ID)
		s.logger.Info("audit: auto-upgrade triggered", "hive_id", h.ID, "branch", branch, "from", currentSHA, "to", latestSHA, "cluster", hiveCluster.ID, "mode", normalizeAutoUpgradeMode(h.AutoUpgradeMode))
		s.recordTimeline(h.ID, TimelineUpgradeStarted,
			fmt.Sprintf("auto-upgrade triggered on %s: %s → %s", branch, orDash(currentSHA), latestSHA), "auto-upgrade")
		s.mu.Lock()
		for i := range s.registry.Hives {
			if s.registry.Hives[i].ID == h.ID {
				s.beginUpgrade(i, latestSHA)
				break
			}
		}
		s.mu.Unlock()
		// PULL ONLY — no kubectl push. Arming the heartbeat is the delivery:
		// the spoke reads UpgradeTo off its next heartbeat response and patches
		// its own Deployment with its own ServiceAccount (cmd/hive/main.go →
		// self_upgrade.go). The former `rolloutRestartHive` call here was only a
		// latency optimisation, and it is deliberately gone: keeping it would
		// require the hub to hold write-capable kubeconfigs into every spoke
		// cluster, which is a large standing blast radius for a few seconds of
		// speed. See the push-path retirement note in pullonly_upgrade.go.
		//
		// The trade is real and accepted: delivery is now bounded by the
		// heartbeat interval rather than being immediate.
		s.mu.Lock()
		s.heartbeatUpgrade[h.ID] = latestSHA
		// Keep Upgrading=true so the dashboard shows the correct state.
		s.mu.Unlock()
	}
}

func fetchAllBranchSHAs(logger *slog.Logger, branches []string) {
	for _, branch := range branches {
		fetchBranchSHA(logger, branch)
	}
}

func fetchBranchSHA(logger *slog.Logger, branch string) {
	// Step 1: get the latest commit SHA on the branch from the GitHub API
	const shaFetchTimeout = 10 * time.Second
	client := &http.Client{Timeout: shaFetchTimeout}
	branchURL := fmt.Sprintf("%s/repos/hivecommons/hive/branches/%s", githubAPIBase, branch)
	req, _ := http.NewRequest("GET", branchURL, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		logger.Warn("SHA poll: branch API request failed", "branch", branch, "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		logger.Warn("SHA poll: branch API non-200", "branch", branch, "status", resp.StatusCode)
		// Backfill missing commit messages for already-cached SHAs
		backfillCommitMessage(client, branch, logger)
		return
	}
	var branchResult struct {
		Commit struct {
			SHA    string `json:"sha"`
			Commit struct {
				Message string `json:"message"`
			} `json:"commit"`
		} `json:"commit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&branchResult); err != nil {
		logger.Warn("SHA poll: branch decode failed", "branch", branch, "error", err)
		return
	}
	if len(branchResult.Commit.SHA) < StandardSHALen {
		logger.Warn("SHA poll: branch SHA too short", "branch", branch, "sha", branchResult.Commit.SHA)
		return
	}
	candidateSHA := shortSHA(branchResult.Commit.SHA)
	// Extract only the first line of the commit message for tooltip display.
	commitMsg := branchResult.Commit.Commit.Message
	if idx := strings.Index(commitMsg, "\n"); idx >= 0 {
		commitMsg = commitMsg[:idx]
	}

	// Step 2: verify that a container image with this SHA tag exists on GHCR.
	// The image-verified cache (latestSHAByBranch) only advances once it does;
	// the head cache advances immediately with a build-status indicator so the
	// dashboard can show the new commit while its image builds.
	prevHead := getBranchHead(branch)
	headChanged := prevHead.SHA != candidateSHA

	// The hub image is a SEPARATE build from the spoke image and can land in
	// either order (or fail independently). Probe it on its own so the hub's
	// upgrade target is never gated on the spoke build, and vice versa.
	if candidateSHA != getLatestHubSHAForBranch(branch) &&
		ghcrTagExists(client, ghcrRepoHub, candidateSHA, logger) {
		latestSHAMu.Lock()
		latestHubSHAByBranch[branch] = branchSHAInfo{SHA: candidateSHA, Message: commitMsg}
		latestSHAMu.Unlock()
		logger.Info("SHA poll: hub image verified on GHCR", "branch", branch, "sha", candidateSHA)
	}

	if candidateSHA == getLatestSHAForBranch(branch) {
		// Head unchanged since its image was verified — nothing to re-check.
		setBranchHead(branch, candidateSHA, commitMsg, imageStatusReady)
		return
	}

	if ghcrTagExists(client, ghcrRepoSpoke, candidateSHA, logger) {
		// If commit message is empty (rate-limited or missing), fetch it separately
		// from the commits API using the full SHA (one-shot, only on new SHAs).
		if commitMsg == "" {
			commitMsg = fetchCommitMessage(client, branchResult.Commit.SHA, logger)
		}
		latestSHAMu.Lock()
		latestSHAByBranch[branch] = branchSHAInfo{SHA: candidateSHA, Message: commitMsg}
		commitMsgBySHA[candidateSHA] = commitMsg
		latestSHAMu.Unlock()
		setBranchHead(branch, candidateSHA, commitMsg, imageStatusReady)
		logger.Info("SHA poll: latest image verified on GHCR", "branch", branch, "sha", candidateSHA)
		return
	}

	// Image not on GHCR yet — ask the docker workflow whether the build for
	// this head commit is still running or has failed.
	buildState := fetchImageBuildState(client, branchResult.Commit.SHA, logger)
	status := buildState.Status
	if status == "" {
		// Actions API unavailable (rate-limited/network): keep the last-known
		// status for this head; a brand-new head with no image is presumed
		// building. Never invent "failed" from an API error.
		status = prevHead.ImageStatus
		if headChanged || status == "" || status == imageStatusReady {
			status = imageStatusBuilding
		}
	}
	if commitMsg == "" && headChanged {
		commitMsg = fetchCommitMessage(client, branchResult.Commit.SHA, logger)
	}
	if status == imageStatusBuilding {
		started := prevHead.BuildStartedAt
		if headChanged || started.IsZero() {
			started = time.Now()
		}
		status = buildStatusWithStaleness(status, started, time.Now())
	}
	setBranchHeadDetails(branch, candidateSHA, commitMsg, status, buildState.RunURL)
	logger.Info("SHA poll: container image not yet on GHCR", "branch", branch, "sha", candidateSHA, "image_status", status, "build_url", buildState.RunURL)
}

// dockerWorkflowFile is the workflow that builds and pushes the container
// images (ghcr.io/hivecommons/hive:<branch>-latest and :<short-sha>) on
// every push to a tracked branch.
const dockerWorkflowFile = "docker.yml"

// fetchImageBuildStatus queries the docker workflow run for a specific head
// commit and maps it to an image build status. Returns "" when the API is
// unavailable so the caller can keep the last-known status instead of
// flapping ready/building on transient errors.
type imageBuildState struct {
	Status string
	RunURL string
}

func fetchImageBuildStatus(client *http.Client, fullSHA string, logger *slog.Logger) string {
	return fetchImageBuildState(client, fullSHA, logger).Status
}

func fetchImageBuildState(client *http.Client, fullSHA string, logger *slog.Logger) imageBuildState {
	runsURL := fmt.Sprintf("%s/repos/hivecommons/hive/actions/workflows/%s/runs?head_sha=%s&per_page=1", githubAPIBase, dockerWorkflowFile, fullSHA)
	req, _ := http.NewRequest("GET", runsURL, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		logger.Warn("SHA poll: workflow runs request failed", "sha", fullSHA, "error", err)
		return imageBuildState{}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		logger.Warn("SHA poll: workflow runs non-200", "sha", fullSHA, "status", resp.StatusCode)
		return imageBuildState{}
	}
	var result struct {
		WorkflowRuns []struct {
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			HTMLURL    string `json:"html_url"`
		} `json:"workflow_runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		logger.Warn("SHA poll: workflow runs decode failed", "sha", fullSHA, "error", err)
		return imageBuildState{}
	}
	if len(result.WorkflowRuns) == 0 {
		// The push event may not have spawned the workflow run yet. If it never does,
		// the caller's stale timer flips the UI out of the spinner state.
		return imageBuildState{Status: imageStatusBuilding}
	}
	run := result.WorkflowRuns[0]
	state := imageBuildState{RunURL: run.HTMLURL}
	if run.Status != "completed" {
		state.Status = imageStatusBuilding // queued, in_progress, waiting, pending
		return state
	}
	switch run.Conclusion {
	case "success":
		// Workflow finished but the manifest isn't visible on GHCR yet —
		// treat as still publishing; the GHCR check flips it to ready, while the
		// stale timer prevents an endless spinner if publication never appears.
		state.Status = imageStatusBuilding
	case "failure", "cancelled", "skipped", "timed_out", "action_required", "startup_failure", "neutral":
		state.Status = imageStatusFailed
	default:
		state.Status = imageStatusFailed
	}
	return state
}

// fetchCommitMessage fetches the first line of a commit message from the GitHub API.
// Uses a separate endpoint that's less likely to be rate-limited since it's called
// only once per new SHA (not every poll cycle).
func fetchCommitMessage(client *http.Client, fullSHA string, logger *slog.Logger) string {
	commitURL := fmt.Sprintf("%s/repos/hivecommons/hive/commits/%s", githubAPIBase, fullSHA)
	req, _ := http.NewRequest("GET", commitURL, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		logger.Warn("SHA poll: commit message fetch non-200", "sha", fullSHA[:7], "status", resp.StatusCode)
		return ""
	}
	var result struct {
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ""
	}
	msg := result.Commit.Message
	if idx := strings.Index(msg, "\n"); idx >= 0 {
		msg = msg[:idx]
	}
	return msg
}

// backfillCommitMessage fills in a missing commit message for an already-cached SHA.
// Called when the branches API is rate-limited but we have the SHA from a prior poll.
func backfillCommitMessage(client *http.Client, branch string, logger *slog.Logger) {
	latestSHAMu.RLock()
	info := latestSHAByBranch[branch]
	latestSHAMu.RUnlock()
	if info.SHA == "" || info.Message != "" {
		return // no SHA cached, or message already present
	}
	msg := fetchCommitMessage(client, info.SHA, logger)
	if msg == "" {
		return
	}
	latestSHAMu.Lock()
	info.Message = msg
	latestSHAByBranch[branch] = info
	commitMsgBySHA[info.SHA] = msg
	latestSHAMu.Unlock()
	logger.Info("SHA poll: backfilled commit message", "branch", branch, "sha", info.SHA, "message", msg)
}

// ghcrTagExists checks whether a container tag exists on ghcr.io/<repo> (e.g.
// ghcrRepoSpoke or ghcrRepoHub). Uses an anonymous token (public package) and a
// HEAD on the manifest endpoint. The repo is a parameter because the hub and the
// spoke are DIFFERENT images built by separate jobs — verifying the wrong one
// lets the hub target a SHA whose own image was never published.
func ghcrTagExists(client *http.Client, repo, tag string, logger *slog.Logger) bool {
	// Get anonymous pull token
	tokenResp, err := client.Get(ghcrBase + "/token?scope=repository:" + repo + ":pull")
	if err != nil {
		logger.Warn("SHA poll: GHCR token request failed", "repo", repo, "error", err)
		return false
	}
	defer func() { _ = tokenResp.Body.Close() }()
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(tokenResp.Body).Decode(&tok); err != nil {
		return false
	}

	manifestURL := fmt.Sprintf("%s/v2/%s/manifests/%s", ghcrBase, repo, tag)
	req, _ := http.NewRequest("HEAD", manifestURL, nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// hiveConfigSSRFGuard is the SSRF check for handleProxyHiveConfig. It is a var
// (not a direct isPrivateURL call) solely so tests can override it to reach a
// loopback httptest server; production always uses isPrivateURL.
var hiveConfigSSRFGuard = isPrivateURL

func (s *HubServer) handleProxyHiveConfig(w http.ResponseWriter, r *http.Request) {
	hiveID := r.PathValue("hiveID")
	caller := s.getAuthUser(r)
	s.mu.RLock()
	var dashURL, owner string
	for _, h := range s.registry.Hives {
		if h.ID == hiveID && h.DashboardURL != "" {
			dashURL = h.DashboardURL
			owner = h.Owner
			break
		}
	}
	s.mu.RUnlock()
	if dashURL == "" {
		http.Error(w, `{"error":"hive not found or no dashboard URL"}`, http.StatusNotFound)
		return
	}
	// Ownership: a hive's config is private to its owner (and site admins). This
	// endpoint proxies a server-side fetch of a self-reported DashboardURL, so
	// without this any authenticated user could pull any hive's config.
	//
	// F9 (CWE-862): an OWNERLESS registry entry (owner == "") must NOT be treated
	// as world-readable. Previously the check was gated on `owner != ""`, so a
	// hive with no owner fell through and its raw config was fetchable by ANY
	// authenticated hub user. Fail closed: only the site admin may pull an
	// ownerless hive's config.
	if !isHubAdmin(caller) && (owner == "" || !canonicalEqual(caller, owner)) {
		http.Error(w, `{"error":"not authorized for this hive"}`, http.StatusForbidden)
		return
	}
	// SSRF guard: DashboardURL is self-reported by the spoke, so refuse to fetch
	// internal / link-local / private targets (e.g. 169.254.169.254 cloud
	// metadata, cluster-internal services). Uses the same guard as the public
	// registry (see registry rendering ~server.go:1626). Indirected through a
	// var so tests can point at a loopback httptest server.
	if hiveConfigSSRFGuard(r.Context(), dashURL) {
		http.Error(w, `{"error":"dashboard URL not permitted"}`, http.StatusForbidden)
		return
	}
	const proxyConfigTimeout = 10 * time.Second
	const maxConfigResponseBytes = 1 << 20
	client := &http.Client{
		Timeout: proxyConfigTimeout,
		// Do NOT follow redirects — a 30x could send us from a public
		// DashboardURL to an internal host, re-opening the SSRF the guard closes.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get(strings.TrimRight(dashURL, "/") + "/api/config/download")
	if err != nil {
		slog.Warn("hive config proxy failed", "hiveID", hiveID, "error", err)
		http.Error(w, `{"error":"could not reach hive"}`, http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxConfigResponseBytes))
	w.Header().Set("Content-Type", "application/x-yaml")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

func (s *HubServer) handleLatestSHA(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"sha": getLatestSHA()})
}

// authorizedUsersForHiveID returns the hub's access list for a hive as
// "username:role" entries, for delivery to the spoke in its heartbeat response.
// Returns nil (not an empty slice) when the hive has no SaaS record, so a
// non-hosted spoke's own allowlist is left untouched. The owner is always
// included as owner even if no explicit access record names them.
func authorizedUsersForHiveID(hiveID string) []string {
	users, _ := authorizedUsersAndNamesForHiveID(hiveID)
	return users
}

// authorizedUsersAndNamesForHiveID does the shared work behind
// authorizedUsersForHiveID and the heartbeat handler's AuthorizedUserNames
// delivery: one roster scan producing both the authoritative "username:role"
// allowlist and its cosmetic display-name companion, so the two can never
// drift out of sync with each other (different key sets, different order) by
// construction.
//
// The name map only gets an entry when provisionRequestUserIdentity resolves
// to something FRIENDLIER than the raw key itself (source != "native") — an
// entry with no known human name is simply absent, and the spoke's own
// rendering falls back to the raw key exactly as it does today for a key with
// no map entry at all.
func authorizedUsersAndNamesForHiveID(hiveID string) ([]string, map[string]string) {
	h := loadSaaSHive(hiveID)
	if h == nil {
		return nil, nil
	}
	out := make([]string, 0, 4)
	names := make(map[string]string, 4)
	seen := map[string]bool{}
	addName := func(key string, u *SaaSUser) {
		if id, source := provisionRequestUserIdentity(key, u, ""); source != "native" && id != key {
			names[key] = id
		}
	}
	if h.Owner != "" {
		out = append(out, h.Owner+":owner")
		seen[strings.ToLower(h.Owner)] = true
		addName(h.Owner, loadSaaSUser(h.Owner))
	}
	for _, u := range listAllSaaSUsers() {
		role, ok := u.Hives[hiveID]
		if !ok || u.GitHubUsername == "" {
			continue
		}
		if seen[strings.ToLower(u.GitHubUsername)] {
			continue // owner already added
		}
		out = append(out, u.GitHubUsername+":"+role)
		seen[strings.ToLower(u.GitHubUsername)] = true
		uu := u
		addName(u.GitHubUsername, &uu)
	}
	if len(names) == 0 {
		return out, nil
	}
	return out, names
}

// HiveAccessEntry is one user's access to a hive.
type HiveAccessEntry struct {
	Username string `json:"username"`
	Role     string `json:"role"`
	// ExpiresAt is the grant's optional expiry (RFC3339 UTC, #4150) copied from
	// the user's HiveExpiry so Manage Access can show and edit it. omitempty —
	// a permanent grant renders exactly as before.
	ExpiresAt string `json:"expires_at,omitempty"`
	// Contact metadata copied from the user's record so the My Hives avatar hover
	// can show WHO someone is, not just their GitHub handle. FullName and SlackID
	// ride for every owner/admin-visible access row. Notes is admin-maintained CRM
	// scratch text (see the SaaSUser doc) and is therefore delivered ONLY to a hub
	// admin — accessForHive's includeNotes gate — so an owner never sees the
	// admin's private commentary about a co-member. All omitempty: a user with no
	// contact fields set renders exactly as before (handle — role).
	FullName string `json:"full_name,omitempty"`
	SlackID  string `json:"slack_id,omitempty"`
	Notes    string `json:"notes,omitempty"`
	// DisplayLabel is the human-facing name for this row, resolved with the
	// SAME precedence provisionRequestUserIdentity uses everywhere else
	// (linked GitHub login → recognizable GitHub login → email → DisplayName
	// → FullName → raw key) — never a second, competing resolver. Username
	// above stays the raw identity key throughout (the auth key / allowlist
	// match, completely unchanged); DisplayLabel is presentation only. Always
	// non-empty: it falls all the way back to Username, so the UI never has
	// to special-case "no name known" beyond comparing the two strings.
	DisplayLabel string `json:"display_label,omitempty"`
	// Provider is the identity provider ("github"/"google"/"ibmid"/"microsoft"/…)
	// so the row can show the right provider mark without re-deriving it from
	// Username client-side. See grantableUserProvider.
	Provider string `json:"provider,omitempty"`
	// AvatarURL is the provider-stored avatar (Google/Microsoft picture claim)
	// for a non-GitHub user; empty for a GitHub user, who keeps the derived
	// github.com/<login>.png the UI already builds from Username.
	AvatarURL string `json:"avatar_url,omitempty"`
	// Engagement stats copied from the user's record so a co-member's My-Hives
	// avatar hover can show the same logins / time-in-hive the admin Users card
	// shows. Like Notes these are stats ABOUT a person, so they ride ONLY for a
	// hub admin (accessForHive's includeAdminOnly gate) — a non-admin owner sees
	// name/Slack but not another member's engagement numbers. omitempty so a user
	// with no stats round-trips as today's handle — role tooltip.
	LoginCount     int   `json:"login_count,omitempty"`
	SessionSeconds int64 `json:"session_seconds,omitempty"`
	// Honest engagement signals (see SaaSUser for semantics) — admin-only like
	// the stats above, and omitempty/absent for records without data yet.
	EngagedSeconds int64  `json:"engaged_seconds,omitempty"`
	LastActionAt   string `json:"last_action_at,omitempty"`
	// LastActive is the RFC3339 time of this user's most recent hub activity —
	// the latest of their last login, last engaged beat, and last audited
	// action (see latestUserActivity). Unlike the engagement stats above it
	// rides for EVERY owner-visible row, not just for admins: "is this account
	// dormant?" is exactly the owner-level question the Manage Access list
	// answers when deciding on access changes (#4146). omitempty — absent means
	// the user has never been active, which the UI renders as "—". It also
	// feeds the Manage Access CSV export's last-active column (#4152).
	LastActive string `json:"last_active,omitempty"`
}

// latestUserActivity returns the RFC3339 timestamp of u's most recent hub
// activity — the latest of LastLogin, LastEngagedAt, and LastActionAt — or ""
// when the user has never been active. Timestamps are parsed rather than
// compared lexically so legacy records with mixed UTC offsets still order
// correctly; an unparseable value is skipped, never surfaced.
func latestUserActivity(u *SaaSUser) string {
	var best time.Time
	var bestStr string
	for _, ts := range []string{u.LastLogin, u.LastEngagedAt, u.LastActionAt} {
		if strings.TrimSpace(ts) == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			continue
		}
		if t.After(best) {
			best = t
			bestStr = ts
		}
	}
	return bestStr
}

// accessForHive returns who can sign in to a hive, newest-role-agnostic and
// sorted for a stable render. Access is derived by scanning user records rather
// than reading h.Owner: a user's Hives map is the authoritative grant (see
// handleApproveProvision / handleAssignHive, which write it), and an owner who
// is missing from it genuinely cannot sign in.
// includeAdminOnly controls whether the admin-only fields — the CRM Notes and
// the engagement stats (LoginCount/SessionSeconds) — are copied onto each entry:
// pass true ONLY for a hub admin. FullName/SlackID always ride (they identify the
// person to a co-owner); Notes and the stats are private to admins.
func accessForHive(hiveID string, users []SaaSUser, includeAdminOnly bool) []HiveAccessEntry {
	access := make([]HiveAccessEntry, 0)
	for _, u := range users {
		if role, ok := u.Hives[hiveID]; ok {
			uu := u
			label, _ := provisionRequestUserIdentity(u.GitHubUsername, &uu, "")
			entry := HiveAccessEntry{
				Username:     u.GitHubUsername,
				Role:         role,
				ExpiresAt:    u.HiveExpiry[hiveID],
				FullName:     u.FullName,
				SlackID:      u.SlackID,
				DisplayLabel: label,
				Provider:     grantableUserProvider(&uu),
				AvatarURL:    u.AvatarURL,
				// Coarse last-active rides for every viewer of the row (see the
				// field doc) — only the granular stats below stay admin-only.
				LastActive: latestUserActivity(&u),
			}
			if includeAdminOnly {
				entry.Notes = u.Notes
				entry.LoginCount = u.LoginCount
				entry.SessionSeconds = u.SessionSeconds
				entry.EngagedSeconds = u.EngagedSeconds
				entry.LastActionAt = u.LastActionAt
			}
			access = append(access, entry)
		}
	}
	sort.Slice(access, func(i, j int) bool {
		// Owners first, then alphabetical — the owner is the useful line to read
		// first when scanning a hover with several users on it.
		if (access[i].Role == "owner") != (access[j].Role == "owner") {
			return access[i].Role == "owner"
		}
		return access[i].Username < access[j].Username
	})
	return access
}

// userIsHiveOwner reports whether username may administer hive h: the registry
// creator, any user holding the granted owner role on that hive, or the hub admin.
func userIsHiveOwner(username string, h *SaaSHive) bool {
	if username == "" || h == nil {
		return false
	}
	if isHubAdmin(username) {
		return true
	}
	if canonicalEqual(h.Owner, username) {
		return true
	}
	u := loadSaaSUser(username)
	return u != nil && u.Hives != nil && u.Hives[h.ID] == "owner"
}

// userOwnsHive reports whether username is the TRUE (canonical) owner of the
// hive identified by hiveID, resolving through the live registry first and
// the SaaS meta record second. Unlike userIsHiveOwner it never consults
// stored per-user roles or hub-admin status, so it is safe to use for
// owner-only role elevation without widening admin behavior (#4081).
func (s *HubServer) userOwnsHive(username, hiveID string) bool {
	if username == "" || hiveID == "" {
		return false
	}
	var regOwner string
	s.mu.Lock()
	for _, h := range s.registry.Hives {
		if h.ID == hiveID {
			regOwner = h.Owner
			break
		}
	}
	s.mu.Unlock()
	if regOwner != "" && canonicalEqual(regOwner, username) {
		return true
	}
	if sh := loadSaaSHive(hiveID); sh != nil && canonicalEqual(sh.Owner, username) {
		return true
	}
	return false
}

func (s *HubServer) handleAccessList(w http.ResponseWriter, r *http.Request) {
	hiveID := r.PathValue("id")
	username := s.getAuthUser(r)
	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	if !userIsHiveOwner(username, h) {
		http.Error(w, `{"error":"only the owner can view access"}`, http.StatusForbidden)
		return
	}
	// Notes is admin-only; a non-admin owner viewing their hive's access gets
	// name+Slack but not the admin's private CRM notes.
	access := accessForHive(hiveID, listAllSaaSUsers(), isHubAdmin(username))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"access": access})
}

// handleGrantableUsers lists the usernames a hive owner may grant access to.
// The Manage Access dropdown used to read /api/saas/admin/users, which is
// requireAdmin — so for every owner who is not the hub admin it 403'd and the
// dropdown silently rendered empty, making it look as though known users simply
// "weren't there". Owners legitimately need the roster to grant access, so this
// exposes exactly that and nothing else: usernames only, no emails, quotas, or
// hive assignments (which would leak the shape of other people's fleets).
func (s *HubServer) handleGrantableUsers(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	// Any user who owns at least one hive may see the roster; that is the same
	// bar as being able to open Manage Access at all. Admin always qualifies.
	owns := isHubAdmin(username)
	if !owns {
		for _, h := range listSaaSHives() {
			h := h
			if userIsHiveOwner(username, &h) {
				owns = true
				break
			}
		}
	}
	if !owns {
		http.Error(w, `{"error":"only hive owners can list users"}`, http.StatusForbidden)
		return
	}
	names := make([]string, 0)
	entries := make([]grantableUserEntry, 0)
	for _, u := range listAllSaaSUsers() {
		if u.GitHubUsername == "" {
			continue
		}
		u := u
		names = append(names, u.GitHubUsername)
		entries = append(entries, grantableUserEntry{
			ID:       u.GitHubUsername,
			Label:    grantableUserLabel(&u),
			Provider: grantableUserProvider(&u),
		})
	}
	sort.Strings(names)
	// Sort by the label an owner actually reads in the picker, falling back to
	// the stable ID so two identical display names still order deterministically.
	sort.Slice(entries, func(i, j int) bool {
		li, lj := strings.ToLower(entries[i].Label), strings.ToLower(entries[j].Label)
		if li != lj {
			return li < lj
		}
		return entries[i].ID < entries[j].ID
	})
	w.Header().Set("Content-Type", "application/json")
	// "users" (bare stable IDs) is kept for back-compat with older dashboards;
	// "entries" adds the normalized display label alongside the same stable ID
	// so the picker can show a human name while still granting by identity key.
	_ = json.NewEncoder(w).Encode(map[string]any{"users": names, "entries": entries})
}

// grantableUserEntry is one row of the Manage Access "Add User" picker: the
// stable identity key used for permission grants plus the friendly label an
// owner should see instead of a raw provider ID.
type grantableUserEntry struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Provider string `json:"provider,omitempty"`
}

// identityProviderPrefixRE matches wire-form provider-prefixed identity keys
// like "google:107812...", "ibmid:6500...", "microsoft:AAAA...". A plain
// GitHub login can never match: GitHub logins cannot contain ":".
var identityProviderPrefixRE = regexp.MustCompile(`^([a-z][a-z0-9_-]*):(.+)$`)

// splitIdentityKey breaks a provider-prefixed identity key ("google:1078")
// into its provider and raw subject. A plain login returns ("", key).
func splitIdentityKey(key string) (provider, sub string) {
	if m := identityProviderPrefixRE.FindStringSubmatch(key); m != nil {
		return m[1], m[2]
	}
	return "", key
}

// normalizeIdentityProvider maps provider aliases onto their canonical name
// so the picker classifies "ms:…" identities the same as "microsoft:…" ones.
func normalizeIdentityProvider(p string) string {
	if p == "ms" {
		return "microsoft"
	}
	return p
}

// grantableUserProvider reports the identity provider for a roster entry,
// preferring the stored Provider field and falling back to the prefix of the
// identity key so legacy records still classify correctly.
func grantableUserProvider(u *SaaSUser) string {
	if u.Provider != "" {
		return normalizeIdentityProvider(u.Provider)
	}
	if p, _ := splitIdentityKey(u.GitHubUsername); p != "" {
		return normalizeIdentityProvider(p)
	}
	return "github"
}

// maxOpaqueIDLabelLen bounds how much of a raw opaque subject the fallback
// label shows before truncating — enough to disambiguate, short enough that a
// token-like Microsoft sub doesn't blow out the dropdown.
const maxOpaqueIDLabelLen = 12

// grantableUserLabel derives the friendly display label for a user in the
// Manage Access picker. It NEVER changes the identity key used for grants —
// display only. Preference order:
//  1. provider-asserted DisplayName from the OIDC name claim
//  2. a plain GitHub login (already human-recognizable)
//  3. the provider email claim (display only, never the key)
//  4. an optional linked GitHub login
//  5. a truncated "provider: subject…" rendering of the raw key, so even a
//     record with no human-readable claims stays scannable instead of a wall
//     of token characters.
func grantableUserLabel(u *SaaSUser) string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	provider, sub := splitIdentityKey(u.GitHubUsername)
	if provider == "" || provider == "github" {
		return sub
	}
	if u.Email != "" {
		return u.Email
	}
	if u.LinkedGitHubLogin != "" {
		return u.LinkedGitHubLogin
	}
	if len(sub) > maxOpaqueIDLabelLen {
		sub = sub[:maxOpaqueIDLabelLen] + "…"
	}
	return provider + ": " + sub
}

func (s *HubServer) handleAccessAdd(w http.ResponseWriter, r *http.Request) {
	hiveID := r.PathValue("id")
	username := s.getAuthUser(r)
	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	if !userIsHiveOwner(username, h) {
		http.Error(w, `{"error":"only the owner can manage access"}`, http.StatusForbidden)
		return
	}
	var body struct {
		Username string `json:"username"`
		Role     string `json:"role"`
		// ExpiresAt is the grant's optional expiry (#4150). Pointer semantics:
		//   absent (nil)  → preserve the target's existing expiry, so a plain
		//                   role change never silently clears a time limit
		//   ""            → clear the expiry (grant becomes permanent)
		//   "YYYY-MM-DD"  → valid through that day, UTC
		//   RFC3339       → exact instant
		ExpiresAt *string `json:"expires_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Username == "" || body.Role == "" {
		http.Error(w, `{"error":"username and role required"}`, http.StatusBadRequest)
		return
	}
	if !config.ValidRole(body.Role) {
		http.Error(w, `{"error":"role must be read, read-write, merger, or owner"}`, http.StatusBadRequest)
		return
	}
	var expiresAt string
	if body.ExpiresAt != nil && strings.TrimSpace(*body.ExpiresAt) != "" {
		t, err := parseAccessExpiry(*body.ExpiresAt)
		if err != nil {
			http.Error(w, `{"error":"expiry must be a YYYY-MM-DD date or RFC3339 timestamp"}`, http.StatusBadRequest)
			return
		}
		if !t.After(time.Now()) {
			http.Error(w, `{"error":"expiry must be in the future"}`, http.StatusBadRequest)
			return
		}
		expiresAt = t.Format(time.RFC3339)
	}
	// An expiring owner grant on the hive's ONLY owner would auto-revoke into
	// an ownerless hive — the same orphaning handleAccessRemove refuses. Block
	// it here rather than special-casing the sweep, so the owner learns at set
	// time instead of the hive silently degrading later.
	if expiresAt != "" && body.Role == "owner" {
		ownerCount := 0
		for _, u := range listAllSaaSUsers() {
			if u.Hives[hiveID] == "owner" && !canonicalEqual(u.GitHubUsername, body.Username) {
				ownerCount++
			}
		}
		if ownerCount == 0 {
			http.Error(w, `{"error":"cannot set an expiry on the only owner"}`, http.StatusBadRequest)
			return
		}
	}
	target := ensureSaaSUser(body.Username)
	// Distinguish a fresh grant from a role change so the audit trail answers
	// "who changed X's role, from what, and when" (#4148) — a bare "granted as
	// merger" entry hides the fact the user was previously an owner.
	prevRole := target.Hives[hiveID]
	prevExpiry := target.HiveExpiry[hiveID]
	target.Hives[hiveID] = body.Role
	switch {
	case body.ExpiresAt == nil:
		// Preserve any existing expiry: a role change is not an extension.
	case expiresAt == "":
		delete(target.HiveExpiry, hiveID)
	default:
		if target.HiveExpiry == nil {
			target.HiveExpiry = map[string]string{}
		}
		target.HiveExpiry[hiveID] = expiresAt
	}
	if err := saveSaaSUser(target); err != nil {
		s.logger.Error("audit: access grant save failed", "hive", hiveID, "target", body.Username, "error", err)
		http.Error(w, `{"error":"failed to save access grant"}`, http.StatusInternalServerError)
		return
	}
	// The stored (possibly preserved) expiry after the update, for the audit
	// trail; "" means permanent.
	newExpiry := target.HiveExpiry[hiveID]
	auditExpiry := newExpiry
	if auditExpiry == "" {
		auditExpiry = "never"
	}
	expiryNote := ""
	if newExpiry != "" {
		expiryNote = " (expires " + newExpiry + ")"
	}
	switch {
	case prevRole == "":
		s.logger.Info("audit: access granted", "hive", hiveID, "target", body.Username, "role", body.Role, "expires", auditExpiry, "by", username)
		s.recordTimeline(hiveID, TimelineAccess,
			fmt.Sprintf("access granted to %s as %s%s", body.Username, body.Role, expiryNote), username)
	case prevRole != body.Role:
		s.logger.Info("audit: role changed", "hive", hiveID, "target", body.Username, "from", prevRole, "to", body.Role, "expires", auditExpiry, "by", username)
		s.recordTimeline(hiveID, TimelineAccess,
			fmt.Sprintf("role for %s changed: %s → %s%s", body.Username, prevRole, body.Role, expiryNote), username)
	case newExpiry != prevExpiry:
		// Expiry-only change (extend, shorten, or clear): still a permission
		// change, so it belongs in the append-only log like any other.
		detail := fmt.Sprintf("access expiry for %s cleared (now permanent)", body.Username)
		if newExpiry != "" {
			detail = fmt.Sprintf("access for %s now expires %s", body.Username, newExpiry)
		}
		s.logger.Info("audit: access expiry changed", "hive", hiveID, "target", body.Username, "expires", auditExpiry, "by", username)
		s.recordTimeline(hiveID, TimelineAccess, detail, username)
	default:
		// Same role re-granted: a no-op — do not pollute the append-only log.
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "granted"})
}

func (s *HubServer) handleAccessRemove(w http.ResponseWriter, r *http.Request) {
	hiveID := r.PathValue("id")
	targetUsername := r.PathValue("username")
	username := s.getAuthUser(r)
	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	if !userIsHiveOwner(username, h) {
		http.Error(w, `{"error":"only the owner can manage access"}`, http.StatusForbidden)
		return
	}
	target := loadSaaSUser(targetUsername)
	if target == nil {
		writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}
	if target.Hives[hiveID] == "owner" {
		ownerCount := 0
		for _, u := range listAllSaaSUsers() {
			if u.Hives[hiveID] == "owner" {
				ownerCount++
			}
		}
		if ownerCount <= 1 {
			http.Error(w, `{"error":"at least one owner is required — cannot remove the last owner"}`, http.StatusBadRequest)
			return
		}
	}
	delete(target.Hives, hiveID)
	delete(target.HiveExpiry, hiveID)
	if err := saveSaaSUser(target); err != nil {
		s.logger.Error("audit: access revoke save failed", "hive", hiveID, "target", targetUsername, "error", err)
		http.Error(w, `{"error":"failed to save access revocation"}`, http.StatusInternalServerError)
		return
	}
	s.logger.Info("audit: access revoked", "hive", hiveID, "target", targetUsername, "by", username)
	s.recordTimeline(hiveID, TimelineAccess,
		fmt.Sprintf("access revoked from %s", targetUsername), username)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "revoked"})
}

type AccessRequest struct {
	Username    string `json:"username"`
	RequestedAt string `json:"requested_at"`
	Status      string `json:"status"`
	// Note is the free-text justification the requester must supply
	// explaining why they should be granted access. Shown to the
	// owner/approver when they review the request. May be empty on
	// legacy records created before this field existed.
	Note string `json:"note,omitempty"`
}

func loadAccessRequests(hiveID string) []AccessRequest {
	if strings.Contains(hiveID, "..") || strings.Contains(hiveID, "/") || strings.Contains(hiveID, "\\") {
		return nil
	}
	path := filepath.Join(saasHivesDir, hiveID, "requests.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var reqs []AccessRequest
	if err := json.Unmarshal(data, &reqs); err != nil {
		return nil
	}
	return reqs
}

func (s *HubServer) decoratePendingAccessRequests(reqs []PendingAccessRequest) []PendingAccessRequest {
	for i := range reqs {
		username := strings.TrimSpace(reqs[i].Username)
		if username == "" {
			continue
		}
		label, avatar := s.displayIdentity(username)
		reqs[i].DisplayLabel = label
		reqs[i].AvatarURL = avatar
		if u := loadSaaSUser(username); u != nil {
			reqs[i].Provider = grantableUserProvider(u)
			continue
		}
		if provider, _ := splitIdentityKey(username); provider != "" {
			reqs[i].Provider = normalizeIdentityProvider(provider)
		} else {
			reqs[i].Provider = legacyProvider
		}
	}
	return reqs
}

func saveAccessRequests(hiveID string, reqs []AccessRequest) {
	if strings.Contains(hiveID, "..") || strings.Contains(hiveID, "/") || strings.Contains(hiveID, "\\") {
		slog.Warn("saveAccessRequests: invalid hiveID", "hiveID", hiveID)
		return
	}
	dir := filepath.Join(saasHivesDir, hiveID)
	// Best-effort: a failed mkdir surfaces via the WriteFile error below.
	_ = os.MkdirAll(dir, 0o755)
	data, err := json.MarshalIndent(reqs, "", "  ")
	if err != nil {
		slog.Warn("saveAccessRequests: marshal failed", "hiveID", hiveID, "error", err)
		return
	}
	path := filepath.Join(dir, "requests.json")
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		slog.Warn("saveAccessRequests: write failed", "error", err)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		slog.Warn("saveAccessRequests: rename failed", "error", err)
	}
}

// maxAccessRequestNoteLen bounds the requester's justification note so a
// single request cannot bloat the stored requests.json.
const maxAccessRequestNoteLen = 2000

func (s *HubServer) handleRequestAccess(w http.ResponseWriter, r *http.Request) {
	hiveID := r.PathValue("id")
	username := s.getAuthUser(r)

	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}

	// The requester must supply a justification note explaining why they
	// need access; it is shown to the owner/approver on review.
	var body struct {
		Note string `json:"note"`
	}
	// Body is optional to decode (missing/invalid JSON leaves Note empty,
	// which the validation below rejects with a clear message).
	_ = json.NewDecoder(r.Body).Decode(&body)
	note := strings.TrimSpace(body.Note)
	if note == "" {
		http.Error(w, `{"error":"a note explaining why you need access is required"}`, http.StatusBadRequest)
		return
	}
	if len(note) > maxAccessRequestNoteLen {
		note = note[:maxAccessRequestNoteLen]
	}

	user := loadSaaSUser(username)
	if user != nil {
		if _, ok := user.Hives[hiveID]; ok {
			http.Error(w, `{"error":"you already have access"}`, http.StatusBadRequest)
			return
		}
	}

	reqs := loadAccessRequests(hiveID)
	for _, req := range reqs {
		if req.Username == username && req.Status == "pending" {
			http.Error(w, `{"error":"request already pending"}`, http.StatusBadRequest)
			return
		}
	}

	reqs = append(reqs, AccessRequest{
		Username:    username,
		RequestedAt: time.Now().UTC().Format(time.RFC3339),
		Status:      "pending",
		Note:        note,
	})
	saveAccessRequests(hiveID, reqs)

	// Push-notify the owner (Slack DM where configured — access_notify.go).
	// Placed after the pending-duplicate rejection above, so a request that was
	// already pending can never fire a second notification.
	s.notifyOwnerAccessRequest(hiveID, h.Owner, username, note)

	s.logger.Info("audit: access requested", "hive", hiveID, "by", username)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "requested"})
}

func (s *HubServer) handleGetRequests(w http.ResponseWriter, r *http.Request) {
	hiveID := r.PathValue("id")
	username := s.getAuthUser(r)

	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}

	user := loadSaaSUser(username)
	if user == nil {
		http.Error(w, `{"error":"not authorized"}`, http.StatusForbidden)
		return
	}
	role := user.Hives[hiveID]
	if !config.RoleAtLeast(role, config.RoleReadWrite) && !isHubAdmin(username) {
		http.Error(w, `{"error":"need owner or read-write access"}`, http.StatusForbidden)
		return
	}

	reqs := loadAccessRequests(hiveID)
	pending := make([]PendingAccessRequest, 0)
	for _, req := range reqs {
		if req.Status == "pending" {
			pending = append(pending, PendingAccessRequest{
				Username:    req.Username,
				RequestedAt: req.RequestedAt,
				Note:        req.Note,
			})
		}
	}
	pending = s.decoratePendingAccessRequests(pending)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"requests": pending})
}

func (s *HubServer) handleApproveRequest(w http.ResponseWriter, r *http.Request) {
	hiveID := r.PathValue("id")
	targetUsername := r.PathValue("username")
	approver := s.getAuthUser(r)

	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}

	approverUser := loadSaaSUser(approver)
	if approverUser == nil {
		http.Error(w, `{"error":"not authorized"}`, http.StatusForbidden)
		return
	}
	approverRole := approverUser.Hives[hiveID]
	if !config.RoleAtLeast(approverRole, config.RoleReadWrite) && !isHubAdmin(approver) {
		http.Error(w, `{"error":"need owner or read-write access"}`, http.StatusForbidden)
		return
	}

	var body struct {
		Role string `json:"role"`
	}
	// Body is optional to decode (missing/invalid JSON leaves Role empty,
	// defaulted to "read" below).
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Role == "" {
		body.Role = "read"
	}

	if !config.ValidRole(body.Role) {
		http.Error(w, `{"error":"role must be read, read-write, merger, or owner"}`, http.StatusBadRequest)
		return
	}
	if !isHubAdmin(approver) && config.RoleAtLeast(body.Role, approverRole) {
		http.Error(w, `{"error":"cannot grant a role equal to or higher than your own"}`, http.StatusForbidden)
		return
	}

	target := ensureSaaSUser(targetUsername)
	target.Hives[hiveID] = body.Role
	if err := saveSaaSUser(target); err != nil {
		s.logger.Error("audit: access request approval save failed", "hive", hiveID, "target", targetUsername, "error", err)
		http.Error(w, `{"error":"failed to save access grant"}`, http.StatusInternalServerError)
		return
	}

	reqs := loadAccessRequests(hiveID)
	for i := range reqs {
		if reqs[i].Username == targetUsername && reqs[i].Status == "pending" {
			reqs[i].Status = "approved"
		}
	}
	saveAccessRequests(hiveID, reqs)

	s.logger.Info("audit: access request approved", "hive", hiveID, "target", targetUsername, "role", body.Role, "by", approver)
	s.recordTimeline(hiveID, TimelineAccess,
		fmt.Sprintf("access request from %s approved as %s", targetUsername, body.Role), approver)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "approved"})
}

func (s *HubServer) handleDenyRequest(w http.ResponseWriter, r *http.Request) {
	hiveID := r.PathValue("id")
	targetUsername := r.PathValue("username")
	denier := s.getAuthUser(r)

	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}

	denierUser := loadSaaSUser(denier)
	if denierUser == nil {
		http.Error(w, `{"error":"not authorized"}`, http.StatusForbidden)
		return
	}
	denierRole := denierUser.Hives[hiveID]
	if !config.RoleAtLeast(denierRole, config.RoleReadWrite) && !isHubAdmin(denier) {
		http.Error(w, `{"error":"need owner or read-write access"}`, http.StatusForbidden)
		return
	}

	reqs := loadAccessRequests(hiveID)
	for i := range reqs {
		if reqs[i].Username == targetUsername && reqs[i].Status == "pending" {
			reqs[i].Status = "denied"
		}
	}
	saveAccessRequests(hiveID, reqs)

	s.logger.Info("audit: access request denied", "hive", hiveID, "target", targetUsername, "by", denier)
	s.recordTimeline(hiveID, TimelineAccess,
		fmt.Sprintf("access request from %s denied", targetUsername), denier)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "denied"})
}

func (s *HubServer) handleApproveAccess(w http.ResponseWriter, r *http.Request) {
	hiveID := r.PathValue("id")
	targetUsername := r.PathValue("username")
	approver := s.getAuthUser(r)

	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}

	approverUser := loadSaaSUser(approver)
	if approverUser == nil {
		http.Error(w, `{"error":"not authorized"}`, http.StatusForbidden)
		return
	}
	approverRole := approverUser.Hives[hiveID]
	if approverRole != "owner" && !isHubAdmin(approver) {
		http.Error(w, `{"error":"only the owner can approve access"}`, http.StatusForbidden)
		return
	}

	reqs := loadAccessRequests(hiveID)
	found := false
	for i := range reqs {
		if reqs[i].Username == targetUsername && reqs[i].Status == "pending" {
			reqs[i].Status = "approved"
			found = true
		}
	}
	if !found {
		http.Error(w, `{"error":"no pending request for this user"}`, http.StatusNotFound)
		return
	}
	saveAccessRequests(hiveID, reqs)

	const defaultApproveRole = "read"
	target := ensureSaaSUser(targetUsername)
	// Never demote the hive's TRUE owner (or an already-granted owner) to the
	// default role — this unconditional overwrite is how owners lost their
	// stored role and, with it, every owner-gated affordance (#4081).
	if target.Hives[hiveID] != "owner" && !canonicalEqual(h.Owner, targetUsername) {
		target.Hives[hiveID] = defaultApproveRole
	}
	if err := saveSaaSUser(target); err != nil {
		s.logger.Error("audit: access approve-via-PUT save failed", "hive", hiveID, "target", targetUsername, "error", err)
		http.Error(w, `{"error":"failed to save access grant"}`, http.StatusInternalServerError)
		return
	}

	s.logger.Info("audit: access approved via PUT", "hive", hiveID, "target", targetUsername, "by", approver)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (s *HubServer) handleDenyAccess(w http.ResponseWriter, r *http.Request) {
	hiveID := r.PathValue("id")
	targetUsername := r.PathValue("username")
	denier := s.getAuthUser(r)

	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}

	denierUser := loadSaaSUser(denier)
	if denierUser == nil {
		http.Error(w, `{"error":"not authorized"}`, http.StatusForbidden)
		return
	}
	denierRole := denierUser.Hives[hiveID]
	if denierRole != "owner" && !isHubAdmin(denier) {
		http.Error(w, `{"error":"only the owner can deny access"}`, http.StatusForbidden)
		return
	}

	reqs := loadAccessRequests(hiveID)
	for i := range reqs {
		if reqs[i].Username == targetUsername && reqs[i].Status == "pending" {
			reqs[i].Status = "denied"
		}
	}
	saveAccessRequests(hiveID, reqs)

	s.logger.Info("audit: access denied via DELETE", "hive", hiveID, "target", targetUsername, "by", denier)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

var provisionRequestsDir = "/data/saas/provision-requests"

const (
	provisionStatusPending  = "pending"
	provisionStatusApproved = "approved"
	provisionStatusDenied   = "denied"
)

const maxProvisionRequestBodyBytes = 4 * 1024

type ProvisionRequest struct {
	Username string `json:"username"`
	// UserID is the human-facing identifier admins should use when reviewing
	// the request. Username remains the stable auth key/native provider subject
	// (for example "ibmid:695000VVZ9"); UserID captures the meaningful login or
	// identity available at request time so the review queue does not headline
	// opaque SSO subjects. Empty on older records; enrichProvisionRequests fills
	// it from the user record when possible, and the UI falls back to Username.
	UserID       string `json:"user_id,omitempty"`
	UserIDSource string `json:"user_id_source,omitempty"`
	// GitHubHost is the GitHub instance the org lives on — empty means public
	// github.com, otherwise a GitHub Enterprise host (github.ibm.com,
	// github.cisco.com, …). Captured so an admin can see which instance a
	// request targets before deciding where to place the hive.
	GitHubHost  string `json:"github_host,omitempty"`
	Org         string `json:"org"`
	Repos       string `json:"repos"`
	PrimaryRepo string `json:"primary_repo"`
	ACMMLevel   int    `json:"acmm_level"`
	AuthMethod  string `json:"auth_method"`
	RequestedAt string `json:"requested_at"`
	Status      string `json:"status"`

	// Who is asking, as a person rather than as a GitHub login. Both reuse the
	// SaaSUser contact fields of the same name and the same caps
	// (maxContactNameLen / maxContactSlackIDLen) — deliberately NOT new keys.
	// A second Slack key would split one fact across two names, with the
	// request form writing one and the admin users panel reading the other.
	//
	// These are captured here and copied onto the SaaSUser record on approval
	// (handleApproveProvision); asking and then dropping the answer would be
	// worse than not asking. Both are omitempty so requests filed before these
	// fields existed round-trip unchanged.
	FullName string `json:"full_name,omitempty"`
	SlackID  string `json:"slack_id,omitempty"`

	// Country is the requester's OPTIONAL self-declared ISO 3166-1 alpha-2
	// code, picked from the wizard's dropdown. Like the two fields above it
	// reuses the SaaSUser key of the same name and is copied onto the user
	// record on approval (applyRequestContactToUser) — asking and then dropping
	// the answer would be worse than not asking.
	//
	// This is the AUTHORITATIVE source of a user's country: they chose it about
	// themselves. The Accept-Language inference on the login path is only a
	// fallback for records that never got one. omitempty so requests filed
	// before this field existed round-trip unchanged.
	Country string `json:"country,omitempty"`

	// Decision audit. Previously a request only carried its final Status, so
	// once it left "pending" there was no record of WHO decided, WHEN, or —
	// for an approval — which hive the requester actually got. That made the
	// history unauditable: an approved request and a denied one looked equally
	// anonymous. Empty on records decided before these fields existed.
	DecidedBy string `json:"decided_by,omitempty"`
	// DecidedByName is the display-only label for DecidedBy, resolved on read.
	DecidedByName string `json:"decided_by_name,omitempty"`
	DecidedAt     string `json:"decided_at,omitempty"`
	AssignedHive  string `json:"assigned_hive,omitempty"`
	// DenyReason is the optional free-text explanation shown back to the
	// requester when a request is turned down.
	DenyReason string `json:"deny_reason,omitempty"`

	// --- Derived, never persisted ---
	// These are filled in on read (see enrichProvisionRequests) so the admin
	// Past Requests table can show the requester's role on the hive they were
	// given, plus the rest of their fleet, without one API call per row. They
	// are omitempty so records written before they existed stay valid and so a
	// round-trip through saveProvisionRequest never bakes stale roles onto disk.
	//
	// AssignedRole is the requester's role on AssignedHive, read from their
	// SaaSUser.Hives map — the authoritative grant (see accessForHive). Empty
	// when the request was denied, when no hive was assigned, or when the grant
	// was since revoked.
	AssignedRole string `json:"assigned_role,omitempty"`
	// OtherHives is every OTHER hive the requester can sign in to, with their
	// role on each — the person's footprint beyond this one request. Sorted
	// owners-first then by hive ID for a stable render.
	OtherHives []UserHiveRole `json:"other_hives,omitempty"`
}

// roleOwner is the role string stored in SaaSUser.Hives for the hive's owner.
// Named so the owners-first sort below does not repeat a bare literal.
const roleOwner = "owner"

// UserHiveRole is one hive a user can sign in to and the role they hold on it.
// The mirror image of HiveAccessEntry: that answers "who is on this hive", this
// answers "which hives is this user on".
type UserHiveRole struct {
	HiveID string `json:"hive_id"`
	Role   string `json:"role"`
}

// hivesForUser returns every hive the named user can sign in to, with their
// role on each, optionally excluding one hive ID (the one already shown in its
// own column). Access comes from SaaSUser.Hives — the authoritative grant that
// handleApproveProvision / handleAssignHive write — not from hive.Owner, which
// can name someone whose grant was revoked.
//
// users is passed in rather than read here so a caller enriching many rows can
// read the roster ONCE: listAllSaaSUsers hits the filesystem per user record.
func hivesForUser(username string, excludeHiveID string, users []SaaSUser) []UserHiveRole {
	if username == "" {
		return nil
	}
	out := make([]UserHiveRole, 0)
	for _, u := range users {
		if !strings.EqualFold(u.GitHubUsername, username) {
			continue
		}
		for id, role := range u.Hives {
			if id == "" || id == excludeHiveID {
				continue
			}
			out = append(out, UserHiveRole{HiveID: id, Role: role})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		// Owned hives first — the useful line to read when scanning someone's
		// footprint — then alphabetical by hive ID for a stable order.
		if (out[i].Role == roleOwner) != (out[j].Role == roleOwner) {
			return out[i].Role == roleOwner
		}
		return out[i].HiveID < out[j].HiveID
	})
	if len(out) == 0 {
		return nil // omitempty: no cell rather than an empty array
	}
	return out
}

// roleForUserOnHive returns the named user's role on the named hive, or "" if
// they hold no grant on it. Same roster-passed-in contract as hivesForUser.
func roleForUserOnHive(username, hiveID string, users []SaaSUser) string {
	if username == "" || hiveID == "" {
		return ""
	}
	for _, u := range users {
		if !strings.EqualFold(u.GitHubUsername, username) {
			continue
		}
		if role, ok := u.Hives[hiveID]; ok {
			return role
		}
	}
	return ""
}

// provisionRequestUserIdentity chooses the operator-facing identifier for a
// request, plus its source so the UI only links identifiers known to be GitHub
// logins.
// The raw request Username is the auth key and can be an opaque provider subject
// ("ibmid:…"). Prefer a linked GitHub/GHE login when the user has one, then a
// recognizable GitHub login, then email/name claims, and fall back to the auth
// key only when no friendlier identity exists.
func provisionRequestUserIdentity(username string, u *SaaSUser, requestFullName string) (id, source string) {
	if u != nil {
		if id := strings.TrimSpace(u.LinkedGitHubLogin); id != "" {
			return id, "github"
		}
		if provider, subject := splitIdentityKey(strings.TrimSpace(u.GitHubUsername)); provider == "" || normalizeIdentityProvider(provider) == legacyProvider {
			if subject != "" {
				return subject, "github"
			}
		}
		if id := strings.TrimSpace(u.Email); id != "" {
			return id, "email"
		}
		if id := strings.TrimSpace(u.DisplayName); id != "" {
			return id, "name"
		}
		if id := strings.TrimSpace(u.FullName); id != "" {
			return id, "name"
		}
	}
	if id := strings.TrimSpace(requestFullName); id != "" {
		return id, "name"
	}
	if provider, subject := splitIdentityKey(strings.TrimSpace(username)); normalizeIdentityProvider(provider) == legacyProvider && subject != "" {
		return subject, "github"
	}
	return strings.TrimSpace(username), "native"
}

func provisionRequestUserID(username string, u *SaaSUser, requestFullName string) string {
	id, _ := provisionRequestUserIdentity(username, u, requestFullName)
	return id
}

func provisionRequestUserFromRoster(username string, users []SaaSUser) *SaaSUser {
	for i := range users {
		if strings.EqualFold(users[i].GitHubUsername, username) {
			return &users[i]
		}
	}
	return nil
}

// enrichProvisionRequests fills in the derived AssignedRole / OtherHives fields
// on every request in place.
//
// ADMIN-ONLY: this exposes other people's hive memberships, so it must only be
// called on a payload already gated behind requireAdmin (or the isAdmin branch
// of the dashboard handler). Do not call it on a per-user response.
//
// The roster is read once here — O(1) filesystem sweeps for the whole table
// rather than O(rows) — because listAllSaaSUsers walks and unmarshals every
// user record on disk.
func enrichProvisionRequests(requests []ProvisionRequest) []ProvisionRequest {
	if len(requests) == 0 {
		return requests
	}
	users := listAllSaaSUsers()
	label := (&HubServer{}).identityLabeler()
	for i := range requests {
		if requests[i].UserID == "" {
			requests[i].UserID, requests[i].UserIDSource = provisionRequestUserIdentity(requests[i].Username, provisionRequestUserFromRoster(requests[i].Username, users), requests[i].FullName)
		} else if requests[i].UserIDSource == "" {
			if requests[i].UserID == requests[i].Username {
				requests[i].UserIDSource = "native"
			}
		}
		if l := label(requests[i].DecidedBy); l != requests[i].DecidedBy {
			requests[i].DecidedByName = l
		}
		requests[i].AssignedRole = roleForUserOnHive(requests[i].Username, requests[i].AssignedHive, users)
		requests[i].OtherHives = hivesForUser(requests[i].Username, requests[i].AssignedHive, users)
	}
	return requests
}

// applyRequestContactToUser carries the requester's contact details from an
// approved provision request onto their SaaSUser record.
//
// This is what makes asking for them worth anything. The admin users panel and
// the Slack sender both read SaaSUser, NOT ProvisionRequest — a value that
// stops at the request file is invisible to every consumer of it. Slack
// messaging shipped with no user having a slack_id, settable only by hand one
// user at a time; approval is the natural point of capture.
//
// It only ever FILLS A BLANK. An admin who has already curated these fields (or
// a user who corrected them later) outranks whatever was typed into a request
// form, and a re-approval must not silently revert that.
//
// Nil-safe on both sides: it runs on the approval path beside other work that
// can legitimately leave either side absent, and must not panic there.
func applyRequestContactToUser(user *SaaSUser, pr *ProvisionRequest) {
	if user == nil || pr == nil {
		return
	}
	if user.FullName == "" && pr.FullName != "" {
		user.FullName = truncateRunes(strings.TrimSpace(pr.FullName), maxContactNameLen)
	}
	if user.SlackID == "" && pr.SlackID != "" {
		user.SlackID = truncateRunes(strings.TrimSpace(pr.SlackID), maxContactSlackIDLen)
	}
	// The explicit pick outranks anything Accept-Language inferred at login, so
	// unlike the two fields above this one overwrites a value already on the
	// record — but only when the request actually carries a choice. Re-normalize
	// rather than trusting the stored request: it may predate the validation.
	if code := normalizeCountryCode(pr.Country); code != "" {
		// The wizard pick is a deliberate statement BY THE USER, so it carries
		// the same provenance the self-service endpoint stamps. Without this,
		// an approval would leave the record looking "inferred", and the
		// priority rule would hold only by the accident of the value being
		// non-empty.
		setUserCountry(user, code, countrySourceUser)
	}
}

func loadProvisionRequest(username string) *ProvisionRequest {
	if strings.Contains(username, "..") || strings.Contains(username, "/") || strings.Contains(username, "\\") {
		return nil
	}
	path := filepath.Join(provisionRequestsDir, username+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pr ProvisionRequest
	if json.Unmarshal(data, &pr) != nil {
		return nil
	}
	return &pr
}

func saveProvisionRequest(pr *ProvisionRequest) error {
	// Same traversal guard as loadProvisionRequest/deleteProvisionRequest (and
	// SaveUser): the username becomes a filename, so a value carrying "..", "/"
	// or "\" must never reach filepath.Join. Auth'd usernames cannot normally
	// contain these (makeCanonical rejects them), but the write path fails
	// closed rather than trusting every future caller to have checked.
	if strings.Contains(pr.Username, "..") || strings.Contains(pr.Username, "/") || strings.Contains(pr.Username, "\\") {
		return fmt.Errorf("invalid username for provision request: %q", pr.Username)
	}
	// Best-effort: a failed mkdir surfaces via the WriteFile error below.
	_ = os.MkdirAll(provisionRequestsDir, 0o755)
	data, err := json.MarshalIndent(pr, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(provisionRequestsDir, pr.Username+".json")
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func deleteProvisionRequest(username string) {
	if strings.Contains(username, "..") || strings.Contains(username, "/") || strings.Contains(username, "\\") {
		return
	}
	if err := os.Remove(filepath.Join(provisionRequestsDir, username+".json")); err != nil && !os.IsNotExist(err) {
		slog.Warn("deleteProvisionRequest: remove failed", "user", username, "error", err)
	}
}

func listProvisionRequests() []ProvisionRequest {
	// Best-effort: a failed mkdir surfaces via the ReadDir error below.
	_ = os.MkdirAll(provisionRequestsDir, 0o755)
	entries, err := os.ReadDir(provisionRequestsDir)
	if err != nil {
		return nil
	}
	var result []ProvisionRequest
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		uname := strings.TrimSuffix(e.Name(), ".json")
		pr := loadProvisionRequest(uname)
		// Return decided requests too, not just pending ones — the admin view
		// splits them into a pending action queue and a decision-history table,
		// and filtering here made the history permanently empty.
		if pr != nil {
			result = append(result, *pr)
		}
	}
	return result
}

func (s *HubServer) handleRequestProvision(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	if username == "" {
		http.Error(w, `{"error":"not authenticated"}`, http.StatusUnauthorized)
		return
	}

	existing := loadProvisionRequest(username)
	if existing != nil && existing.Status == provisionStatusPending {
		http.Error(w, `{"error":"you already have a pending provision request"}`, http.StatusBadRequest)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxProvisionRequestBodyBytes)
	var body struct {
		Org         string `json:"org"`
		GitHubHost  string `json:"github_host"`
		Repos       string `json:"repos"`
		PrimaryRepo string `json:"primary_repo"`
		ACMMLevel   int    `json:"acmm_level"`
		AuthMethod  string `json:"auth_method"`
		FullName    string `json:"full_name"`
		SlackID     string `json:"slack_id"`
		Country     string `json:"country"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.Org == "" || body.Repos == "" {
		http.Error(w, `{"error":"org and repos are required"}`, http.StatusBadRequest)
		return
	}
	// Contact details for the person asking.
	//
	// No charset validation on purpose: isValidName() is for identifiers
	// (org/repo/host names) and would reject spaces, apostrophes and every
	// non-ASCII script. These are display strings — trimmed, rune-capped and
	// escaped at every render site, exactly as the admin contact editor
	// (handleAdminUpdateUser) already treats these same two fields. We also do
	// NOT try to detect a "real" name: any pattern for that (two words,
	// capitalised, Latin script) is wrong for a large fraction of the world's
	// names, and a determined user types junk regardless. Human review is the
	// check, not a regex.
	body.FullName = truncateRunes(strings.TrimSpace(body.FullName), maxContactNameLen)
	body.SlackID = truncateRunes(strings.TrimSpace(body.SlackID), maxContactSlackIDLen)
	// Country is OPTIONAL and normalized rather than rejected: a malformed or
	// absent code stores "", which renders no flag at all. Validating here (the
	// last point before the PVC) means the render sites can trust that a stored
	// country is two uppercase letters, and a client that never sends the field
	// behaves exactly as before this shipped.
	body.Country = normalizeCountryCode(body.Country)
	// Accept a pasted org/repo URL, not just a bare name. Users read
	// "GitHub Organization" and paste the org's URL; the old validator rejected
	// ":" and "/" and returned a bare "invalid org name" that explained nothing.
	// The host is kept so the admin can see whether a request is for github.com
	// or a GitHub Enterprise instance before placing the hive.
	ghHost, orgName := normalizeOrgRef(body.Org)
	body.Org = orgName
	if ghHost != "" {
		body.GitHubHost = ghHost
	}
	// The forge is REQUIRED. A bare org name ("z-innersource") does not say
	// whether the org lives on github.com or on a GitHub Enterprise instance,
	// and the hub cannot guess: the wrong choice provisions the hive against
	// the wrong GitHub and points the App-install link at a forge the org
	// admin never sees. Normalize FIRST — the field accepts
	// "https://github.ibm.com/" and isValidName rejects ":" and "/", so
	// validating the raw value would reject a form the form itself advertises.
	forgeHost, ok := normalizeForgeHost(body.GitHubHost)
	if !ok {
		http.Error(w, `{"error":"a GitHub forge is required — enter the host your org lives on (e.g. github.com or github.ibm.com); without it we cannot tell which GitHub to provision against"}`, http.StatusBadRequest)
		return
	}
	body.GitHubHost = forgeHost
	// Single-host-per-spoke: every repo (and the primary) must be on the same
	// GitHub host as the org. Check BEFORE normalizeRepoRef strips the host off
	// each pasted repo. Reject a mixed request up front with a clear message —
	// a spoke that mixed github.com and a GHE instance would silently fail to
	// authenticate against half its repos, the onboarding footgun this removes.
	if err := validateSingleRepoHost(body.GitHubHost, body.PrimaryRepo, strings.Split(body.Repos, ",")); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
		return
	}
	{
		var cleaned []string
		for _, repo := range strings.Split(body.Repos, ",") {
			if r := normalizeRepoRef(repo); r != "" {
				cleaned = append(cleaned, r)
			}
		}
		body.Repos = strings.Join(cleaned, ",")
		body.PrimaryRepo = normalizeRepoRef(body.PrimaryRepo)
	}
	if !isValidName(body.Org) {
		http.Error(w, fmt.Sprintf(`{"error":"invalid org name %q — use the org name or its URL (e.g. github.ibm.com/my-org)"}`, body.Org), http.StatusBadRequest)
		return
	}
	// Defence in depth: normalizeForgeHost above already guaranteed this, but
	// keep the check so a future edit that reorders the handler cannot let an
	// unvalidated host reach the stored request.
	if !isValidName(body.GitHubHost) {
		http.Error(w, `{"error":"invalid github host"}`, http.StatusBadRequest)
		return
	}
	for _, repo := range strings.Split(body.Repos, ",") {
		if !isValidRepoRef(strings.TrimSpace(repo)) {
			http.Error(w, `{"error":"invalid repo name"}`, http.StatusBadRequest)
			return
		}
	}

	// Name is REQUIRED; Slack ID is optional.
	//
	// The name exists to map a GitHub login to a person an operator can talk
	// to, which only works if it is actually populated — a field that is
	// usually blank cannot be relied on and stops being read. Requesting a hive
	// is a deliberate, one-per-user action already reviewed by a human, so one
	// short field is negligible friction at the moment the requester is most
	// motivated to answer.
	//
	// Checked AFTER the org/forge/repo validation above so the more specific
	// "which GitHub is this org on?" errors keep their precedence — a request
	// missing both should be told about the forge, not sent round the loop one
	// field at a time.
	//
	// This does tighten an existing endpoint: a caller that posted no full_name
	// now gets a 400 where it used to get a 200. That is intended — the whole
	// point is that the field is reliably present.
	//
	// The get-started wizard (static/get-started.html) is now the only in-tree
	// caller, but do not read that as "it always was". When this check landed
	// (#2369) this comment claimed the wizard was the only caller and it was
	// simply wrong: the hub dashboard had its own Request-a-Hive modal posting
	// here, added later than the wizard, reachable by every logged-in user, and
	// it went un-updated — so the button 400'd with no field on screen that
	// could satisfy it. The modal has since been removed deliberately (the
	// wizard is the single supported request path), which is what makes this
	// sentence true today rather than aspirational.
	//
	// The modal was invisible to CI because it was inline JS inside the
	// dashboardHTML asset with no test naming any of its symbols. Before
	// adding another required field here, re-run the caller audit rather than
	// trusting this comment: TestRequestProvisionInTreeCallersSendRequiredFields
	// greps the embedded JS and the static wizard for callers of this endpoint
	// and fails on one that omits a required field.
	if body.FullName == "" {
		http.Error(w, `{"error":"your name is required — we use it to know who the request is from"}`, http.StatusBadRequest)
		return
	}

	// A hive is never REQUESTED above L3 Quality-Gated. L4-L6 are real levels, but
	// they are reached after provisioning, from the hive's own dashboard, once
	// the project has the coverage and CI history to justify them. The
	// get-started wizard only offers L1-L3, but the wizard is one client: clamp
	// here so a crafted request cannot provision straight into auto-merge.
	// Admin paths (assign/provision) keep the full 0..6 range on purpose — an
	// operator setting a level deliberately is not the case being guarded.
	acmm := body.ACMMLevel
	if acmm < minRequestACMMLevel || acmm > maxRequestACMMLevel {
		acmm = minRequestACMMLevel
	}

	primaryRepo := body.PrimaryRepo
	if primaryRepo == "" {
		repos := strings.Split(body.Repos, ",")
		if len(repos) > 0 {
			primaryRepo = strings.TrimSpace(repos[0])
		}
	}

	userID, userIDSource := provisionRequestUserIdentity(username, loadSaaSUser(username), body.FullName)
	pr := &ProvisionRequest{
		Username:     username,
		UserID:       userID,
		UserIDSource: userIDSource,
		GitHubHost:   body.GitHubHost,
		Org:          body.Org,
		Repos:        body.Repos,
		PrimaryRepo:  primaryRepo,
		ACMMLevel:    acmm,
		AuthMethod:   body.AuthMethod,
		FullName:     body.FullName,
		SlackID:      body.SlackID,
		Country:      body.Country,
		RequestedAt:  time.Now().UTC().Format(time.RFC3339),
		Status:       provisionStatusPending,
	}
	if err := saveProvisionRequest(pr); err != nil {
		http.Error(w, `{"error":"failed to save provision request"}`, http.StatusInternalServerError)
		return
	}

	s.logger.Info("audit: provision request created", "user", username, "org", body.Org, "repos", body.Repos)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "status": provisionStatusPending})
}

// ApproveProvisionRequest is the OPTIONAL body of PUT
// /api/saas/approve-provision/{username}. HiveID lets the admin pick the exact
// available placeholder to assign (from the approve-picker modal); an empty or
// absent HiveID preserves the historical auto-pick behavior.
type ApproveProvisionRequest struct {
	HiveID string `json:"hive_id"`
	// GitHubHost is the admin's explicit choice of GitHub instance for this
	// hive, overriding the one on the provision request. "public" forces public
	// github.com even on a GitHub Enterprise cluster; empty means "use the
	// request's host, else the cluster default".
	GitHubHost string `json:"github_host,omitempty"`
}

func (s *HubServer) handleApproveProvision(w http.ResponseWriter, r *http.Request) {
	targetUsername := r.PathValue("username")
	approver := s.getAuthUser(r)

	pr := loadProvisionRequest(targetUsername)
	if pr == nil || pr.Status != provisionStatusPending {
		http.Error(w, `{"error":"no pending provision request for this user"}`, http.StatusNotFound)
		return
	}

	// Optionally the admin picks the EXACT placeholder to assign (from the
	// approve-picker modal) instead of letting the hub auto-pick. The body is
	// tolerated as absent/empty — an empty hive_id preserves the historical
	// auto-pick behavior. The body is tiny (a single id), so cap it small.
	const maxApproveRequestBodyBytes = 1 * 1024
	var approveBody ApproveProvisionRequest
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxApproveRequestBodyBytes)
		// An empty body is valid (auto-pick); ignore EOF/empty decode errors and
		// fall through to auto-pick. Any non-empty malformed body is rejected.
		if err := json.NewDecoder(r.Body).Decode(&approveBody); err != nil && err != io.EOF {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
	}

	// Approving now ASSIGNS an available placeholder instead of just bumping
	// quota for a manual provision. Pick the pool from the request's auth_method
	// (private → the heartbeat-only cluster / GPU pool, otherwise → the hub-reachable cluster / public pool). If no
	// placeholder is available in that pool, tell the admin to provision more.
	pool := poolClusterForAuthMethod(pr.AuthMethod)
	hiveID := strings.TrimSpace(approveBody.HiveID)
	if hiveID != "" {
		// Admin chose a specific placeholder — validate it is an available
		// placeholder (same check the assign path uses) before using it. The
		// full status recheck under loadSaaSHive below still guards the race.
		if sel := loadSaaSHive(hiveID); sel == nil || sel.Status != statusAvailable || !isHubAdmin(sel.Owner) {
			http.Error(w, `{"error":"selected placeholder is not available"}`, http.StatusConflict)
			return
		}
	} else {
		hiveID = findAvailablePlaceholder(pool)
	}
	if hiveID == "" {
		http.Error(w, fmt.Sprintf(`{"error":"no available placeholder hive in pool %q — provision more placeholders"}`, pool), http.StatusConflict)
		return
	}

	h := loadSaaSHive(hiveID)
	if h == nil || h.Status != statusAvailable {
		// Raced with another assignment between selection and load.
		http.Error(w, `{"error":"selected placeholder became unavailable — retry"}`, http.StatusConflict)
		return
	}

	// Reuse the request's org/repos/primary_repo/acmm as the assignment inputs.
	var repos []string
	for _, repo := range strings.Split(pr.Repos, ",") {
		if repo = strings.TrimSpace(repo); repo != "" {
			repos = append(repos, repo)
		}
	}
	primaryRepo := pr.PrimaryRepo
	if primaryRepo == "" && len(repos) > 0 {
		primaryRepo = repos[0]
	}
	acmm := pr.ACMMLevel
	if acmm == 0 {
		acmm = defaultAssignACMMLevel
	}
	if acmm < minAssignACMMLevel || acmm > maxAssignACMMLevel {
		acmm = defaultAssignACMMLevel
	}

	// Rewrite the placeholder's meta.json to the requesting user's real project.
	// Flipping status to statusAssigned makes it show under the new owner in My
	// Hives AND marks it as no-longer-available for the fleet counters; the
	// project config reaches the spoke via the heartbeat channel
	// (projectConfigForHiveID).
	h.Owner = targetUsername
	h.Org = pr.Org
	h.Repos = repos
	h.PrimaryRepo = primaryRepo
	h.ACMMLevel = acmm
	// Record the level as REQUESTED, not merely as current. ACMMLevel is
	// overwritten the moment the spoke reports the level it was minted at, so it
	// cannot be the source of truth for "what did the owner ask for". Keeping the
	// requested value in its own field is what lets the delivery reconcile below
	// remain correct — and idempotent — across any number of heartbeats.
	h.RequestedACMMLevel = acmm
	// Re-arm the level handshake for this claim. A placeholder is reused across
	// assignments, so a flag left true by a previous tenancy would suppress
	// delivery for the new owner.
	h.ACMMDelivered = false
	// Re-arm the org/repos claim handshake too (#2372). ClaimDelivered gates
	// both halves of adoptSpokeProjectConfig: the org/repos PUSH fires only
	// while !ClaimDelivered, and the spoke's report is ADOPTED only once it is
	// true. A RECYCLED placeholder (previously claimed, returned to the pool)
	// carries the prior tenant's ClaimDelivered=true, so without this reset the
	// hub never pushes the new owner's org/repos AND adopts the spoke's stale
	// self-report — hub and spoke silently agree on the PREVIOUS tenant's
	// project. The heartbeat is the only hub->spoke write channel, so the claim
	// must re-arm on (re)assignment for exactly the same reason ACMMDelivered
	// does above. (The assign path — handleAssignHive — already does this.)
	h.ClaimDelivered = false
	h.Status = statusAssigned
	// Stamp when this claim began so the self-heal sweep can age it out if the
	// spoke never reports the project back (ClaimDelivered stuck false).
	h.AssignedAt = time.Now().UTC().Format(time.RFC3339)
	h.Error = ""
	// Preserve the placeholder's real cluster before ANY cluster-derived
	// resolution below (host backfill uses s.clusterForHive(h), which silently
	// returns the hub-reachable cluster when ClusterID is blank). The placeholder was picked
	// from `pool`, so a blank cluster_id here can only mean the placeholder was
	// created without one — stamp the pool it came from rather than leaving a
	// blank that later resolves to the wrong (default) cluster's App/host.
	s.ensureClusterIDForClaim(h, pool)
	// The requester already told us which GitHub their org lives on (parsed
	// from the org URL they pasted, or picked explicitly), so honour it. Before
	// this, approve dropped github_host entirely — only the manual assign path
	// ever set it — and a GHE request approved through this path produced a
	// hive with a blank host talking to api.github.com. An override the admin
	// chose in the approve modal arrives on the request body below and wins.
	if host := strings.TrimSpace(approveBody.GitHubHost); host != "" {
		if !isValidName(host) && !strings.EqualFold(host, githubHostPublic) {
			http.Error(w, `{"error":"invalid github host"}`, http.StatusBadRequest)
			return
		}
		// An explicit "public" choice means public github.com. Record it as a
		// blank host so forgeAPIURLForHost pushes nothing and the spoke keeps its
		// own api.github.com default — and so the cluster backfill below, which
		// only ever fills a blank, does not silently re-GHE it.
		if strings.EqualFold(host, githubHostPublic) {
			// Record public github.com EXPLICITLY. This used to store a blank
			// host plus the "public" sentinel, because a blank was the only way
			// to stop backfillGitHubHostFromCluster re-GHE-ing the hive on the
			// next line. Storing the real host achieves the same thing — that
			// backfill only ever fills a value that is EMPTY — without leaving
			// an absent field that means "public" by implication.
			//
			// An absent field is what hid the 2026-07-31 incident and what left
			// 25 of 50 hub records with no github_host at all. github_host is
			// the single stored input for a hive's identity (#2386); it should
			// never be the one field we deliberately leave blank.
			h.GitHubHost = publicForgeHost
		} else {
			h.GitHubHost = host
		}
	} else if pr.GitHubHost != "" {
		// The request itself named a host. Honour a "public" sentinel here the SAME
		// way the admin override does: the self-service onboarding form now sends
		// "public" for an explicit github.com choice (never a blank), so a
		// github.com request must be pinned public — NOT stored as the literal host
		// "public", and NOT left blank for the cluster backfill below to re-GHE.
		if strings.EqualFold(pr.GitHubHost, githubHostPublic) {
			// Same as the admin-override branch above: store the real host, not
			// a blank plus the sentinel. The literal string "public" must never
			// be stored as a host — it is a request-time marker, not a hostname,
			// and a hive naming it resolves to no forge at all.
			h.GitHubHost = publicForgeHost
		} else {
			h.GitHubHost = pr.GitHubHost
		}
	}
	// Same cluster backfill the manual assign path does: when neither the admin
	// nor the request named a host, inherit the cluster's GHE default rather
	// than leaving a blank that pushes an empty API URL forever.
	if host := backfillGitHubHostFromCluster(h, s.clusterForHive(h)); host != "" {
		h.GitHubHost = host
		s.logger.Info("backfilled hive github host from cluster defaults",
			"hive", hiveID, "github_host", host)
	}
	if err := saveSaaSHive(h); err != nil {
		http.Error(w, `{"error":"failed to assign placeholder hive"}`, http.StatusInternalServerError)
		return
	}

	// Entry point 2/3 for namespace identity: this is the moment a placeholder
	// gets a real owner/org — the hive's identity is known here for the first
	// time, so the namespace's labels/annotations must be (re)written. That
	// stamp is kubectl against the hive's cluster, so it runs in the BACKGROUND
	// via kickClaimClusterWorkAsync below — inline it held the approve dialog's
	// fetch behind up to 2×15s of dial timeouts against an unreachable cluster
	// (the same request-path disease #2730 cured on the heartbeat path).

	// Ensure the user record exists, grant them owner access, and count this
	// owned hive against a quota.
	//
	// Granting Hives[hiveID] is what actually puts the requester on the hive's
	// permissions: handleAccessList builds the access list by scanning every
	// user record for Hives[hiveID], NOT from h.Owner. Without this the
	// assignment set h.Owner correctly but the new owner never appeared under
	// Manage Access — only the admin who provisioned the placeholder did — and
	// on a heartbeat-only cluster (the heartbeat-only cluster) that stale list is what gets
	// delivered to the spoke.
	user := loadSaaSUser(targetUsername)
	if user == nil {
		user = ensureSaaSUser(targetUsername)
	}
	if user.Hives == nil {
		user.Hives = map[string]string{}
	}
	user.Hives[hiveID] = "owner"
	user.SaaSQuota++
	applyRequestContactToUser(user, pr)
	if err := saveSaaSUser(user); err != nil {
		s.logger.Warn("assigned placeholder but failed to grant owner access", "user", targetUsername, "hive", hiveID, "error", err)
	}

	// Mark the request fulfilled, recording who approved it and which hive the
	// requester actually received — that pairing is the whole point of the
	// history table, and it is unrecoverable after the fact if not stored now.
	pr.Status = provisionStatusApproved
	pr.DecidedBy = approver
	pr.DecidedAt = time.Now().UTC().Format(time.RFC3339)
	pr.AssignedHive = hiveID
	if err := saveProvisionRequest(pr); err != nil {
		s.logger.Warn("assigned placeholder but failed to update provision request", "user", targetUsername, "error", err)
	}

	s.logger.Info("audit: provision request approved via placeholder assignment",
		"target", targetUsername, "by", approver, "org", pr.Org, "repos", pr.Repos,
		"hive_id", hiveID, "cluster", clusterIDForHive(h))

	// Everything the response depends on is persisted above — all fast local
	// ops. The cluster-facing side effects (namespace identity stamp; vanity
	// mint, which this path previously left to the heartbeat repair) run in the
	// background so the approve dialog gets its ack immediately; the claim
	// itself reaches the spoke over the heartbeat channel regardless.
	s.kickClaimClusterWorkAsync(hiveID)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"status":  provisionStatusApproved,
		"hive_id": hiveID,
	})
}

func (s *HubServer) handleDenyProvision(w http.ResponseWriter, r *http.Request) {
	targetUsername := r.PathValue("username")
	denier := s.getAuthUser(r)

	pr := loadProvisionRequest(targetUsername)
	if pr == nil || pr.Status != provisionStatusPending {
		http.Error(w, `{"error":"no pending provision request for this user"}`, http.StatusNotFound)
		return
	}

	// Retain the record instead of deleting it. Deleting made a denial
	// indistinguishable from a request that was never made — the history table
	// could only ever show approvals, and an admin had no way to answer "did we
	// already turn this person down, and why?". The retained record is what
	// makes a denial re-requestable-but-accountable.
	const maxDenyRequestBodyBytes = 1 * 1024
	var denyBody struct {
		Reason string `json:"reason"`
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxDenyRequestBodyBytes)
		_ = json.NewDecoder(r.Body).Decode(&denyBody) // absent body is fine
	}
	pr.Status = provisionStatusDenied
	pr.DecidedBy = denier
	pr.DecidedAt = time.Now().UTC().Format(time.RFC3339)
	pr.DenyReason = strings.TrimSpace(denyBody.Reason)
	if err := saveProvisionRequest(pr); err != nil {
		s.logger.Warn("failed to record provision denial", "target", targetUsername, "error", err)
	}

	s.logger.Info("audit: provision request denied", "target", targetUsername, "by", denier)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// authMethodPrivate is the request auth_method value that routes a provision
// request to the private/GPU placeholder pool (the heartbeat-only cluster). Any other value (the
// default) routes to the public pool (the hub-reachable cluster).
const authMethodPrivate = "private"

// poolClusterForAuthMethod maps a provision request's auth_method to the
// placeholder pool it should draw from: private methods → the GPU pool
// (the heartbeat-only cluster); everything else → the public pool (the hub-reachable cluster).
func poolClusterForAuthMethod(authMethod string) string {
	if strings.EqualFold(authMethod, authMethodPrivate) || strings.EqualFold(authMethod, gpuClusterID) {
		return gpuClusterID
	}
	return defaultClusterID
}

// clusterIDForHive returns the effective cluster ID of a SaaS hive, treating an
// empty cluster_id as the default (the hub-reachable cluster) — matching clusterForHive's own
// fallback so pool matching agrees with cluster resolution.
func clusterIDForHive(h *SaaSHive) string {
	if h.ClusterID == "" {
		return defaultClusterID
	}
	return h.ClusterID
}

// findAvailablePlaceholder returns the ID of an available placeholder hive in
// the given pool (cluster), or "" if none exists. A placeholder is a SaaS hive
// owned by the hub admin, sitting at statusAvailable, on the target cluster.
func findAvailablePlaceholder(clusterID string) string {
	for _, h := range listSaaSHives() {
		if h.Status != statusAvailable {
			continue
		}
		if !isHubAdmin(h.Owner) {
			continue
		}
		if clusterIDForHive(&h) != clusterID {
			continue
		}
		return h.ID
	}
	return ""
}

// AvailablePlaceholder is one row of the approve-picker dropdown: an available
// placeholder hive the admin can assign a provision request to.
type AvailablePlaceholder struct {
	ID          string `json:"id"`
	ClusterID   string `json:"cluster_id"`
	ProjectName string `json:"project_name"`
}

// listAvailablePlaceholders returns every available placeholder (admin-owned,
// statusAvailable), optionally filtered to a single pool (cluster). An empty
// pool returns placeholders across all pools. It mirrors findAvailablePlaceholder's
// availability predicate so the picker and the assign path agree on what's usable.
func listAvailablePlaceholders(pool string) []AvailablePlaceholder {
	var result []AvailablePlaceholder
	for _, h := range listSaaSHives() {
		if h.Status != statusAvailable {
			continue
		}
		if !isHubAdmin(h.Owner) {
			continue
		}
		cluster := clusterIDForHive(&h)
		if pool != "" && cluster != pool {
			continue
		}
		result = append(result, AvailablePlaceholder{
			ID:          h.ID,
			ClusterID:   cluster,
			ProjectName: h.ProjectName,
		})
	}
	return result
}

// handleAvailablePlaceholders (admin-only) returns the available placeholders
// the approve-picker modal populates its dropdown from. An optional ?pool=
// filters to a single cluster; the default is all available placeholders.
func (s *HubServer) handleAvailablePlaceholders(w http.ResponseWriter, r *http.Request) {
	pool := strings.TrimSpace(r.URL.Query().Get("pool"))
	placeholders := listAvailablePlaceholders(pool)
	if placeholders == nil {
		placeholders = []AvailablePlaceholder{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"placeholders": placeholders})
}

// projectConfigForHiveID returns the claimed project's real org/repos/ACMM for
// delivery to the spoke in its heartbeat response, or nil when no reconcile is
// needed. It mirrors authorizedUsersForHiveID: nil when the hive has no SaaS
// record, is still an unclaimed placeholder (statusAvailable), or the spoke
// already reports the recorded project. The spoke's currently-reported values
// (curOrg/curRepos/curPrimary/curACMM) let the hub stop sending once matched.
// adoptSpokeProjectConfig makes the hub's meta.json track what a CLAIMED hive's
// spoke reports for its OPERATOR-CONTROLLED runtime settings: org, repos,
// primary_repo, and ACMM level. Once a placeholder is claimed, the spoke
// dashboard is the source of truth for these — an operator can re-point repos or
// change the level there. Without adopting them, the reconcile in
// projectConfigForHiveID keeps re-pushing meta's old values and silently reverts
// the operator's edit every heartbeat (the spyre / Joe Runde bug — originally
// just ACMM, but org/repos have the identical failure mode).
//
// Guardrails:
//   - No-op for a hive with no SaaS record or an unclaimed placeholder
//     (statusAvailable) — assign owns those until claimed.
//   - Never adopt EMPTY/zero values: a spoke reporting empty org/repos or level 0
//     (e.g. mid-boot, before its config loads) must not wipe/downgrade meta.
//   - Only writes when something actually changed.
func (s *HubServer) adoptSpokeProjectConfig(hiveID, org string, repos []string, primary string, level int) {
	h := loadSaaSHive(hiveID)
	if h == nil || h.Status == statusAvailable {
		return // no record, or an unclaimed placeholder — assign controls it
	}
	changed := false
	prevLevel := h.ACMMLevel

	// ACMM runs its OWN delivery handshake (ACMMDelivered), not the org/repos
	// one (ClaimDelivered).
	//
	// #2061 made ACMM "operator-owned from the start — always adopt", which fixed
	// the spyre revert but left a gap on the ASSIGN path: a freshly-claimed
	// placeholder keeps reporting the level it was MINTED at, and adopting that
	// pre-delivery report overwrote the level the requester asked for. #2333
	// closed that by gating on ClaimDelivered — correct for hives claimed after
	// it shipped, but a no-op for every hive claimed BEFORE, whose
	// ClaimDelivered was already true from the old org/repos-only rule. Those
	// hives adopt the stale report on the very first beat after upgrade and the
	// push stays disabled, which is why the live oke-11 hive is still L2 against
	// an approved L3 request.
	//
	// The dedicated flag defaults to false on those hives, so the level is
	// delivered exactly once, then ownership passes to the spoke as #2061
	// intended.

	// Backfill the requested level for hives assigned before the field existed.
	// Without this the reconcile has no target on exactly the hives that need
	// it. ACMMLevel is the best available record of the assignment at this
	// point, and using it is safe: if the spoke already agrees, the delivery is
	// a no-op that simply marks itself done.
	if h.RequestedACMMLevel == 0 && h.ACMMLevel > 0 {
		h.RequestedACMMLevel = h.ACMMLevel
		changed = true
	}

	// Delivery is complete once the spoke reports the level the hub asked for.
	// A level-0 report means "too old / mid-boot to say", never a mismatch.
	if !h.ACMMDelivered && h.RequestedACMMLevel > 0 && level == h.RequestedACMMLevel {
		h.ACMMDelivered = true
		changed = true
		s.logger.Info("acmm level delivered to spoke",
			"hive_id", hiveID, "acmm_level", level)
	}

	switch {
	case level <= 0:
		// Nothing reported — say nothing, change nothing.
	case !h.ACMMDelivered && h.RequestedACMMLevel > 0:
		// Pre-delivery the REQUESTED level is authoritative. Hold meta at it so
		// the stale spoke report cannot overwrite the target the push below is
		// still working toward — the exact loop that lost the L3.
		if h.ACMMLevel != h.RequestedACMMLevel {
			h.ACMMLevel = h.RequestedACMMLevel
			changed = true
		}
		if level != h.RequestedACMMLevel {
			s.logger.Info("acmm level not yet applied on spoke; holding requested level",
				"hive_id", hiveID,
				"spoke_reports", level,
				"requested", h.RequestedACMMLevel)
		}
	case level != h.ACMMLevel:
		// Post-delivery the spoke's dashboard owns the level: adopt operator
		// edits, and keep the requested level in step so a later re-delivery
		// never reverts the operator's choice.
		h.ACMMLevel = level
		h.RequestedACMMLevel = level
		changed = true
	}

	// Org/repos: while the claim hasn't been delivered yet, the hub is still
	// PUSHING them down and the spoke may report its OLD placeholder project —
	// do NOT adopt that (it would clobber the real claim). Mark the claim
	// delivered once the spoke reports the matching org/repos, and only AFTER
	// delivery treat the spoke as the source of truth for repo edits.
	orgMatches := org != "" && strings.EqualFold(org, h.Org)
	reposMatch := len(repos) > 0 && sameStringSliceFold(repos, h.Repos)
	primaryMatches := primary == "" || strings.EqualFold(primary, h.PrimaryRepo)
	// ACMM is NO LONGER part of this condition. #2333 added it here so the claim
	// could not be declared delivered while the level was outstanding, but that
	// coupled two independent deliveries: a hive whose level lagged would also
	// have its org/repos pushed forever, and — worse — the level had no way to
	// re-arm on a hive whose ClaimDelivered was already true. ACMMDelivered
	// above now tracks the level on its own, so this returns to being purely
	// about the project payload.
	if !h.ClaimDelivered {
		if orgMatches && reposMatch && primaryMatches {
			h.ClaimDelivered = true
			changed = true
		}
	} else {
		if org != "" && !strings.EqualFold(org, h.Org) {
			h.Org = org
			changed = true
		}
		if len(repos) > 0 && !sameStringSliceFold(repos, h.Repos) {
			h.Repos = repos
			changed = true
		}
		if primary != "" && !strings.EqualFold(primary, h.PrimaryRepo) {
			h.PrimaryRepo = primary
			changed = true
		}
	}
	if !changed {
		return
	}
	if err := saveSaaSHive(h); err != nil {
		s.logger.Warn("failed to persist spoke-reported project config to meta",
			"hive_id", hiveID, "error", err)
		return
	}
	// Keep the in-memory registry consistent so the UI and the next
	// projectConfigForHiveID comparison see the adopted values immediately.
	s.mu.Lock()
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == hiveID {
			s.registry.Hives[i].Org = h.Org
			s.registry.Hives[i].Repos = h.Repos
			s.registry.Hives[i].PrimaryRepo = h.PrimaryRepo
			s.registry.Hives[i].ACMMLevel = h.ACMMLevel
			break
		}
	}
	s.mu.Unlock()
	s.logger.Info("adopted dashboard-set project config from spoke heartbeat",
		"hive_id", hiveID, "org", h.Org, "primary_repo", h.PrimaryRepo,
		"acmm_was", prevLevel, "acmm_now", h.ACMMLevel)
}

// claimedVanityURL returns the vanity dashboard URL the hub should SHOW and LINK
// for a hive (My Hives, the SSO /open handoff, config proxy), or "" to fall back
// to the spoke-reported placeholder host.
//
// The rule is deliberately narrow so it never resurrects the 503 bug:
//   - Unclaimed placeholder (statusAvailable): "" — it has no project yet, so its
//     placeholder host is the only correct URL. Leave it.
//   - Claimed hive with a non-empty meta VanityURL: return it. A vanity URL is
//     only non-empty because it was VALIDATED as servable at provision/assign
//     time (addVanityHostToIngress succeeded, or a cluster wildcard/OpenShift
//     route already serves it, e.g. the heartbeat-only cluster's hosted OpenShift-route host).
//     Trusting it here means the hub shows/links the friendly host the instant a
//     hive is claimed, instead of waiting for the spoke to adopt+report it back.
//   - Claimed hive with an empty VanityURL: "" — never mint an unvalidated host
//     here; the placeholder still works.
func claimedVanityURL(h *SaaSHive) string {
	if h == nil || h.Status == statusAvailable {
		return ""
	}
	return h.VanityURL
}

// placeholderHostURL builds the "<hiveID>.<domain>" placeholder URL for a hive
// that has not yet reported a dashboard URL, using the domain of the cluster
// the hive actually lives on.
//
// The domain MUST come from the hive's own cluster rather than the hub's
// hardcoded hive.kubestellar.io. That constant is the wildcard fronting the
// HUB's router, so using it for a spoke on any other cluster produces a name
// that resolves to the hub and 503s — the exact defect this path exhibited on
// the OpenShift pool. Deriving it per-cluster is also what keeps this correct
// for clusters added in future, without naming any of them here.
//
// Returns "" when the cluster (or its domain) is unknown, so the caller reports
// "no reachable dashboard URL yet" instead of inventing an unreachable host.
func (s *HubServer) placeholderHostURL(hiveID string) string {
	// A hive with no meta record yet (mid-provision, or a registry-only entry)
	// still resolves through clusterForHive, which falls back to the default
	// cluster. That is the correct answer for it: with nothing recorded about
	// where it lives, the hub's own pool is the only defensible guess, and it
	// is the pool such a hive is in fact provisioned into.
	cluster := s.clusterForHive(&SaaSHive{ID: hiveID})
	if h := loadSaaSHive(hiveID); h != nil {
		cluster = s.clusterForHive(h)
	}
	if cluster == nil || cluster.Domain == "" {
		return ""
	}
	return "https://" + hiveID + "." + strings.Trim(strings.TrimSpace(cluster.Domain), ".")
}

// curAPIURL is the GitHub API base URL the spoke reports it is CURRENTLY using
// (HeartbeatPayload.GitHubAPIURL). Empty means the spoke is too old to report
// it — treated as UNKNOWN, never as a mismatch.
func projectConfigForHiveID(hiveID, curOrg string, curRepos []string, curPrimary string, curACMM int, curURL, curAPIURL string) *HeartbeatProjectConfig {
	h := loadSaaSHive(hiveID)
	if h == nil {
		return nil
	}
	// This reconcile exists ONLY to push a freshly-CLAIMED placeholder's project
	// down to its spoke. It must NEVER touch a pre-existing hive, whose meta.json
	// predates the claim feature and carries stale/empty fields (empty
	// primary_repo, acmm_level: 0) even though the spoke runs a real project at a
	// real ACMM. Reconciling from that stale record silently wiped org/repos and
	// DOWNGRADED live hives to L0. So we only reconcile a record that looks like a
	// genuine claim — a complete project (org + repos + primary_repo) AND a real
	// non-zero ACMM — and even then we never send a value that would blank/lower
	// what the spoke already has.
	if h.Status == statusAvailable { // still an unclaimed placeholder
		return nil
	}
	primary := h.PrimaryRepo
	if primary == "" && len(h.Repos) > 0 {
		primary = h.Repos[0]
	}
	claimComplete := h.Org != "" && len(h.Repos) > 0 && primary != "" && h.ACMMLevel > 0
	if !claimComplete {
		// Incomplete/stale record (a pre-claim hive) — leave the spoke's PROJECT
		// (org/repos/ACMM) alone; reconciling from a stale record wiped/downgraded
		// live hives. BUT the vanity URL is independent of project completeness: a
		// claimed hive can carry a validated meta vanity_url (set at provision,
		// e.g. a hive's hosted OpenShift-route on the heartbeat-only cluster) while its meta's
		// org/repos/ACMM are still stale/empty. Without pushing it, the spoke never
		// adopts the vanity URL and the hub keeps showing the raw placeholder host
		// forever (the placeholder-URL-persists bug). Push the URL alone — never
		// the stale project — until the spoke reports the vanity back.
		//
		// Safety: only a NON-EMPTY VanityURL is ever pushed. A vanity URL is only
		// non-empty because it was validated/served at provision or assign time
		// (addVanityHostToIngress succeeded, or a cluster wildcard route already
		// serves it), so this never pushes an unserved host and never reintroduces
		// the 503.
		//
		// A pending FORGE SWITCH is likewise independent of project
		// completeness — a hive can be moved between forges whether or not its
		// meta carries a complete claim, and the switch is precisely the
		// operator action that must not be silently dropped. Push the API URL
		// alone (never the stale project) until the spoke reports the requested
		// host back.
		if apiURL := pendingForgeAPIURL(h, curAPIURL); apiURL != "" {
			return &HeartbeatProjectConfig{GitHubAPIURL: apiURL}
		}
		if h.VanityURL != "" && curURL != h.VanityURL {
			return &HeartbeatProjectConfig{DashboardURL: h.VanityURL}
		}
		return nil
	}
	// What the hub still PUSHES to the spoke, and until when:
	//   - org/repos/primary_repo: pushed only until the claim is DELIVERED (the
	//     spoke first reports the assigned project). After delivery the spoke's
	//     dashboard owns them and the caller adopts operator edits instead.
	//   - vanity URL: pushed until the spoke first reports it back.
	//   - ACMM level: pushed on its OWN handshake (ACMMDelivered), until the
	//     spoke reports the requested level back. After that it is never pushed
	//     again — the spoke's dashboard owns it and the caller adopts operator
	//     edits, exactly as #2061 intended.
	//
	//     #2061 removed the ACMM push entirely because pushing it FOREVER
	//     reverted every dashboard level change (the spyre bug). Dropping it
	//     altogether meant a freshly-assigned placeholder was never told the
	//     level its owner requested. #2333 bounded the push by ClaimDelivered,
	//     which is right in principle but dead in practice for every hive
	//     claimed before it shipped: their ClaimDelivered was already true, so
	//     the push never fires. Bounding by the level's own flag is what makes
	//     the delivery reachable for those hives — and it is self-limiting, so
	//     re-running it is harmless.
	needClaimPush := !h.ClaimDelivered &&
		(!strings.EqualFold(curOrg, h.Org) ||
			!sameStringSliceFold(curRepos, h.Repos) ||
			!strings.EqualFold(curPrimary, primary))
	// Independent of the project claim: deliver the requested level until the
	// spoke confirms it. curACMM == 0 means the spoke did not report a level, so
	// there is nothing to correct yet.
	needACMMPush := !h.ACMMDelivered && h.RequestedACMMLevel > 0 && curACMM != h.RequestedACMMLevel
	// Vanity URL: the spoke always reports its dashboard URL, so observed is
	// known and an empty one is a real "I have none" rather than silence.
	needURLPush := needsPush(h.VanityURL, curURL, true)
	// A spoke reporting a repo that fails isValidRepoRef is wedged: the hub 400s
	// its every heartbeat ("invalid repo name"), /api/livez then fails on the
	// stale heartbeat and the kubelet crash-loops the pod. Push a corrected
	// project even when the claim was already delivered — otherwise the guard
	// above ("nothing left to push") leaves the hive broken forever, since a
	// wedged spoke can never report anything the hub will accept.
	needRepoRepair := false
	for _, r := range append(append([]string{}, curRepos...), curPrimary) {
		if r != "" && !isValidRepoRef(r) {
			needRepoRepair = true
			break
		}
	}
	// A hive whose GitHubHost was filled in AFTER its claim was delivered —
	// the retroactive repair, or an admin editing the host later — has
	// ClaimDelivered == true and a matching vanity URL, so every gate above is
	// false and the reconcile returns nil forever. The spoke would then keep
	// talking to api.github.com against a GitHub Enterprise org (the heartbeat-only cluster /
	// hosted-available-vllmd-01 failure). Push whenever we have a GHE API URL
	// to deliver and the spoke reports a DIFFERENT one.
	//
	// Deliberately conservative: an empty curAPIURL means the spoke is too old
	// to report its API URL, which is UNKNOWN, not a mismatch — pushing on it
	// would re-send on every beat with no read-back to ever stop it.
	wantAPIURL := forgeAPIURLForHost(h.Forge, h.GitHubHost)
	// The api_url is the field where unknown-vs-mismatch actually bites: a spoke
	// too old to report it sends "", which is NOT "I am on api.github.com". The
	// observedKnown argument carries that distinction explicitly instead of
	// hiding it in a curAPIURL != "" conjunct that a later edit could drop.
	needGHEAPIPush := needsPush(wantAPIURL, curAPIURL, curAPIURL != "")
	// A pending FORGE SWITCH pushes on its own handshake. It cannot ride
	// needGHEAPIPush: that gate is deliberately conservative about an empty
	// curAPIURL (unknown, not a mismatch) and — more importantly — it can never
	// deliver a switch TO public github.com, whose wantAPIURL is "" by
	// definition. An operator moving a hive back to github.com must be able to,
	// so the switch carries its own target and its own read-back.
	forgeAPIURL := pendingForgeAPIURL(h, curAPIURL)
	needForgePush := forgeAPIURL != ""
	if !needClaimPush && !needURLPush && !needRepoRepair && !needGHEAPIPush && !needACMMPush && !needForgePush {
		return nil // nothing left to push
	}
	// Sanitize before pushing. A repo pasted as a URL
	// ("github.ibm.com/enricom-ibm/jackrabbit") has two slashes, which
	// isValidRepoRef rejects — so the hub 400s the spoke's every heartbeat
	// ("invalid repo name"), /api/livez then fails on the stale heartbeat, and
	// the kubelet restarts the pod in a loop. Normalizing here repairs an
	// already-broken hive over the heartbeat, which is the only channel that
	// reaches a firewalled cluster (the heartbeat-only cluster).
	pushRepos := make([]string, 0, len(h.Repos))
	for _, r := range h.Repos {
		if rr := sanitizeRepoEntry(r); rr != "" {
			pushRepos = append(pushRepos, rr)
		}
	}
	if len(pushRepos) == 0 {
		pushRepos = h.Repos
	}
	pushPrimary := sanitizeRepoEntry(primary)
	if pushPrimary == "" {
		pushPrimary = primary
	}

	// Pre-delivery the REQUESTED level is what goes down the wire. The adopt path
	// also holds h.ACMMLevel at that value, so the two normally agree — but
	// stating it here means a push cannot deliver a stale level if this function
	// ever runs against a record the adopt path has not touched yet.
	pushACMM := h.ACMMLevel
	if !h.ACMMDelivered && h.RequestedACMMLevel > 0 {
		pushACMM = h.RequestedACMMLevel
	}

	return &HeartbeatProjectConfig{
		Org:          h.Org,
		Repos:        pushRepos,
		PrimaryRepo:  pushPrimary,
		ACMMLevel:    pushACMM,
		DashboardURL: h.VanityURL,
		// IssueFilter rides the claim push only when the record carries one.
		// nil (the ordinary case) tells the spoke "keep your own filter" — the
		// echo of this struct on later beats must never blank an operator's
		// locally configured project.issue_filter.
		IssueFilter: h.IssueFilter,
		// Point a GHE hive at its enterprise API. jjs-world
		// (hosted-open-source-osscar) is the working reference: a bare
		// primary_repo plus github.api_url = https://<host>/api/v3. Empty host
		// pushes nothing, so a github.com hive keeps the spoke's own default.
		// A pending forge switch wins: forgeAPIURL is the host the operator
		// asked for and is only non-empty while that delivery is outstanding.
		// Once the spoke reports the requested host, ForgeDelivered latches and
		// this falls back to the ordinary host-derived value — which by then
		// derives from the SAME host, so the two agree and nothing flaps.
		GitHubAPIURL: func() string {
			if forgeAPIURL != "" {
				return forgeAPIURL
			}
			return forgeAPIURLForHost(h.Forge, h.GitHubHost)
		}(),
		// AIAuthor is deliberately left empty here. Provisioning state never
		// knows the agents' GitHub account — the spoke owns it — and the spoke
		// treats an empty author as "leave mine alone". Setting it from this
		// struct would reintroduce the blanking bug.
	}
}

// sameStringSliceFold reports whether two string slices contain the same
// entries in the same order, case-insensitively (org/repo names are compared
// case-insensitively throughout the hub).
func sameStringSliceFold(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}

// AssignHiveRequest is the body of POST /api/saas/hives/{id}/assign. It carries
// the real project the placeholder is being claimed for, plus optional GitHub
// App credentials to deliver to the spoke via the heartbeat channel.
type AssignHiveRequest struct {
	Owner string `json:"owner"`
	Org   string `json:"org"`
	// GitHubHost is the GitHub instance the org lives on ("" = public
	// github.com, otherwise a GHE host). Parsed from a pasted org URL when the
	// caller does not send it explicitly.
	GitHubHost     string `json:"github_host,omitempty"`
	Repos          string `json:"repos"`
	PrimaryRepo    string `json:"primary_repo"`
	ProjectName    string `json:"project_name"`
	ACMMLevel      int    `json:"acmm_level"`
	IsPublic       bool   `json:"is_public"`
	AppID          string `json:"app_id"`
	InstallationID string `json:"installation_id"`
	AppPrivateKey  string `json:"app_private_key"`
}

// handleAssignHive assigns an available placeholder hive to a real owner/project
// (admin-only). It rewrites the hive's meta.json to the real project and clears
// its "available" status, then delivers the new project config — and any GitHub
// App creds — to the spoke via the heartbeat response. This works uniformly for
// both reachable (the hub-reachable cluster) and heartbeat-only (the heartbeat-only cluster) clusters: NO hub→spoke
// push or kubectl is used, so a heartbeat-only-cluster claim is delivered entirely by heartbeat.
func (s *HubServer) handleAssignHive(w http.ResponseWriter, r *http.Request) {
	if !isHubAdmin(s.getAuthUser(r)) {
		http.Error(w, `{"error":"admin access required"}`, http.StatusForbidden)
		return
	}
	hiveID := r.PathValue("id")

	// A GitHub App private key PEM can be a few KB, so allow more headroom than
	// the provision-request body limit.
	const maxAssignRequestBodyBytes = 16 * 1024
	r.Body = http.MaxBytesReader(w, r.Body, maxAssignRequestBodyBytes)
	var body AssignHiveRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	if h.Status != statusAvailable {
		http.Error(w, `{"error":"hive is not an available placeholder"}`, http.StatusConflict)
		return
	}

	// Validate the claimed project inputs (reuse the shared validators).
	if body.Owner == "" || !isValidName(body.Owner) {
		http.Error(w, `{"error":"invalid owner"}`, http.StatusBadRequest)
		return
	}
	// Accept a pasted org/repo URL here too — an admin assigning a hive reaches
	// for the same paste the requester did. normalizeOrgRef returns a non-empty
	// host with an EMPTY org when the field held only a forge host
	// ("github.ibm.com") and no org — clear body.Org in that case so the
	// isValidName check below rejects it, instead of the raw hostname (which
	// isValidName accepts, dots and all) silently becoming the org. That
	// host-as-org bug produced the two broken github.ibm.com claims on the heartbeat-only cluster.
	if h, o := normalizeOrgRef(body.Org); h != "" {
		body.Org = o // may be "" for a bare-host paste — rejected just below
		if body.GitHubHost == "" {
			body.GitHubHost = h
		}
	}
	if body.Org == "" || !isValidName(body.Org) {
		http.Error(w, fmt.Sprintf(`{"error":"invalid org name %q — use the org name or its URL (e.g. github.ibm.com/my-org)"}`, body.Org), http.StatusBadRequest)
		return
	}
	// "public" is the sentinel that forces public github.com even on a GHE
	// cluster; it is not a hostname, so exempt it from the hostname validator.
	if body.GitHubHost != "" && !isValidName(body.GitHubHost) && !strings.EqualFold(body.GitHubHost, githubHostPublic) {
		http.Error(w, `{"error":"invalid github host"}`, http.StatusBadRequest)
		return
	}
	if body.Repos == "" {
		http.Error(w, `{"error":"repos are required"}`, http.StatusBadRequest)
		return
	}
	// Single-host-per-spoke (assign path — mirrors the request path). Every repo
	// and the primary must share the spoke's host, checked on the raw pasted
	// values before normalizeRepoRef strips the host. The "public" sentinel means
	// github.com, so pass "" to the validator for it.
	{
		spokeHost := body.GitHubHost
		if strings.EqualFold(spokeHost, githubHostPublic) {
			spokeHost = ""
		}
		if err := validateSingleRepoHost(spokeHost, body.PrimaryRepo, strings.Split(body.Repos, ",")); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
			return
		}
	}
	{
		var cleaned []string
		for _, r := range strings.Split(body.Repos, ",") {
			if rr := normalizeRepoRef(r); rr != "" {
				cleaned = append(cleaned, rr)
			}
		}
		body.Repos = strings.Join(cleaned, ",")
		if body.PrimaryRepo != "" {
			body.PrimaryRepo = normalizeRepoRef(body.PrimaryRepo)
		}
	}
	var repos []string
	for _, repo := range strings.Split(body.Repos, ",") {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			continue
		}
		if !isValidRepoRef(repo) {
			http.Error(w, `{"error":"invalid repo name"}`, http.StatusBadRequest)
			return
		}
		repos = append(repos, repo)
	}
	if len(repos) == 0 {
		http.Error(w, `{"error":"repos are required"}`, http.StatusBadRequest)
		return
	}
	primaryRepo := strings.TrimSpace(body.PrimaryRepo)
	if primaryRepo == "" {
		primaryRepo = repos[0]
	} else if !isValidRepoRef(primaryRepo) {
		http.Error(w, `{"error":"invalid primary repo"}`, http.StatusBadRequest)
		return
	}
	forgeHost := body.GitHubHost
	if strings.EqualFold(forgeHost, githubHostPublic) || forgeHost == "" {
		forgeHost = "github.com"
	}
	if issue := config.ValidateProjectRepoTargets(body.Org, repos, primaryRepo, forgeHost); issue != nil {
		writeJSONError(w, http.StatusBadRequest, issue.Message)
		return
	}

	acmm := body.ACMMLevel
	if acmm == 0 {
		acmm = defaultAssignACMMLevel
	}
	if acmm < minAssignACMMLevel || acmm > maxAssignACMMLevel {
		http.Error(w, `{"error":"acmm_level must be between 0 and 6"}`, http.StatusBadRequest)
		return
	}

	// Rewrite the placeholder's meta.json to the real project. Clearing status
	// (and any stale error) alone makes it show under the new owner in My Hives.
	h.Owner = body.Owner
	h.Org = body.Org
	// Preserve the placeholder's real cluster before the host backfill below,
	// which resolves via s.clusterForHive(h) and would fall back to the hub-reachable cluster on
	// a blank cluster_id. The admin assigns a specific placeholder, so its own
	// ClusterID is authoritative; only a placeholder created without one lands
	// on the default here (never a silent blank that mis-routes to the hub-reachable cluster).
	s.ensureClusterIDForClaim(h, "")
	// Record the GHE host (if any) so the heartbeat can point this spoke at the
	// right GitHub API. Never blank an existing value with an empty one.
	//
	// "public" is an explicit choice of public github.com on a cluster whose
	// defaults point at GHE. Record it as a blank host (so forgeAPIURLForHost
	// pushes nothing and the spoke keeps api.github.com) PLUS the
	// GitHubBaseURL sentinel, which is what makes effectiveGitHubBaseURL
	// resolve to "" and therefore makes the cluster backfill below decline to
	// re-GHE the hive. Without the sentinel a blank host would simply be
	// refilled from the cluster on the very next line.
	if strings.EqualFold(body.GitHubHost, githubHostPublic) {
		// Explicit public github.com, stored as the real host rather than a
		// blank + sentinel. The cluster backfill below only fills an EMPTY
		// host, so a stated value blocks it just as the blank did — and does
		// not leave a field whose absence has to be interpreted.
		h.GitHubHost = publicForgeHost
	} else if body.GitHubHost != "" {
		h.GitHubHost = body.GitHubHost
	}
	// Backfill the host from the hive's cluster when neither the request nor
	// the placeholder carries one. Placeholders provisioned BEFORE their
	// cluster gained github_base_url/github_api_url have GitHubHost == "", and
	// nothing else ever fills it in: projectConfigForHiveID pushes
	// forgeAPIURLForHost(h.Forge, h.GitHubHost), which is empty for those hives, so the
	// spoke keeps api.github.com and the public app_id even though the cluster
	// is a GHE cluster (observed on the heartbeat-only cluster: hosted-available-vllmd-01 has
	// base_url: "" / api_url: "" against a github.ibm.com cluster). The hive's
	// own value always wins; this only fills a blank.
	if host := backfillGitHubHostFromCluster(h, s.clusterForHive(h)); host != "" {
		h.GitHubHost = host
		s.logger.Info("backfilled hive github host from cluster defaults",
			"hive", hiveID, "github_host", host)
	}
	h.Repos = repos
	h.PrimaryRepo = primaryRepo
	if body.ProjectName != "" {
		h.ProjectName = body.ProjectName
	}
	h.ACMMLevel = acmm
	// Same as the approve-provision path: the admin-assigned level is what the
	// hub must deliver, and it needs its own field because the spoke will
	// overwrite ACMMLevel with whatever it is currently running.
	h.RequestedACMMLevel = acmm
	h.ACMMDelivered = false
	h.IsPublic = body.IsPublic
	h.Status = statusAssigned
	// Stamp when this claim began so the self-heal sweep can age it out if the
	// spoke never reports the project back (ClaimDelivered stuck false).
	h.AssignedAt = time.Now().UTC().Format(time.RFC3339)
	h.Error = ""
	// A (re)assignment is a new claim payload: reset delivery so the hub pushes
	// this project to the spoke until it reports the new org/repos back, before
	// letting the spoke's dashboard own them.
	h.ClaimDelivered = false
	// The vanity URL is NOT minted here anymore. It used to be derived and made
	// servable inline (makeVanityHostServable → kubectl against the hive's
	// cluster) between this save and the HTTP response, which held the admin's
	// assign dialog hostage for ~a minute whenever the cluster was slow or
	// unreachable (each kubectl call eats a ~45s TCP dial timeout on the
	// heartbeat-only cluster pool — the same disease #2730 cured on the
	// heartbeat path). The mint — same host preference (name-bearing Option B
	// host, org/repo fallback), same servability seam, same "never adopt an
	// unservable host" rule — now runs in the background via
	// kickClaimClusterWorkAsync below, after the response is written. Nothing
	// about the response depends on it: the claim reaches the spoke over the
	// heartbeat channel regardless, and a failed mint is retried by the
	// heartbeat-kicked repair exactly as before.
	if err := saveSaaSHive(h); err != nil {
		http.Error(w, `{"error":"failed to save hive assignment"}`, http.StatusInternalServerError)
		return
	}

	// Grant the assignee owner access. handleAccessList builds a hive's access
	// list by scanning every user record for Hives[hiveID], NOT from h.Owner —
	// so without this the assignment set h.Owner correctly while Manage Access
	// still showed only the admin who provisioned the placeholder. On a
	// heartbeat-only cluster (the heartbeat-only cluster) that stale list is what reaches the spoke.
	assignee := loadSaaSUser(body.Owner)
	if assignee == nil {
		assignee = ensureSaaSUser(body.Owner)
	}
	if assignee.Hives == nil {
		assignee.Hives = map[string]string{}
	}
	if assignee.Hives[hiveID] != "owner" {
		assignee.Hives[hiveID] = "owner"
		assignee.SaaSQuota++
		if err := saveSaaSUser(assignee); err != nil {
			s.logger.Warn("assigned hive but failed to grant owner access", "user", body.Owner, "hive", hiveID, "error", err)
		}
	}

	// Deliver GitHub App creds (if supplied) via the SAME heartbeat channel the
	// webhook path uses — storePendingGitHubAppConfig queues them for the next
	// heartbeat response (consumePendingGitHubAppConfig in handleHeartbeat). We
	// deliberately do NOT call pushGitHubConfigToSpoke here: it requires a
	// reachable dashboardURL and would fail for the heartbeat-only cluster. The heartbeat path
	// covers both clusters uniformly.
	appDelivered := false
	if body.AppID != "" && body.InstallationID != "" && strings.TrimSpace(body.AppPrivateKey) != "" {
		appID, err1 := strconv.ParseInt(strings.TrimSpace(body.AppID), 10, 64)
		installID, err2 := strconv.ParseInt(strings.TrimSpace(body.InstallationID), 10, 64)
		if err1 != nil || err2 != nil {
			http.Error(w, `{"error":"app_id and installation_id must be numeric"}`, http.StatusBadRequest)
			return
		}
		if !strings.HasPrefix(strings.TrimSpace(body.AppPrivateKey), "-----BEGIN") {
			http.Error(w, `{"error":"app_private_key must be a PEM private key"}`, http.StatusBadRequest)
			return
		}
		s.storePendingGitHubAppConfig(hiveID, &HeartbeatGitHubAppConfig{
			AppID:          appID,
			InstallationID: installID,
			PrivateKey:     strings.TrimSpace(body.AppPrivateKey),
		})
		appDelivered = true
	}

	// NO CREDS PASTED: derive the identity from the forge we already know.
	//
	// The three-way AND above is an ADMIN OVERRIDE, not the normal path — it
	// fires only when someone hand-carries an app_id, an installation_id and a
	// PEM into the dialog. Every other assignment fell through it silently and
	// left the hive on config.PlaceholderAppID, even though h.GitHubHost was
	// resolved a hundred lines earlier and the hub holds that forge's App key.
	// Deriving here is what makes "assign knows the forge, so assign sets the
	// forge identity" true in code rather than only in intent.
	if !appDelivered {
		if appCfg := s.assignTimeAppIdentity(h); appCfg != nil {
			s.storePendingGitHubAppConfig(hiveID, appCfg)
			appDelivered = true
			s.logger.Info("assign: derived github app identity from the hive's forge",
				"hive_id", hiveID,
				"forge", h.GitHubHost,
				"app_id", appCfg.AppID,
				"app_slug", appCfg.AppSlug,
				"api_url", appCfg.APIURL,
				"key_delivered", appCfg.PrivateKey != "",
			)
		} else {
			s.logger.Warn("assign: no github app identity for this hive's forge — spoke keeps the placeholder app_id and starts in dashboard-only mode",
				"hive_id", hiveID,
				"forge", h.GitHubHost,
				"cluster", clusterIDForHive(h),
				"remedy", "name an App for this forge in clusters.json, or supply app_id/installation_id/app_private_key on the assign request",
			)
		}
	}

	// The project config itself is delivered by handleHeartbeat via
	// projectConfigForHiveID on the next beat — it keeps sending until the spoke
	// reports the matching project. No hub→spoke push or kubectl is needed, so
	// this works for the heartbeat-only cluster pool as well as the hub-reachable cluster.

	s.logger.Info("audit: placeholder hive assigned",
		"hive_id", hiveID,
		"owner", h.Owner,
		"org", h.Org,
		"primary_repo", h.PrimaryRepo,
		"acmm_level", h.ACMMLevel,
		"cluster", clusterIDForHive(h),
		"app_creds_delivered", appDelivered,
	)
	s.recordTimeline(hiveID, TimelineOwnership,
		fmt.Sprintf("hive assigned to %s (%s, ACMM %d)", h.Owner, repoDisplayLine(h.Org, h.PrimaryRepo), h.ACMMLevel),
		s.getAuthUser(r))

	// Everything the response depends on is persisted above (meta.json, owner
	// grant, pending App creds, audit/timeline) — all fast local ops. The
	// cluster-facing work (namespace identity stamp + vanity-host mint, both
	// kubectl against the hive's cluster) runs in the background so the assign
	// dialog gets its ack immediately; the row's "claim pending" indicators
	// track actual delivery, which happens over the heartbeat channel anyway.
	s.kickClaimClusterWorkAsync(hiveID)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":       "assigned",
		"id":           hiveID,
		"owner":        h.Owner,
		"org":          h.Org,
		"primary_repo": h.PrimaryRepo,
		"acmm_level":   h.ACMMLevel,
	})
}

func (s *HubServer) handleUserToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		HiveID   string `json:"hive_id"`
		Username string `json:"username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.HiveID == "" || body.Username == "" {
		http.Error(w, `{"error":"hive_id and username required"}`, http.StatusBadRequest)
		return
	}

	requester := s.getAuthUser(r)
	if requester != body.Username && !isHubAdmin(requester) {
		http.Error(w, `{"error":"can only retrieve your own token"}`, http.StatusForbidden)
		return
	}

	user := loadSaaSUser(body.Username)
	if user == nil {
		writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}

	if _, ok := user.Hives[body.HiveID]; !ok {
		http.Error(w, `{"error":"user has no access to this hive"}`, http.StatusForbidden)
		return
	}

	if user.EncryptedToken == "" {
		http.Error(w, `{"error":"no token stored for this user"}`, http.StatusNotFound)
		return
	}

	token, err := decryptToken(user.EncryptedToken)
	if err != nil {
		s.logger.Warn("failed to decrypt user token", "user", body.Username, "error", err)
		http.Error(w, `{"error":"token decryption failed"}`, http.StatusInternalServerError)
		return
	}

	s.logger.Info("audit: user token issued", "user", body.Username, "hive", body.HiveID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
}

var publicPaths = []string{"/snapshot", "/leaderboard", "/contribute", "/api/leaderboard", "/api/contribute", ssoHandoffPath}

// ssoHandoffPath is the spoke's SSO handoff endpoint. It MUST bypass the hub's
// nginx auth_request gate.
//
// Why: the auth-check subrequest authorizes a user against THIS hive's grant
// list (user.Hives[hiveID]). The whole point of the handoff is to admit a user
// who is authenticated on the hub but has no hub-side grant row for the hive —
// the spoke's own authorized_users allowlist is the authority. Gating /sso on
// the hub check therefore 401s exactly the requests the handoff exists to
// serve, and nginx turns that 401 into an auth-signin redirect back to the hub
// login, which (the user already having a valid hub cookie) immediately
// redirects to /sso again — an infinite bounce the browser eventually aborts.
//
// This does NOT weaken authentication: the signed, hive-scoped, short-lived
// HMAC token in the query IS the credential, and dashboard.handleSSO verifies
// it against HIVE_HUB_SECRET plus the spoke's own allowlist before minting any
// session. It is the same reasoning that already makes /sso a public path on
// the spoke half (see dashboard.isPublicPath).
const ssoHandoffPath = "/sso"

// proxyAuthHeader is the response header the hub sets on a successful
// auth-check subrequest to PROVE to the spoke that the request was
// authenticated by the hub's nginx auth-proxy (not spoofed by a client that
// merely supplied X-Hive-User/X-Hive-Role directly). nginx is configured to
// copy this header onto the upstream request via the auth-response-headers
// annotation (see saas_provision.go). Its value is the spoke's own dashboard
// token, so the spoke can constant-time-compare it against its authToken.
//
// CONTRACT (spoke half, separate v3 PR must verify EXACTLY this):
//   - Header name: "X-Hive-Proxy-Auth"
//   - Value: the hive's dashboard token — the raw value stored in the spoke's
//     "hive-secrets" k8s secret under key "dashboard-token", i.e. the same
//     string the spoke reads from DASHBOARD_AUTH_TOKEN into its authToken.
//   - Set ONLY on the authenticated success path; NEVER on public-path,
//     unfurl-bot, unauthenticated (401), or no-access (403) responses.
const proxyAuthHeader = "X-Hive-Proxy-Auth"

// spokeProxyAuthCacheTTL bounds how long a hive's dashboard token is memoized
// so the per-request auth-check subrequest avoids a kubectl exec on every call
// while still picking up a re-provisioned token within a bounded window.
const spokeProxyAuthCacheTTL = 5 * time.Minute

// spokeProxyAuthEntry is a cached dashboard token with its expiry.
type spokeProxyAuthEntry struct {
	token   string
	expires time.Time
}

// spokeProxyAuthToken returns the given hive's dashboard token — the shared
// secret the spoke holds as its authToken (DASHBOARD_AUTH_TOKEN / the
// "dashboard-token" key of the "hive-secrets" k8s secret). It memoizes the
// value for spokeProxyAuthCacheTTL so the hot auth-check path does not exec
// kubectl on every proxied request. Returns "" if the token cannot be
// resolved (e.g. the hive isn't in the registry or its cluster is unreachable);
// callers must then omit the proof header rather than send an empty one.
func (s *HubServer) spokeProxyAuthToken(hiveID string) string {
	now := time.Now()

	s.spokeProxyAuthMu.Lock()
	if entry, ok := s.spokeProxyAuthCache[hiveID]; ok && now.Before(entry.expires) {
		tok := entry.token
		s.spokeProxyAuthMu.Unlock()
		return tok
	}
	s.spokeProxyAuthMu.Unlock()

	// Resolve the registry entry (ID + ClusterID) that loadSpokeAuthToken needs.
	var hive *RegistryEntry
	s.mu.RLock()
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == hiveID {
			h := s.registry.Hives[i]
			hive = &h
			break
		}
	}
	s.mu.RUnlock()
	if hive == nil {
		return ""
	}

	token := s.loadSpokeAuthToken(hive)
	if token == "" {
		return ""
	}

	s.spokeProxyAuthMu.Lock()
	s.spokeProxyAuthCache[hiveID] = spokeProxyAuthEntry{token: token, expires: now.Add(spokeProxyAuthCacheTTL)}
	s.spokeProxyAuthMu.Unlock()

	return token
}

// handleSaaSWhoami resolves the hub session for a sibling first-party product
// (#4171). Dibs (dibs.kubestellar.io) has no login of its own: it forwards the
// browser's hive_hub_user cookie here server-to-server and expects
//
//	200 {"username","display_name","email","avatar_url"}
//
// for a valid session, or 401 JSON otherwise.
//
// username is the STABLE identity key: the bare GitHub login for GitHub users
// (byte-identical to what every pre-multi-provider consumer keys on), or the
// hub's canonical "provider:sub" form for OIDC users — never a display name,
// never an email (emails are reassignable; subs are not). display_name/email/
// avatar_url come from the enriched SaaSUser record (DisplayName et al are
// refreshed on every completed login).
//
// Deliberately no CORS headers: the caller is a server, not a browser, and
// adding credentialed CORS here would hand the session identity to scripts.
// Cache-Control: no-store because the answer is per-session and revocable.
func (s *HubServer) handleSaaSWhoami(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	username := s.getAuthUser(r)
	if username == "" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"not authenticated"}`))
		return
	}
	user := loadSaaSUser(username)
	if user == nil {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"not authenticated"}`))
		return
	}
	// Bare login for GitHub identities, canonical provider:sub for the rest —
	// exactly the key SaaSUser records are stored and looked up under.
	stableKey := username
	if provider, subject, ok := parseCanonical(canonicalizeLegacy(username)); ok && provider == legacyProvider {
		stableKey = subject
	}
	displayLogin, avatar := s.displayIdentity(username)
	displayName := user.DisplayName
	if displayName == "" {
		// GitHub users have no provider-asserted name claim stored; the display
		// login (their bare GitHub login) is the established fallback.
		displayName = displayLogin
	}
	data, err := json.Marshal(map[string]string{
		"username":     stableKey,
		"display_name": displayName,
		"email":        user.Email,
		"avatar_url":   avatar,
	})
	if err != nil {
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(data)
}

// dibsRepoEntry is one hive-managed repo in the feed dibs's registry syncs
// from (GET /api/saas/dibs/repos, #4193). The field set and JSON names match
// dibs's pkg/registry RepoProfile contract exactly:
//
//	[{"repoID":"org/name","hiveID":"...","owner":"github-login","description":"..."}]
//
// Owner-editable dibs fields (topics, acceptingIdeas, appetite) are dibs-local
// state and deliberately absent here.
type dibsRepoEntry struct {
	RepoID      string `json:"repoID"`
	HiveID      string `json:"hiveID"`
	Owner       string `json:"owner"`
	Description string `json:"description,omitempty"`
	// ContributeURL is the hive's public /contribute page (ClankeR, the
	// contributor relay), built from the claimed vanity URL when present or
	// the heartbeat-reported dashboard URL otherwise (#4238). Empty when the
	// hive has reported no public base — dibs falls back to the hub's public
	// hive directory. Public info: the hub's own public-hives table renders
	// this same link to anonymous visitors.
	ContributeURL string `json:"contributeURL,omitempty"`
}

// handleDibsRepos lists the PUBLIC hive-managed repos for dibs's idea-matching
// registry (#4193, policy revised in #4233). Dibs polls this server-to-server
// with no browser session, so the endpoint is deliberately unauthenticated —
// which is safe only because every fact it returns is already public, and the
// inclusion rules below are what make that true. A hive contributes a repo
// only when ALL hold:
//
//   - the repo is PUBLIC: either the operator set is_public (the registry-
//     visibility opt-in, an immediate include with no API call), or the repo
//     is verifiably public on github.com right now — an unauthenticated
//     GET api.github.com/repos/{owner}/{repo} answering 200 with
//     "private": false. Verdicts are cached in memory with a TTL and checked
//     lazily in the background (dibs_public_check.go), so this handler never
//     blocks on the GitHub API: an unverified repo is excluded until its
//     verdict lands and the feed converges across dibs's 5-minute polls.
//     The opt-in-only policy this replaces could never populate the feed —
//     is_public is false on every production hive;
//   - it lives on PUBLIC github.com. github_host "" and "github.com" both
//     mean public GitHub (the sameGitHubHost normalization; production
//     records store the EXPLICIT "github.com" the spoke heartbeats, which the
//     original empty-only check wrongly excluded as GHE). A real GHE host is
//     excluded outright — an enterprise repo's very NAME can be confidential.
//     A cluster-level GHE default also excludes, unless the hive's own
//     github_host explicitly says github.com (the spoke-reported truth
//     outranks the cluster fallback);
//   - it is GitHub-family (not the gitlab/gitea forge adapters) and has a
//     real assigned identity: a non-placeholder org (the synthetic
//     "available-<id>" inventory org never names a repo) and at least one
//     repo recorded.
//
// owner is the hive owner's stable identity key — bare GitHub login for
// GitHub users, canonical provider:sub otherwise — byte-identical to the
// username /api/saas/whoami reports, so dibs can match a signed-in user to
// the repos they own.
//
// Cache-Control allows short shared caching: the answer is public, identical
// for every caller, and dibs re-syncs every ~5 minutes anyway.
func (s *HubServer) handleDibsRepos(w http.ResponseWriter, r *http.Request) {
	entries := []dibsRepoEntry{}
	seen := map[string]bool{}
	// Heartbeat-reported dashboard bases, snapshotted once so the hive loop
	// never holds the registry lock while doing per-repo work.
	dashByID := map[string]string{}
	s.mu.RLock()
	for i := range s.registry.Hives {
		if u := s.registry.Hives[i].DashboardURL; u != "" {
			dashByID[s.registry.Hives[i].ID] = u
		}
	}
	s.mu.RUnlock()
	for _, sh := range listSaaSHives() {
		// GitHub family only — the pkg/forge adapters (gitlab/gitea) never
		// point at github.com repos.
		if sh.Forge != "" && sh.Forge != "github" {
			continue
		}
		// "" and "github.com" both mean public GitHub; anything else is a
		// real GHE host and excludes the hive. Production meta.json records
		// carry the explicit "github.com" the spoke heartbeats (#4233).
		explicitPublicHost := sh.GitHubHost != "" && sameGitHubHost(sh.GitHubHost, publicGitHubHost)
		if sh.GitHubHost != "" && !explicitPublicHost {
			continue
		}
		// A GHE pin elsewhere in the resolution chain (hive-level
		// github_base_url, or the cluster default) also excludes — except
		// that an explicit github.com github_host outranks the CLUSTER
		// fallback: the host is spoke-reported truth, the cluster value only
		// a default for hives that never said.
		cluster := s.clusters[sh.ClusterID]
		if explicitPublicHost {
			cluster = ClusterConfig{}
		}
		if effectiveGitHubBaseURL(&sh, &cluster) != "" {
			continue
		}
		if sh.Org == "" || strings.HasPrefix(sh.Org, placeholderOrgPrefix) {
			continue
		}
		// Same stable-key normalization as handleSaaSWhoami, so the two
		// endpoints can never disagree about who a user is.
		owner := sh.Owner
		if provider, subject, ok := parseCanonical(canonicalizeLegacy(owner)); ok && provider == legacyProvider {
			owner = subject
		}
		// The hive's public contribute page: claimed vanity URL first (the
		// validated, user-facing host), else the heartbeat-reported dashboard
		// URL. Private/unset bases yield no link — same guard the hub's own
		// contribute proxy applies (findContributeHive).
		contributeURL := ""
		base := claimedVanityURL(&sh)
		if base == "" {
			base = dashByID[sh.ID]
		}
		if base != "" && !isPrivateURL(r.Context(), base) {
			contributeURL = strings.TrimRight(base, "/") + "/contribute"
		}
		for _, repo := range append([]string{sh.PrimaryRepo}, sh.Repos...) {
			if repo == "" {
				continue
			}
			// A primary_repo may already carry "owner/repo" (GHE/legacy
			// records do) — that pair IS the repo ID; otherwise the hive's
			// org is the owner half.
			repoID := repo
			if !strings.Contains(repo, "/") {
				repoID = sh.Org + "/" + repo
			}
			if seen[repoID] {
				continue
			}
			seen[repoID] = true
			// is_public stays an immediate include (the operator already
			// published the identity); everything else must be verifiably
			// public on github.com per the cached verdict (#4233). isPublic
			// never blocks — it answers from the cache and refreshes lazily.
			if !sh.IsPublic && !s.dibsChecker().isPublic(repoID) {
				continue
			}
			entries = append(entries, dibsRepoEntry{
				RepoID:        repoID,
				HiveID:        sh.ID,
				Owner:         owner,
				Description:   sh.ProjectName,
				ContributeURL: contributeURL,
			})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].RepoID < entries[j].RepoID })
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	if err := json.NewEncoder(w).Encode(entries); err != nil {
		s.logger.Warn("dibs repos: encoding response", "error", err)
	}
}

func (s *HubServer) handleSaaSAuthCheck(w http.ResponseWriter, r *http.Request) {
	hiveID := r.URL.Query().Get("hive")
	if hiveID == "" {
		http.Error(w, "missing hive param", http.StatusBadRequest)
		return
	}

	originalURI := r.Header.Get("X-Original-URI")
	if originalURI == "" {
		if origURL := r.Header.Get("X-Original-URL"); origURL != "" {
			if u, err := url.Parse(origURL); err == nil {
				originalURI = u.Path
			}
		}
	}
	if originalURI == "" {
		originalURI = r.URL.Query().Get("uri")
	}
	for _, p := range publicPaths {
		if strings.HasPrefix(originalURI, p) {
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	if isUnfurlBot(r.Header.Get("User-Agent")) {
		w.WriteHeader(http.StatusOK)
		return
	}

	username := s.getAuthUser(r)
	if username == "" {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}

	user := loadSaaSUser(username)
	if user == nil {
		http.Error(w, "no access", http.StatusForbidden)
		return
	}

	role, ok := user.Hives[hiveID]
	// The spoke enforces owner-gated actions (requireOwnerRole, e.g.
	// POST /api/self-upgrade) from X-Hive-Role alone, so a stale/demoted
	// stored role would lock the hive's TRUE owner out of their own spoke.
	// Elevate the canonical owner — scoped to their OWN hive — before
	// forwarding the role (#4081).
	if role != "owner" && s.userOwnsHive(username, hiveID) {
		role = "owner"
		ok = true
	}
	if !ok {
		http.Error(w, "no access to this hive", http.StatusForbidden)
		return
	}

	w.Header().Set("X-Hive-User", username)
	w.Header().Set("X-Hive-Role", role)
	// Prove to the spoke that this request really passed through the hub's
	// auth-proxy: set X-Hive-Proxy-Auth to the hive's own dashboard token so
	// the spoke can constant-time-compare it against its authToken. Only set on
	// this authenticated success path; a client hitting the spoke directly
	// cannot forge it because it never learns the token. If the token can't be
	// resolved, omit the header (backward-compatible: the spoke half must fail
	// open only until it is deployed — see the v3 spoke PR).
	if proxyAuth := s.spokeProxyAuthToken(hiveID); proxyAuth != "" {
		w.Header().Set(proxyAuthHeader, proxyAuth)
	}
	w.WriteHeader(http.StatusOK)
}

func isUnfurlBot(ua string) bool {
	bots := []string{"Slackbot", "Slack-ImgProxy", "Discordbot", "Twitterbot", "facebookexternalhit", "LinkedInBot", "WhatsApp", "TelegramBot"}
	for _, b := range bots {
		if strings.Contains(ua, b) {
			return true
		}
	}
	return false
}

const ogFallbackHTML = `<!DOCTYPE html><html><head>
<meta charset="utf-8">
<meta property="og:title" content="My Hives — Hive Hub">
<meta property="og:description" content="AI Agent Orchestration for Open Source. Manage your hive instances — monitor agents, governor mode, issues, PRs, and contributor activity.">
<meta property="og:type" content="website">
<meta property="og:site_name" content="Hive Hub">
<meta property="og:url" content="https://hive.kubestellar.io/dashboard">
<link rel="icon" href="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'><text y='.9em' font-size='90'>🍯</text></svg>">
<title>My Hives — Hive Hub</title>
</head><body></body></html>`

func (s *HubServer) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if isUnfurlBot(r.UserAgent()) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(ogFallbackHTML))
		return
	}
	cookie, err := r.Cookie("hive_hub_user")
	if err != nil || cookie.Value == "" {
		http.Redirect(w, r, "/login", http.StatusTemporaryRedirect)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprint(w, dashboardHTML)
}

func (s *HubServer) handleAccessDenied(w http.ResponseWriter, r *http.Request) {
	hiveID := sanitize(r.URL.Query().Get("hive"))

	ownerLink := ""
	s.mu.RLock()
	for _, h := range s.registry.Hives {
		if h.ID == hiveID && h.Owner != "" {
			safeOwner := sanitize(h.Owner)
			if safeOwner != "" {
				ownerLink = fmt.Sprintf(`<a href="https://github.com/%s" target="_blank" style="color:#58a6ff;text-decoration:underline">the hive owner</a>`, safeOwner)
			}
			break
		}
	}
	s.mu.RUnlock()
	if ownerLink == "" {
		ownerLink = "the hive owner"
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_, _ = fmt.Fprintf(w, `<!DOCTYPE html>
<html><head><meta charset="UTF-8"><title>Access Denied — Hive Hub</title>
<script async src="https://www.googletagmanager.com/gtag/js?id=G-4707R797K3"></script><script>window.dataLayer=window.dataLayer||[];function gtag(){dataLayer.push(arguments)}gtag("js",new Date());gtag("config","G-4707R797K3");gtag("event","access_denied",{hive_id:"%s"});</script>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:#0d1117;color:#e6edf3;display:flex;justify-content:center;align-items:center;min-height:100vh}
.card{background:#161b22;border:1px solid #30363d;border-radius:12px;padding:48px;max-width:520px;text-align:center}
h1{font-size:2rem;margin-bottom:8px}
.bee{font-size:3rem;margin-bottom:16px}
.msg{color:#8b949e;margin-bottom:24px;line-height:1.6}
.hive-name{color:#f0883e;font-family:monospace;font-weight:600}
.btn{display:inline-block;padding:10px 24px;border-radius:8px;text-decoration:none;font-weight:600;font-size:0.9rem;margin:6px}
.btn-primary{background:#238636;color:#fff}
.btn-secondary{background:transparent;color:#58a6ff;border:1px solid #30363d}
.help{color:#8b949e;font-size:0.8rem;margin-top:24px}
</style></head><body>
<div class="card">
<div class="bee">🐝</div>
<h1>Access Denied</h1>
<p class="msg">
You don't have access to
<span class="hive-name">%s</span>.<br><br>
Ask %s to grant you access from their
<a href="/dashboard" style="color:#58a6ff">My Hives</a> dashboard.
</p>
<a href="/dashboard" class="btn btn-primary">Go to My Hives</a>
<a href="/" class="btn btn-secondary">Browse Public Hives</a>
<p class="help">If you believe this is an error, <a href="https://github.com/hivecommons/hive/issues" style="color:#58a6ff">file an issue</a>.</p>
</div>
</body></html>`, hiveID, hiveID, ownerLink)
}


const (
	bannerIDPrefix       = "hub-banner-"
	maxBannerMessageLen  = 500
	maxBannerTargetHives = 100
)

var validBannerColors = map[string]bool{
	"green": true,
	"blue":  true,
	"amber": true,
	"gray":  true,
}

func (s *HubServer) handleSendHubBanner(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Message string   `json:"message"`
		Color   string   `json:"color"`
		HiveIDs []string `json:"hive_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	body.Message = strings.TrimSpace(body.Message)
	if body.Message == "" {
		http.Error(w, `{"error":"message is required"}`, http.StatusBadRequest)
		return
	}
	if len([]rune(body.Message)) > maxBannerMessageLen {
		http.Error(w, fmt.Sprintf(`{"error":"message exceeds %d characters"}`, maxBannerMessageLen), http.StatusBadRequest)
		return
	}
	if body.Color == "" {
		body.Color = "green"
	}
	if !validBannerColors[body.Color] {
		http.Error(w, `{"error":"invalid color; must be green, blue, amber, or gray"}`, http.StatusBadRequest)
		return
	}
	if len(body.HiveIDs) == 0 {
		http.Error(w, `{"error":"at least one hive must be selected"}`, http.StatusBadRequest)
		return
	}
	if len(body.HiveIDs) > maxBannerTargetHives {
		http.Error(w, fmt.Sprintf(`{"error":"too many hives (max %d)"}`, maxBannerTargetHives), http.StatusBadRequest)
		return
	}

	bannerID := fmt.Sprintf("%s%d", bannerIDPrefix, time.Now().UnixMilli())
	now := time.Now().UTC().Format(time.RFC3339)
	entry := &HubBannerEntry{
		ID:      bannerID,
		Message: body.Message,
		Color:   body.Color,
		SentAt:  now,
	}

	s.hubBannersMu.Lock()
	for _, hiveID := range body.HiveIDs {
		s.hubBanners[hiveID] = entry
	}
	s.hubBannersMu.Unlock()
	// Persist so the banner survives a hub restart/upgrade (the pod roll would
	// otherwise wipe the in-memory map and silently drop it).
	s.saveHubBanners()

	username := s.getAuthUser(r)
	s.logger.Info("hub banner sent",
		"banner_id", bannerID,
		"message", body.Message,
		"color", body.Color,
		"hive_count", len(body.HiveIDs),
		"by", username,
	)

	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"ok":true,"banner_id":%q,"hive_count":%d}`, bannerID, len(body.HiveIDs))
}

func (s *HubServer) handleClearHubBanner(w http.ResponseWriter, r *http.Request) {
	s.hubBannersMu.Lock()
	count := len(s.hubBanners)
	s.hubBanners = make(map[string]*HubBannerEntry)
	s.hubBannersMu.Unlock()
	// Persist the cleared (empty) state so banners stay gone across a restart.
	s.saveHubBanners()

	username := s.getAuthUser(r)
	s.logger.Info("hub banners cleared", "cleared_count", count, "by", username)

	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"ok":true,"cleared":%d}`, count)
}

func (s *HubServer) handleGetHubBanner(w http.ResponseWriter, r *http.Request) {
	s.hubBannersMu.RLock()
	defer s.hubBannersMu.RUnlock()

	type bannerStatus struct {
		HiveID  string `json:"hive_id"`
		ID      string `json:"id"`
		Message string `json:"message"`
		Color   string `json:"color"`
		SentAt  string `json:"sent_at"`
	}
	var banners []bannerStatus
	for hiveID, entry := range s.hubBanners {
		banners = append(banners, bannerStatus{
			HiveID:  hiveID,
			ID:      entry.ID,
			Message: entry.Message,
			Color:   entry.Color,
			SentAt:  entry.SentAt,
		})
	}
	if banners == nil {
		banners = []bannerStatus{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"banners": banners})
}
