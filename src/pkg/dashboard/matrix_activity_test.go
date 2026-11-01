package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/sentinel"
)

func matrixGitFixture(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init"}, {"config", "user.name", "Matrix fixture"}, {"config", "user.email", "matrix@example.invalid"}, {"remote", "add", "origin", "https://github.com/" + matrixTestRepo + ".git"}} {
		if _, err := matrixGit(context.Background(), dir, 8192, args...); err != nil {
			t.Fatalf("fixture Git %s: %v", args[0], err)
		}
	}
}

func matrixWriteFixture(t *testing.T, dir, path, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, path), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMatrixWorkspaceRealDiffsPreserveBothSidesAndFilterSecrets(t *testing.T) {
	dir := t.TempDir()
	matrixGitFixture(t, dir)
	matrixWriteFixture(t, dir, "source file.go", "func old() {\n\treturn false\n}\n")
	matrixWriteFixture(t, dir, "deleted.go", "old implementation\n")
	for _, args := range [][]string{{"add", "--", "source file.go", "deleted.go"}, {"commit", "-m", "fixture baseline"}} {
		if _, err := matrixGit(context.Background(), dir, 8192, args...); err != nil {
			t.Fatal(err)
		}
	}
	matrixWriteFixture(t, dir, "source file.go", "func newer() {\n\treturn true\n\n}\n")
	matrixWriteFixture(t, dir, "new.go", "new unpublished paragraph\n\nsecond line\n")
	matrixWriteFixture(t, dir, ".env", "API_KEY=not-for-the-display\n")
	matrixWriteFixture(t, dir, "unsafe.go", "safe-looking neighbour\npassword: hunter2\n")
	if err := os.Remove(filepath.Join(dir, "deleted.go")); err != nil {
		t.Fatal(err)
	}
	cfg := matrixTestConfig()
	cfg.Workspaces = true
	w := matrixWorkspace{dir: dir, agent: "quality", repo: matrixTestRepo}
	obs, digest, err := w.snapshot(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := newTestMatrixSource(t, cfg)
	m.ingest(obs)
	lines := matrixAllLines(m)
	var removed, added, fresh []string
	for _, line := range lines {
		if line.Stage != "in-progress" || line.Agent != "quality" || line.PR != 0 || line.Head == "" {
			t.Fatalf("unpublished changes have misleading provenance: %+v", line)
		}
		if strings.Contains(line.Line, "hunter2") || line.Path == ".env" || line.Path == "unsafe.go" {
			t.Fatalf("unsafe file or partial region reached the display: %+v", line)
		}
		if line.Path == "source file.go" && line.Change == "remove" {
			removed = append(removed, line.Line)
		}
		if line.Path == "source file.go" && line.Change == "add" {
			added = append(added, line.Line)
		}
		if line.Path == "new.go" {
			fresh = append(fresh, line.Line)
		}
	}
	if strings.Join(removed, "|") != "func old() {|\treturn false" || strings.Join(added, "|") != "func newer() {|\treturn true|" || strings.Join(fresh, "|") != "new unpublished paragraph||second line" {
		t.Fatalf("real diff paragraphs changed: removed=%q added=%q new=%q", removed, added, fresh)
	}
	foundDeletion := false
	for _, line := range lines {
		if line.Path == "deleted.go" && line.Change == "remove" && line.Line == "old implementation" {
			foundDeletion = true
		}
	}
	if !foundDeletion {
		t.Fatal("a deleted file disappeared instead of showing a removal")
	}
	_, again, err := w.snapshot(context.Background(), cfg)
	if err != nil || digest != again {
		t.Fatalf("unchanged workspace produced a different snapshot: %v", err)
	}
	if _, err := matrixGit(context.Background(), dir, 8192, "remote", "set-url", "origin", "https://github.com/private/other.git"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.snapshot(context.Background(), cfg); err == nil {
		t.Fatal("a reused checkout with a different origin was still captured")
	}
}

func TestMatrixWorkspaceDiscoveryRejectsUnmanagedAndSymlinkedCheckouts(t *testing.T) {
	root := t.TempDir()
	managed := filepath.Join(root, "quality", "repo")
	outside := t.TempDir()
	matrixGitFixture(t, managed)
	matrixGitFixture(t, outside)
	matrixGitFixture(t, filepath.Join(root, "unmanaged", "repo"))
	if err := os.Symlink(outside, filepath.Join(root, "quality", "linked-outside")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	found := matrixDiscoverWorkspaces(context.Background(), root, map[string]config.AgentConfig{"quality": {}}, matrixTestConfig())
	if len(found) != 1 || found[0].dir != managed || found[0].agent != "quality" {
		t.Fatalf("discovery escaped the managed workspace boundary: %+v", found)
	}
	if out, err := matrixGit(context.Background(), managed, 3, "config", "--get", "remote.origin.url"); !errors.Is(err, io.ErrShortBuffer) || out != nil {
		t.Fatalf("oversized Git output was partially admitted: bytes=%q err=%v", out, err)
	}
}

func TestMatrixWorkspaceVisibilityFailsClosed(t *testing.T) {
	for _, body := range []string{`{"private":true}`, `{}`, `{"private":false}`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			s := &Server{deps: &Dependencies{GHClient: github.NewClientForTest(server.URL, "hivecommons", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))}}
			if got := s.matrixWorkspacePublic(context.Background(), matrixTestRepo); got != (body == `{"private":false}`) {
				t.Fatalf("visibility admission = %v for %s", got, body)
			}
		})
	}
}

