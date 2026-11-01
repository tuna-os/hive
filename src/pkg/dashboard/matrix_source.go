package dashboard

import (
	"crypto/sha256"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// Matrix line source bounds (#15934). Every buffer is fixed so a burst of
// pushes can cost memory and CPU only up to these limits.
const (
	matrixIngestCap       = 64
	matrixMaxLineRunes    = 4096
	matrixMaxBlockBytes   = 16 << 10
	matrixBacklogBytes    = 512 << 10
	matrixMaxLinesPerHead = 200
	matrixBacklogCap      = 2000
	matrixMaxTrackedPRs   = 500
	matrixDedupTTL        = 24 * time.Hour
	matrixRateWindow      = time.Minute
	matrixMaxLinesPerRate = 600
	matrixMaxFilesPerObs  = 300
	matrixMaxPatchBytes   = 512 << 10
	matrixDefaultSinceMax = 200
)

// matrixMaxDedupHashes caps dedup hashes across every tracked PR; a var so
// tests can exercise the bound without 100k lines.
var matrixMaxDedupHashes = 100000

// matrixLine is one scrub-clean changed line in an atomically admitted block.
type matrixLine struct {
	Seq    uint64    `json:"seq"`
	Block  uint64    `json:"block"`
	Change string    `json:"change"`
	Stage  string    `json:"stage"`
	Agent  string    `json:"agent,omitempty"`
	Repo   string    `json:"repo"`
	PR     int       `json:"pr"`
	Head   string    `json:"head"`
	Path   string    `json:"path"`
	Lang   string    `json:"lang,omitempty"`
	Line   string    `json:"line"`
	At     time.Time `json:"at"`
}

type matrixPatchKey struct {
	path string
	hash [sha256.Size]byte
}

type matrixPRState struct {
	head      string
	headCount int
	hashes    map[matrixPatchKey]time.Time
	touched   time.Time
}

type matrixFile struct {
	Path  string
	Patch string
}

// matrixObservation is the dashboard's source-independent diff snapshot.
// Workspace identity is internal, never a host path sent to the browser.
type matrixObservation struct {
	Repo           string
	Number         int
	HeadSHA        string
	Author         string
	HiveIdentified bool
	Private        bool
	Files          []matrixFile
	At             time.Time
	stage          string
	agent          string
	workspace      string
	revision       string
	epoch          uint64
}

// matrixSource turns published patches and managed-workspace snapshots into a
// bounded, scrubbed in-memory backlog. One goroutine parses admitted content.
// Nothing is written to disk.
type matrixSource struct {
	cfg        atomic.Pointer[config.DashboardMatrixConfig]
	in         chan matrixObservation
	dropped    atomic.Uint64
	generation atomic.Uint64
	logger     *slog.Logger
	now        func() time.Time

	startOnce     sync.Once
	stopOnce      sync.Once
	workspaceOnce sync.Once
	stop          chan struct{}
	done          chan struct{}

	mu          sync.Mutex
	ring        []matrixLine
	next        int
	count       int
	bytes       int
	seq         uint64
	epoch       uint64
	prs         map[string]*matrixPRState
	totalHashes int
	rateStart   time.Time
	rateCount   int
	rateDropped uint64
	notify      func(seq uint64)
}

func newMatrixSource(cfg config.DashboardMatrixConfig, logger *slog.Logger) *matrixSource {
	if logger == nil {
		logger = slog.Default()
	}
	m := &matrixSource{
		in:     make(chan matrixObservation, matrixIngestCap),
		logger: logger,
		now:    time.Now,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		ring:   make([]matrixLine, matrixBacklogCap),
		epoch:  1,
		prs:    make(map[string]*matrixPRState),
	}
	m.generation.Store(m.epoch)
	m.setConfig(cfg)
	return m
}

func (m *matrixSource) setConfig(cfg config.DashboardMatrixConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.cfg.Load()
	if old != nil && old.Enabled == cfg.Enabled && old.Workspaces == cfg.Workspaces &&
		slices.Equal(old.Repos, cfg.Repos) && slices.Equal(old.ExcludePaths, cfg.ExcludePaths) {
		return
	}
	c := cfg
	c.Repos = append([]string(nil), cfg.Repos...)
	c.ExcludePaths = append([]string(nil), cfg.ExcludePaths...)
	m.cfg.Store(&c)
	if old != nil {
		m.resetLocked()
	}
}

// admits applies the observation-level gates: Matrix active, hive-identified
// author, public repo on the explicit allowlist. There is no way to widen it.
func (m *matrixSource) admits(obs matrixObservation) bool {
	cfg := m.cfg.Load()
	return cfg != nil && cfg.Active() && obs.HiveIdentified && !obs.Private && cfg.RepoAllowed(obs.Repo) &&
		(obs.stage != "in-progress" || cfg.Workspaces)
}

// Observe is the sentinel PatchObserver. It gates the observation, then hands
// it off without blocking: when the ingest channel is full the observation
// is dropped and counted.
func (m *matrixSource) Observe(obs github.SentinelPatchObservation) {
	packet := matrixObservation{Repo: obs.Repo, Number: obs.Number, HeadSHA: obs.HeadSHA, Author: obs.Author,
		HiveIdentified: obs.HiveIdentified, Private: obs.Private, At: obs.At, stage: "pushed"}
	if !m.admits(packet) {
		return
	}
	files := obs.Files[:min(len(obs.Files), matrixMaxFilesPerObs)]
	packet.Files = make([]matrixFile, 0, len(files))
	bytes := 0
	for _, file := range files {
		if bytes+len(file.Patch) > matrixMaxPatchBytes {
			continue
		}
		bytes += len(file.Patch)
		packet.Files = append(packet.Files, matrixFile{Path: file.Path, Patch: file.Patch})
	}
	m.enqueue(packet)
}

func (m *matrixSource) enqueue(obs matrixObservation) {
	obs.epoch = m.generation.Load()
	if !m.admits(obs) {
		return
	}
	select {
	case m.in <- obs:
	default:
		m.dropped.Add(1)
	}
}

// Dropped counts observations Observe discarded because the channel was full.
func (m *matrixSource) Dropped() uint64 { return m.dropped.Load() }

func (m *matrixSource) start() {
	m.startOnce.Do(func() { go m.run() })
}

func (m *matrixSource) close() {
	m.stopOnce.Do(func() { close(m.stop) })
}

func (m *matrixSource) run() {
	defer close(m.done)
	for {
		select {
		case <-m.stop:
			return
		case obs := <-m.in:
			m.safeIngest(obs)
		}
	}
}

func (m *matrixSource) safeIngest(obs matrixObservation) {
	defer func() {
		if r := recover(); r != nil {
			m.logger.Warn("matrix line source dropped an observation after a panic", "repo", obs.Repo, "number", obs.Number, "panic", fmt.Sprint(r))
		}
	}()
	m.ingest(obs)
}

// ingest filters one observation into the backlog and returns how many lines
// it added.
func (m *matrixSource) ingest(obs matrixObservation) int {
	if !m.admits(obs) {
		return 0
	}
	now := m.now()
	at := obs.At
	if at.IsZero() {
		at = now
	}
	key := obs.Repo + "#" + strconv.Itoa(obs.Number) + ":" + obs.workspace
	revision := obs.HeadSHA
	if obs.stage == "in-progress" {
		revision = obs.revision
	}

	m.mu.Lock()
	if !m.admits(obs) || (obs.epoch != 0 && obs.epoch != m.epoch) {
		m.mu.Unlock()
		return 0
	}
	excludes := m.cfg.Load().ExcludePaths
	m.pruneLocked(now)
	st := m.prs[key]
	if st == nil {
		st = &matrixPRState{hashes: make(map[matrixPatchKey]time.Time)}
		m.prs[key] = st
	}
	st.touched = now
	if st.head != revision {
		if obs.stage == "in-progress" {
			m.totalHashes -= len(st.hashes)
			clear(st.hashes)
		}
		st.head = revision
		st.headCount = 0
	}
	added := 0
	files := obs.Files
	if len(files) > matrixMaxFilesPerObs {
		files = files[:matrixMaxFilesPerObs]
	}
	for _, f := range files {
		if matrixPathDenied(f.Path, excludes) || len(f.Patch) > matrixMaxPatchBytes {
			continue
		}
		lang := matrixLang(f.Path)
		for _, block := range matrixDiffBlocks(f.Patch) {
			if st.headCount+len(block.lines) > matrixMaxLinesPerHead {
				continue
			}
			key := matrixPatchKey{path: f.Path, hash: sha256.Sum256([]byte(block.raw))}
			if seen, dup := st.hashes[key]; dup && now.Sub(seen) < matrixDedupTTL {
				continue
			}
			if !m.rateAllowLocked(now, len(block.lines)) {
				continue
			}
			if _, dup := st.hashes[key]; !dup {
				m.totalHashes++
			}
			st.hashes[key] = now
			st.headCount += len(block.lines)
			m.reserveBlockLocked(len(block.lines), block.bytes)
			id := m.seq + 1
			for _, line := range block.lines {
				m.seq++
				// Do not retain the complete, possibly sensitive raw patch
				// through a substring of a small accepted line.
				m.appendLocked(matrixLine{Seq: m.seq, Block: id, Change: line.change, Stage: obs.stage, Agent: obs.agent,
					Repo: obs.Repo, PR: obs.Number, Head: obs.HeadSHA, Path: f.Path, Lang: lang, Line: strings.Clone(line.text), At: at})
				added++
			}
		}
	}
	m.evictLocked(key)
	seq, notify := m.seq, m.notify
	m.mu.Unlock()

	if added > 0 && notify != nil {
		notify(seq)
	}
	return added
}

// reserveBlockLocked evicts whole old blocks, never half a paragraph.
func (m *matrixSource) reserveBlockLocked(lines, bytes int) {
	for m.count > 0 && (m.count+lines > matrixBacklogCap || m.bytes+bytes > matrixBacklogBytes) {
		block := m.ring[m.next].Block
		for m.count > 0 && m.ring[m.next].Block == block {
			m.bytes -= len(m.ring[m.next].Line) + 1
			m.ring[m.next] = matrixLine{}
			m.next = (m.next + 1) % matrixBacklogCap
			m.count--
		}
	}
}

func (m *matrixSource) appendLocked(l matrixLine) {
	m.ring[(m.next+m.count)%matrixBacklogCap] = l
	m.count++
	m.bytes += len(l.Line) + 1
}

// rateAllowLocked admits a complete block only when it fits this window.
func (m *matrixSource) rateAllowLocked(now time.Time, lines int) bool {
	if now.Sub(m.rateStart) >= matrixRateWindow || now.Before(m.rateStart) {
		m.rateStart = now
		m.rateCount = 0
	}
	if m.rateCount+lines > matrixMaxLinesPerRate {
		m.rateDropped++
		return false
	}
	m.rateCount += lines
	return true
}

// pruneLocked forgets PR sets untouched for a full dedup TTL.
func (m *matrixSource) pruneLocked(now time.Time) {
	for key, st := range m.prs {
		if now.Sub(st.touched) >= matrixDedupTTL {
			m.removeLocked(key)
		}
	}
}

// evictLocked drops the least recently touched PR sets until both the PR
// count and the global hash count are within bounds. keep is the PR just
// ingested; it goes last.
func (m *matrixSource) evictLocked(keep string) {
	for len(m.prs) > matrixMaxTrackedPRs || m.totalHashes > matrixMaxDedupHashes {
		oldest := ""
		var oldestAt time.Time
		for key, st := range m.prs {
			if key == keep && len(m.prs) > 1 {
				continue
			}
			if oldest == "" || st.touched.Before(oldestAt) {
				oldest, oldestAt = key, st.touched
			}
		}
		if oldest == "" {
			return
		}
		m.removeLocked(oldest)
	}
}

func (m *matrixSource) removeLocked(key string) {
	if st, ok := m.prs[key]; ok {
		m.totalHashes -= len(st.hashes)
		delete(m.prs, key)
	}
}

// Since returns complete blocks newer than the cursor, oldest first, followed
// by the next cursor and latest sequence. Filtering precedes the page limit.
// A small limit still returns one whole block so a reader can make progress.
func (m *matrixSource) Since(seq uint64, limit int, allowed *config.DashboardMatrixConfig) ([]matrixLine, uint64, uint64, uint64) {
	if limit <= 0 {
		limit = matrixDefaultSinceMax
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if seq > m.seq {
		seq = 0
	}
	out := make([]matrixLine, 0, min(limit, m.count))
	cursor := seq
	for i := 0; i < m.count; {
		first := m.ring[(m.next+i)%matrixBacklogCap]
		end := i + 1
		for end < m.count && m.ring[(m.next+end)%matrixBacklogCap].Block == first.Block {
			end++
		}
		last := m.ring[(m.next+end-1)%matrixBacklogCap].Seq
		denied := allowed != nil && (!allowed.RepoAllowed(first.Repo) ||
			(first.Stage == "in-progress" && !allowed.Workspaces) || matrixPathDenied(first.Path, allowed.ExcludePaths))
		if last <= seq || first.Seq <= seq || denied {
			cursor = max(cursor, last)
			i = end
			continue
		}
		if len(out) > 0 && len(out)+end-i > limit {
			return out, cursor, m.seq, m.epoch
		}
		for ; i < end; i++ {
			out = append(out, m.ring[(m.next+i)%matrixBacklogCap])
		}
		cursor = last
		if len(out) >= limit {
			return out, cursor, m.seq, m.epoch
		}
	}
	return out, max(cursor, m.seq), m.seq, m.epoch
}

// Latest is the sequence number of the newest accepted line; 0 when none.
func (m *matrixSource) Latest() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seq
}

// reset forgets the backlog and dedup state; Seq keeps counting so a reader
// never sees a sequence number reused.
func (m *matrixSource) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resetLocked()
}

