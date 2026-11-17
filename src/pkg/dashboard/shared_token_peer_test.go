package dashboard

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// withPeerUIDs points the peer lookup at a fixed port→uid table and the agent
// UID map at a temp file listing agentUID.
func withPeerUIDs(t *testing.T, ports map[int]int, agentUID int) {
	t.Helper()
	oldLookup, oldPaths := peerUIDForPort, agentUIDMapPaths
	t.Cleanup(func() { peerUIDForPort, agentUIDMapPaths = oldLookup, oldPaths })
	peerUIDForPort = func(port int) (int, error) {
		if uid, ok := ports[port]; ok {
			return uid, nil
		}
		return -1, errors.New("no socket")
	}
	path := filepath.Join(t.TempDir(), "uid-map.json")
	body := `{"agents":{"guide":` + strconv.Itoa(agentUID) + `},"proxy_uid":1001,"base_uid":2001}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	agentUIDMapPaths = func() []string { return []string{filepath.Join(t.TempDir(), "missing.json"), path} }
}

// The shared dashboard token makes a request the verified owner. From a
// loopback peer whose socket an agent's user owns, it must not: an agent that
// obtains the token by any route would otherwise control every other agent.
// The same request from the hive's own user (the Node proxy, the operator's
// exec) keeps the owner role.
func TestSharedTokenRefusedFromAgentUser(t *testing.T) {
	const token = "dash-secret"
	withPeerUIDs(t, map[int]int{50000: 2005, 50001: 1001}, 2005)

	s := NewServerWithAuth(0, token, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var gotRole string
	h := s.authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRole = r.Header.Get("X-Hive-Role")
		w.WriteHeader(http.StatusOK)
	}))

	for _, tc := range []struct {
		name   string
		remote string
		header string
		value  string
		owner  bool
	}{
		{"X-Hive-Internal from an agent", "127.0.0.1:50000", "X-Hive-Internal", token, false},
		{"Bearer from an agent", "127.0.0.1:50000", "Authorization", "Bearer " + token, false},
		{"X-Hive-Internal from an agent over IPv6 loopback", "[::1]:50000", "X-Hive-Internal", token, false},
		{"X-Hive-Internal from the hive user", "127.0.0.1:50001", "X-Hive-Internal", token, true},
		{"Bearer from the hive user", "127.0.0.1:50001", "Authorization", "Bearer " + token, true},
		{"owner unknown fails open", "127.0.0.1:50002", "X-Hive-Internal", token, true},
		{"non-loopback is not looked up", "10.1.2.3:50000", "X-Hive-Internal", token, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotRole = ""
			req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
			req.RemoteAddr = tc.remote
			req.Header.Set(tc.header, tc.value)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			isOwner := rec.Code == http.StatusOK && gotRole == "owner"
			if isOwner != tc.owner {
				t.Fatalf("status %d role %q: owner=%v, want %v", rec.Code, gotRole, isOwner, tc.owner)
			}
		})
	}
}

// The hub's X-Hive-Proxy-Auth proof is the same token; an agent must not be
// able to forge hub identity headers with it either.
func TestProxyProofRefusedFromAgentUser(t *testing.T) {
	const token = "dash-secret"
	withPeerUIDs(t, map[int]int{50000: 2005}, 2005)
	s := NewServerWithAuth(0, token, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var gotUser string
	h := s.authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser = r.Header.Get("X-Hive-User")
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("X-Hive-User", "forged-owner")
	req.Header.Set("X-Hive-Role", "owner")
	req.Header.Set(proxyAuthHeader, token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK && gotUser == "forged-owner" {
		t.Fatal("an agent user forged hub identity with the dashboard token as proxy proof")
	}
}