func TestMatrixWorkspaceConsentAndPolicyEpochForgetUnpublishedCode(t *testing.T) {
	cfg := matrixTestConfig()
	m, _ := newTestMatrixSource(t, cfg)
	obs := matrixObs(0, "head", sentinel.File{Path: "main.go", Patch: matrixPatch("unpublished code")})
	obs.stage, obs.agent, obs.workspace, obs.revision = "in-progress", "quality", "managed-workspace", "snapshot-one"
	if got := m.ingest(obs); got != 0 {
		t.Fatalf("workspace capture without separate consent admitted %d lines", got)
	}
	cfg.Workspaces = true
	m.setConfig(cfg)
	if got := m.ingest(obs); got != 1 {
		t.Fatalf("consented workspace did not deliver its paragraph: %d", got)
	}
	m.enqueue(obs)
	queued := <-m.in
	before := m.generation.Load()
	cfg.Workspaces = false
	m.setConfig(cfg)
	if m.generation.Load() == before || len(matrixAllLines(m)) != 0 || m.ingest(queued) != 0 {
		t.Fatal("revocation retained or resurrected unpublished code")
	}
}

func TestMatrixSourceByteBoundEvictsWholeParagraphs(t *testing.T) {
	m, _ := newTestMatrixSource(t, matrixTestConfig())
	for pr := 1; pr <= 40; pr++ {
		line := strings.Repeat("word ", 790) + " paragraph " + strings.Repeat("x ", pr)
		m.ingest(matrixObs(pr, "head", sentinel.File{Path: "long.md", Patch: matrixPatch(line, line, line, line)}))
	}
	lines := matrixAllLines(m)
	if m.bytes > matrixBacklogBytes || len(lines)%4 != 0 || len(lines) == 0 || lines[0].Seq <= 1 || (lines[0].Seq-1)%4 != 0 {
		t.Fatalf("byte eviction retained a fragment or exceeded its bound: bytes=%d rows=%d", m.bytes, len(lines))
	}
	for i := 0; i < len(lines); i += 4 {
		if lines[i].Block != lines[i+3].Block || lines[i].Line != lines[i+3].Line {
			t.Fatal("byte eviction altered a repeated-line paragraph")
		}
	}
}

func TestMatrixAPIPagesWholeBlocksWithoutLosingTheCursor(t *testing.T) {
	s := newTestServer()
	defer s.Close()
	s.MatrixObserver(matrixTestConfig())
	s.matrix.ingest(matrixObs(1, "head", sentinel.File{Path: "main.go", Patch: "@@\n-old\n+new\n+\n+}\n context\n+next paragraph\n"}))
	get := func(since uint64) matrixLinesResponse {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/matrix/lines?max=1&since="+strconv.FormatUint(since, 10), nil)
		req.Header.Set("X-Hive-Role", config.RoleRead)
		rr := httptest.NewRecorder()
		s.handleMatrixLines(rr, req)
		var page matrixLinesResponse
		if rr.Code != http.StatusOK {
			t.Fatalf("page failed: status=%d body=%s", rr.Code, rr.Body.String())
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		return page
	}
	first := get(0)
	if len(first.Lines) != 4 || first.Lines[0].Change != "remove" || first.Lines[3].Line != "}" || first.Seq != 4 || first.Latest != 5 {
		t.Fatalf("first page was split or skipped future content: %+v", first)
	}
	next := get(first.Seq)
	if len(next.Lines) != 1 || next.Lines[0].Line != "next paragraph" || next.Seq != 5 || next.Latest != 5 || next.Epoch != first.Epoch {
		t.Fatalf("continuation lost or repeated a paragraph: %+v", next)
	}
}

func TestMatrixNewFileReadCannotEscapeRepositoryRoot(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	matrixWriteFixture(t, outside, "source.go", "outside repository content\n")
	if err := os.Symlink(outside, filepath.Join(dir, "linked")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if patch, err := matrixUntrackedPatch(root, "linked/source.go", 4096); err == nil || patch != "" {
		t.Fatal("a symlinked parent escaped the repository capture boundary")
	}
	matrixWriteFixture(t, dir, "local.go", "  first paragraph\n\nlast line")
	patch, err := matrixUntrackedPatch(root, "local.go", 4096)
	if err != nil {
		t.Fatal(err)
	}
	blocks := matrixDiffBlocks(patch)
	if len(blocks) != 1 || len(blocks[0].lines) != 3 || blocks[0].lines[0].text != "  first paragraph" ||
		blocks[0].lines[1].text != "" || blocks[0].lines[2].text != "last line" {
		t.Fatalf("new-file capture changed its complete paragraph: %#v", blocks)
	}
	if patch, err := matrixUntrackedPatch(root, "local.go", 5); err == nil || patch != "" {
		t.Fatal("an oversized new file was truncated instead of omitted")
	}
}