func (m *matrixSource) resetLocked() {
	clear(m.ring)
	m.count, m.bytes = 0, 0
	m.next = 0
	m.prs = make(map[string]*matrixPRState)
	m.totalHashes = 0
	m.epoch++
	m.generation.Store(m.epoch)
}

func (m *matrixSource) setNotify(fn func(seq uint64)) {
	m.mu.Lock()
	m.notify = fn
	m.mu.Unlock()
}

// MatrixObserver returns the sentinel PatchObserver feeding the Matrix line
// source, or nil when dashboard.matrix is not active — then nothing is
// created, and a source left over from an earlier
// active config stops admitting and forgets its backlog. The source is built on first use and
// later calls refresh its allowlist and exclude globs.
func (s *Server) MatrixObserver(cfg config.DashboardMatrixConfig) func(github.SentinelPatchObservation) {
	if s == nil {
		return nil
	}
	s.matrixMu.Lock()
	defer s.matrixMu.Unlock()
	if !cfg.Active() {
		if s.matrix != nil {
			s.matrix.setConfig(cfg)
		}
		return nil
	}
	if s.matrix == nil {
		s.matrix = newMatrixSource(cfg, s.logger)
		s.matrix.setNotify(s.nudgeMatrixLines)
		s.matrix.start()
	} else {
		s.matrix.setConfig(cfg)
		s.matrix.setNotify(s.nudgeMatrixLines)
	}
	return s.matrix.Observe
}
