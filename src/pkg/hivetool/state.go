package hivetool

// State is the executor memory a resumed session needs: without it, the
// evidence gate would reject citations of code read in an earlier pass and
// the duplicate-call guard would let a resumed model repeat its reads.
// It holds no credential and no canary.
type State struct {
	Seen     map[string]string   `json:"seen,omitempty"`     // call fingerprint -> first call id
	Evidence map[string][][2]int `json:"evidence,omitempty"` // path -> served [from, to] spans
	Outcome  *Outcome            `json:"outcome,omitempty"`
}

// Snapshot captures the executor's session memory.
func (x *Executor) Snapshot() State {
	x.mu.Lock()
	defer x.mu.Unlock()
	s := State{}
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
	x.outcome = nil
	if s.Outcome != nil {
		o := *s.Outcome
		x.outcome = &o
	}
}
