package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// localVerifier checks a candidate patch in a scratch copy of the checkout
// before hive-lite commits it: it writes every file the session has
// committed so far plus the candidate, then runs cmd. Run it only where
// executing model-authored code is acceptable (an ephemeral CI runner).
type localVerifier struct {
	checkout string
	cmd      string
	timeout  time.Duration

	mu        sync.Mutex
	committed map[string]string
}

func (v *localVerifier) Verify(ctx context.Context, files map[string]string) (string, bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	dir, err := os.MkdirTemp("", "hive-lite-verify-")
	if err != nil {
		return "", false, err
	}
	defer os.RemoveAll(dir)
	if err := copyTree(v.checkout, dir); err != nil {
		return "", false, err
	}
	all := map[string]string{}
	for p, c := range v.committed {
		all[p] = c
	}
	for p, c := range files {
		all[p] = c
	}
	for p, c := range all {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if !strings.HasPrefix(full, dir+string(os.PathSeparator)) {
			return "", false, fmt.Errorf("path %s escapes the checkout", p)
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return "", false, err
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			return "", false, err
		}
	}
	cctx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "sh", "-c", v.cmd)
	cmd.Dir = dir
	// No credentials reach model-authored code.
	cmd.Env = scrubbedEnv()
	out, runErr := cmd.CombinedOutput()
	report := tail(string(out), 4000)
	if runErr != nil {
		return fmt.Sprintf("$ %s\n%s\n(%v)", v.cmd, report, runErr), false, nil
	}
	if v.committed == nil {
		v.committed = map[string]string{}
	}
	for p, c := range files {
		v.committed[p] = c
	}
	return fmt.Sprintf("$ %s\n%s", v.cmd, report), true, nil
}

func scrubbedEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		u := strings.ToUpper(k)
		if strings.Contains(u, "TOKEN") || strings.Contains(u, "KEY") || strings.Contains(u, "SECRET") || strings.Contains(u, "GATEWAYS") || strings.HasPrefix(u, "GH_") || strings.HasPrefix(u, "GITHUB_") || strings.HasPrefix(u, "ACTIONS_") {
			continue
		}
		env = append(env, kv)
	}
	return env
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(os.PathSeparator)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}
