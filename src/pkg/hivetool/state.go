package hivetool

// State is the executor memory a resumed session needs: without it, the
// evidence gate would reject citations of code read in an earlier pass and
// the duplicate-call guard would let a resumed model repeat its reads.
// It holds no credential and no canary.
type State struct {
	Seen     map[string]string   `json:"seen,omitempty"`     // call fingerprint -> first call id
	Evidence map[string][][2]int `json:"evidence,omitempty"` // path -> served [from, to] spans
	HeadSHA  string              `json:"head_sha,omitempty"`
	PRNumber int                 `json:"pr_number,omitempty"`
	Verified []VerifyRecord      `json:"verified,omitempty"`
	Outcome  *Outcome            `json:"outcome,omitempty"`
}

// VerifyRecord is one Verifier report and the commit it was taken for.
type VerifyRecord struct {
	SHA    string `json:"sha,omitempty"`
	Report string `json:"report"`
}

// Snapshot captures the executor's session memory.
func (x *Executor) Snapshot() State {
	x.mu.Lock()
	defer x.mu.Unlock()
	s := State{HeadSHA: x.headSHA, PRNumber: x.prNumber}
	if len(x.seen) > 0 {
		s.Seen = make(map[string]string, len(x.seen))
		for k, v := range x.seen {
			s.Seen[k] = v
		}
	}
	if len(x.evidence) > 0 {
		s.Evidence = make(map[string][][2]int, len(x.evidence))
		for p, spans := range x.evidence {
			for _, sp := range spans {
				s.Evidence[p] = append(s.Evidence[p], [2]int{sp.from, sp.to})
			}
		}
	}
	for _, v := range x.verified {
		s.Verified = append(s.Verified, VerifyRecord{SHA: v.sha, Report: v.report})
	}
	if x.outcome != nil {
		o := *x.outcome
		s.Outcome = &o
	}
	return s
}

// Restore replaces the executor's session memory with s. Call it before the
// first Execute of a resumed session.
func (x *Executor) Restore(s State) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.seen = map[string]string{}
	for k, v := range s.Seen {
		x.seen[k] = v
	}
	x.evidence = map[string][]span{}
	for p, spans := range s.Evidence {
		for _, sp := range spans {
			x.evidence[p] = append(x.evidence[p], span{sp[0], sp[1]})
		}
	}
	x.headSHA, x.prNumber = s.HeadSHA, s.PRNumber
	x.verified = nil
	for _, v := range s.Verified {
		x.verified = append(x.verified, verifyRecord{sha: v.SHA, report: v.Report})
	}
	x.outcome = nil
	if s.Outcome != nil {
		o := *s.Outcome
		x.outcome = &o
	}
}
