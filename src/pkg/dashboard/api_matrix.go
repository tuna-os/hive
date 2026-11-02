package dashboard

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// matrixLinesNudgeInterval is the minimum spacing between `code-lines` SSE frames.
const matrixLinesNudgeInterval = time.Second

const (
	matrixMinSinceMax = 1
	matrixMaxSinceMax = 500
)

func matrixCodeLinesSSEFrame(seq uint64) string {
	return "event: code-lines\ndata: {\"seq\":" + strconv.FormatUint(seq, 10) + "}\n\n"
}

// nudgeMatrixLines broadcasts the coalesced `code-lines` SSE frame.
func (s *Server) nudgeMatrixLines(seq uint64) {
	if s == nil {
		return
	}
	cfg := s.currentMatrixConfig()
	if !cfg.Active() {
		return
	}
	s.matrixNudge.nudge(seq, func(seq uint64) {
		if s.currentMatrixConfig().Active() {
			s.broadcastFrame(matrixCodeLinesSSEFrame(seq))
		}
	})
}

// currentMatrixConfig returns the effective MatrixConfig.
func (s *Server) currentMatrixConfig() config.DashboardMatrixConfig {
	if s == nil {
		return config.DashboardMatrixConfig{}
	}
	if s.deps != nil && s.deps.Config != nil {
		return s.deps.Config.Dashboard.Matrix
	}
	s.matrixMu.Lock()
	defer s.matrixMu.Unlock()
	if s.matrix != nil {
		if cfg := s.matrix.cfg.Load(); cfg != nil {
			return *cfg
		}
	}
	return config.DashboardMatrixConfig{}
}

// matrixActiveSource starts an enabled source independently of sentinel sweeps,
// so workspace edits do not wait for a PR push or a GitHub polling cycle.
func (s *Server) matrixActiveSource() (*matrixSource, *config.DashboardMatrixConfig) {
	if s == nil {
		return nil, nil
	}
	cfg := s.currentMatrixConfig()
	if !cfg.Active() {
		return nil, nil
	}
	s.MatrixObserver(cfg)
	s.matrixMu.Lock()
	m := s.matrix
	s.matrixMu.Unlock()
	if s.deps != nil && s.deps.AgentMgr != nil {
		m.workspaceOnce.Do(func() { go s.watchMatrixWorkspaces(m) })
	}
	return m, &cfg
}

type matrixLinesResponse struct {
	Seq        uint64       `json:"seq"`
	Latest     uint64       `json:"latest"`
	Epoch      uint64       `json:"epoch"`
	Source     string       `json:"source"`
	Workspaces bool         `json:"workspaces"`
	Lines      []matrixLine `json:"lines"`
}

// handleMatrixLines serves GET /api/matrix/lines?since=<seq>&max=<n>
func (s *Server) handleMatrixLines(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !config.RoleAtLeast(r.Header.Get("X-Hive-Role"), config.RoleRead) {
		jsonError(w, "read access required", http.StatusForbidden)
		return
	}

	src, cfg := s.matrixActiveSource()
	if src == nil || cfg == nil || !cfg.Active() {
		jsonError(w, "matrix mode disabled", http.StatusNotFound)
		return
	}

	var since uint64
	if sinceStr := r.URL.Query().Get("since"); sinceStr != "" {
		if val, err := strconv.ParseUint(sinceStr, 10, 64); err == nil {
			since = val
		}
	}

	limit := matrixDefaultSinceMax // 200
	if r.URL.Query().Has("max") {
		n, err := strconv.Atoi(r.URL.Query().Get("max"))
		if err != nil || n < matrixMinSinceMax {
			limit = matrixMinSinceMax
		} else if n > matrixMaxSinceMax {
			limit = matrixMaxSinceMax
		} else {
			limit = n
		}
	} else if since == 0 {
		limit = matrixBacklogCap // 2000
	}

	lines, cursor, latest, epoch := src.Since(since, limit, cfg)

	w.Header().Set("Content-Type", "application/json")
	resp := matrixLinesResponse{
		Seq:        cursor,
		Latest:     latest,
		Epoch:      epoch,
		Source:     "code-activity",
		Workspaces: cfg.Workspaces,
		Lines:      lines,
	}
	_ = json.NewEncoder(w).Encode(resp)
}
