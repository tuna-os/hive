package dashboard

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// startFakeTtydOnSocket stands in for ttyd on a unix socket and points
// HIVE_TTYD_SOCKET at it. HIVE_TTYD_PORT points at a port nothing listens on,
// so a request that reaches the fake at all went through the socket. It
// records the Authorization header of the last request.
func startFakeTtydOnSocket(t *testing.T) (lastAuth *string, hits *int) {
	t.Helper()
	// os.MkdirTemp, not t.TempDir: a unix socket path is limited to ~108 bytes.
	dir, err := os.MkdirTemp("", "ttyd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "ttyd.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen on %s: %v", sock, err)
	}
	var last string
	var n int
	upgrader := websocket.Upgrader{Subprotocols: []string{"tty"}}
	srv := &httptest.Server{
		Listener: ln,
		Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n++
			last = r.Header.Get("Authorization")
			if websocket.IsWebSocketUpgrade(r) {
				c, err := upgrader.Upgrade(w, r, nil)
				if err == nil {
					_ = c.Close()
				}
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ttyd-on-socket")
		})},
	}
	srv.Start()
	t.Cleanup(srv.Close)
	t.Setenv("HIVE_TTYD_SOCKET", sock)
	t.Setenv("HIVE_TTYD_PORT", "1")
	return &last, &n
}

// By default ttyd listens on a unix socket in a directory only the hive's
// user can enter, without a credential (a credential would be on its command
// line, readable by every user in the container). The Go dashboard's
// /terminal must reach it there, HTTP and websocket, and present no
// credential even when HIVE_TTYD_CREDENTIAL or a dashboard token is set.
func TestTerminalProxyUsesTtydSocket(t *testing.T) {
	t.Setenv("HIVE_DASHBOARD_TOKEN", "dash-tok-123")
	t.Setenv("HIVE_TTYD_CREDENTIAL", "ops:override-pass")
	last, hits := startFakeTtydOnSocket(t)
	h := newTerminalTestHandler(t, "")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/terminal/?arg=hive-quality", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ttyd-on-socket" {
		t.Fatalf("GET /terminal/: status %d body %q, want 200 from the ttyd on HIVE_TTYD_SOCKET", rec.Code, rec.Body.String())
	}
	if *last != "" {
		t.Fatalf("ttyd on the private socket was sent Authorization %q", *last)
	}

	front := httptest.NewServer(h)
	t.Cleanup(front.Close)
	wsURL := "ws" + strings.TrimPrefix(front.URL, "http") + "/terminal/ws?arg=hive-quality"
	dialer := websocket.Dialer{Subprotocols: []string{"tty"}}
	conn, resp, err := dialer.Dial(wsURL, nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("websocket dial through proxy: %v (status %d)", err, status)
	}
	_ = conn.Close()
	if *hits != 2 || *last != "" {
		t.Fatalf("upgrade: ttyd saw %d requests (want 2), Authorization %q", *hits, *last)
	}
}
