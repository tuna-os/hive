package dashboard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/sentinel"
)

const matrixWorkspaceCap = 64

type matrixWorkspace struct {
	dir, agent, repo string
}

type matrixVisibility struct {
	public bool
	until  time.Time
}

// watchMatrixWorkspaces samples changes against HEAD, not terminal output or
// keystrokes. Discovery never follows symlinks or leaves a configured agent's
// managed directory. At most two workspaces are sampled per two-second tick.
func (s *Server) watchMatrixWorkspaces(m *matrixSource) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-m.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var workspaces []matrixWorkspace
	last := make(map[string][sha256.Size]byte)
	visibility := make(map[string]matrixVisibility)
	var discoverAt time.Time
	var epoch uint64
	next := 0
	for {
		cfg := s.currentMatrixConfig()
		m.setConfig(cfg)
		if current := m.generation.Load(); current != epoch {
			clear(last)
			clear(visibility)
			discoverAt = time.Time{}
			epoch = current
		}
		if cfg.Active() && cfg.Workspaces && s.deps != nil && s.deps.Config != nil {
			now := time.Now()
			if !now.Before(discoverAt) {
				workspaces = matrixDiscoverWorkspaces(ctx, crux.WorkDir(), s.deps.Config.Agents, cfg)
				live := make(map[string]bool, len(workspaces))
				for _, w := range workspaces {
					live[w.dir] = true
				}
				for dir := range last {
					if !live[dir] {
						delete(last, dir)
					}
				}
				for repo := range visibility {
					if !cfg.RepoAllowed(repo) {
						delete(visibility, repo)
					}
				}
				discoverAt = now.Add(30 * time.Second)
			}
			for range min(2, len(workspaces)) {
				w := workspaces[next%len(workspaces)]
				next++
				proof := visibility[w.repo]
				if !now.Before(proof.until) {
					wasPublic := proof.public
					proof = matrixVisibility{public: s.matrixWorkspacePublic(ctx, w.repo), until: now.Add(5 * time.Minute)}
					visibility[w.repo] = proof
					if wasPublic && !proof.public {
						m.reset()
					}
				}
				if !proof.public || !cfg.RepoAllowed(w.repo) {
					continue
				}
				obs, digest, err := w.snapshot(ctx, cfg)
				if err != nil {
					continue
				}
				if prior, ok := last[w.dir]; ok && prior == digest {
					continue
				}
				// A cached discovery proof never authorizes publishing new
				// unpublished text: verify every changed snapshot again.
				if len(obs.Files) > 0 && !s.matrixWorkspacePublic(ctx, w.repo) {
					visibility[w.repo] = matrixVisibility{until: now.Add(5 * time.Minute)}
					m.reset()
					continue
				}
				last[w.dir] = digest
				if len(obs.Files) > 0 {
					m.enqueue(obs)
				}
			}
		} else {
			workspaces = nil
			clear(last)
			clear(visibility)
			discoverAt = time.Time{}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// matrixDiscoverWorkspaces only inspects directory and Git-origin metadata.
// Code is not read until a separate authoritative public-repository check.
func matrixDiscoverWorkspaces(ctx context.Context, root string, agents map[string]config.AgentConfig, cfg config.DashboardMatrixConfig) []matrixWorkspace {
	var err error
	root, err = filepath.Abs(root)
	if err != nil {
		return nil
	}
	var out []matrixWorkspace
	entries, _ := os.ReadDir(root)
	for _, entry := range entries {
		if _, managed := agents[entry.Name()]; !managed || !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		agentRoot := filepath.Join(root, entry.Name())
		visited := 0
		_ = filepath.WalkDir(agentRoot, func(p string, d fs.DirEntry, err error) error {
			visited++
			if len(out) >= matrixWorkspaceCap || visited > 4096 || ctx.Err() != nil {
				return fs.SkipAll
			}
			if err != nil || !d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(agentRoot, p)
			if p != agentRoot && (strings.HasPrefix(d.Name(), ".") || strings.Count(rel, string(filepath.Separator)) >= 5 || d.Name() == "node_modules" || d.Name() == "vendor") {
				return filepath.SkipDir
			}
			git, err := os.Lstat(filepath.Join(p, ".git"))
			if err != nil || (!git.IsDir() && !git.Mode().IsRegular()) {
				return nil
			}
			origin, err := matrixGit(ctx, p, 8192, "config", "--get", "remote.origin.url")
			if err == nil {
				repo := matrixOriginRepo(strings.TrimSpace(string(origin)))
				if cfg.RepoAllowed(repo) {
					out = append(out, matrixWorkspace{dir: p, agent: entry.Name(), repo: repo})
				}
			}
			return filepath.SkipDir
		})
		if len(out) >= matrixWorkspaceCap {
			break
		}
	}
	return out
}

func matrixOriginRepo(origin string) string {
	var repo string
	if strings.HasPrefix(origin, "git@github.com:") {
		repo = strings.TrimPrefix(origin, "git@github.com:")
	} else if u, err := url.Parse(origin); err == nil && strings.EqualFold(u.Hostname(), "github.com") && (u.Scheme == "https" || u.Scheme == "ssh") {
		repo = strings.TrimPrefix(u.Path, "/")
	}
	repo = strings.TrimSuffix(repo, ".git")
	if (config.DashboardMatrixConfig{Repos: []string{repo}}).Validate() != nil {
		return ""
	}
	return strings.Clone(strings.ToLower(repo))
}

func matrixWorkspacePath(p string, cfg config.DashboardMatrixConfig) bool {
	if p == "" || !utf8.ValidString(p) || filepath.IsAbs(p) || filepath.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") || matrixPathDenied(p, cfg.ExcludePaths) {
		return false
	}
	for _, r := range p {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (w matrixWorkspace) snapshot(ctx context.Context, cfg config.DashboardMatrixConfig) (matrixObservation, [sha256.Size]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	obs := matrixObservation{
		SentinelPatchObservation: github.SentinelPatchObservation{Repo: w.repo, HiveIdentified: true, At: time.Now()},
		stage:                    "in-progress", agent: w.agent, workspace: w.dir,
	}
	// Revalidate origin on every sample: a reused checkout may have changed
	// repositories since discovery or its public-visibility proof.
	origin, err := matrixGit(ctx, w.dir, 8192, "config", "--get", "remote.origin.url")
	if err != nil || matrixOriginRepo(strings.TrimSpace(string(origin))) != w.repo || !cfg.RepoAllowed(w.repo) {
		return obs, [sha256.Size]byte{}, errors.New("workspace origin no longer admitted")
	}
	head, headErr := matrixGit(ctx, w.dir, 256, "rev-parse", "--verify", "HEAD")
	obs.HeadSHA = strings.TrimSpace(string(head))
	var tracked []byte
	if headErr == nil {
		tracked, err = matrixGit(ctx, w.dir, 64<<10, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--ignore-submodules=all", "--name-only", "-z", "HEAD", "--")
		if err != nil {
			return obs, [sha256.Size]byte{}, err
		}
	} else {
		tracked, err = matrixGit(ctx, w.dir, 64<<10, "ls-files", "--cached", "-z")
		if err != nil {
			return obs, [sha256.Size]byte{}, err
		}
	}
	var paths []string
	for p := range strings.SplitSeq(string(tracked), "\x00") {
		if matrixWorkspacePath(p, cfg) && len(paths) < matrixMaxFilesPerObs {
			paths = append(paths, p)
		}
	}
	remaining := matrixMaxPatchBytes
	if headErr == nil && len(paths) > 0 {
		args := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-color", "--no-renames", "--ignore-submodules=all", "--unified=0", "--src-prefix=a/", "--dst-prefix=b/", "--output-indicator-new=+", "--output-indicator-old=-", "--output-indicator-context= ", "HEAD", "--"}
		patch, err := matrixGit(ctx, w.dir, remaining, append(args, paths...)...)
		if err != nil {
			return obs, [sha256.Size]byte{}, err
		}
		obs.Files = matrixWorkspaceFiles(string(patch), cfg)
		remaining -= len(patch)
		paths = nil
	}
	untracked, err := matrixGit(ctx, w.dir, 64<<10, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return obs, [sha256.Size]byte{}, err
	}
	for p := range strings.SplitSeq(string(untracked), "\x00") {
		if matrixWorkspacePath(p, cfg) && len(paths)+len(obs.Files) < matrixMaxFilesPerObs {
			paths = append(paths, p)
		}
	}
	if len(paths) > 0 {
		root, err := os.OpenRoot(w.dir)
		if err != nil {
			return obs, [sha256.Size]byte{}, err
		}
		defer root.Close()
		for _, p := range paths {
			patch, err := matrixUntrackedPatch(root, p, remaining)
			if err != nil || patch == "" {
				continue
			}
			remaining -= len(patch)
			obs.Files = append(obs.Files, sentinel.File{Path: p, Patch: patch})
		}
	}
	hash := sha256.New()
	for _, f := range obs.Files {
		_, _ = io.WriteString(hash, f.Path)
		_, _ = hash.Write([]byte{0})
		_, _ = io.WriteString(hash, f.Patch)
	}
	var digest [sha256.Size]byte
	hash.Sum(digest[:0])
	obs.revision = hex.EncodeToString(digest[:])
	return obs, digest, nil
}

// matrixWorkspaceFiles reads filenames only before a file's first hunk.
// Header-like source text inside a hunk is never interpreted as metadata.
func matrixWorkspaceFiles(patch string, cfg config.DashboardMatrixConfig) []sentinel.File {
	var files []sentinel.File
	var start int
	flush := func(end int) {
		part := patch[start:end]
		old, name := "", ""
		for line := range strings.SplitSeq(part, "\n") {
			if strings.HasPrefix(line, "@@ ") {
				if name == "/dev/null" {
					name = old
				}
				if matrixWorkspacePath(name, cfg) {
					files = append(files, sentinel.File{Path: strings.Clone(name), Patch: part})
				}
				return
			}
			if strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") {
				p := strings.TrimSuffix(line[4:], "\t")
				if strings.HasPrefix(p, "\"") {
					decoded, err := strconv.Unquote(p)
					if err != nil {
						return
					}
					p = decoded
				}
				if strings.HasPrefix(line, "--- ") {
					old = strings.TrimPrefix(p, "a/")
				} else {
					name = strings.TrimPrefix(p, "b/")
				}
			}
		}
	}
	offset := 0
	for line := range strings.SplitSeq(patch, "\n") {
		if strings.HasPrefix(line, "diff --git ") && offset > start {
			flush(offset)
			start = offset
		}
		offset = min(len(patch), offset+len(line)+1)
	}
	flush(len(patch))
	return files
}

type matrixGitBuffer struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (b *matrixGitBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.buf.Len() {
		b.overflow = true
		return 0, io.ErrShortBuffer
	}
	return b.buf.Write(p)
}

// Git is read-only, output-bounded, time-bounded and cannot invoke configured
// external diff drivers, text converters, pagers or filesystem monitors.
func matrixGit(parent context.Context, dir string, limit int, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	base := []string{"--no-pager", "--literal-pathspecs", "--work-tree=" + dir, "-c", "safe.directory=" + dir, "-c", "core.fsmonitor=false", "-c", "core.quotePath=false"}
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0"}
	out := &matrixGitBuffer{limit: limit}
	cmd.Stdout, cmd.Stderr = out, io.Discard
	err := cmd.Run()
	if out.overflow {
		return nil, io.ErrShortBuffer
	}
	return out.buf.Bytes(), err
}

func (s *Server) matrixWorkspacePublic(parent context.Context, repo string) bool {
	if s.deps == nil || s.deps.GHClient == nil {
		return false
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return false
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	r, _, err := s.deps.GHClient.GetRepo(ctx, owner, name)
	return err == nil && r != nil && r.Private != nil && !r.GetPrivate()
}

// Rooted reads prevent a renamed parent or symlink race from turning a new
// source file into an arbitrary host-file read. New files are all additions.
func matrixUntrackedPatch(root *os.Root, path string, limit int) (string, error) {
	info, err := root.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > int64(limit) {
		return "", errors.New("new file is not a bounded regular file")
	}
	f, err := root.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if info, err = f.Stat(); err != nil || !info.Mode().IsRegular() {
		return "", errors.New("new file changed type")
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", nil
	}
	if len(data) > limit || !utf8.Valid(data) || bytes.ContainsRune(data, 0) {
		return "", errors.New("new file is oversized or binary")
	}
	lines := bytes.Count(data, []byte{'\n'})
	trailing := data[len(data)-1] == '\n'
	if !trailing {
		lines++
	}
	// Include prefix, hunk header and no-newline metadata in the byte budget.
	if len(data)+lines+64 > limit {
		return "", io.ErrShortBuffer
	}
	var patch strings.Builder
	patch.Grow(len(data) + lines + 64)
	patch.WriteString("@@ -0,0 +1,")
	var number [20]byte
	patch.Write(strconv.AppendInt(number[:0], int64(lines), 10))
	patch.WriteString(" @@\n")
	offset := 0
	for line := range bytes.SplitSeq(data, []byte{'\n'}) {
		if offset == len(data) && len(line) == 0 {
			break
		}
		patch.WriteByte('+')
		patch.Write(line)
		patch.WriteByte('\n')
		offset += len(line) + 1
	}
	if !trailing {
		patch.WriteString("\\ No newline at end of file\n")
	}
	return patch.String(), nil
}
