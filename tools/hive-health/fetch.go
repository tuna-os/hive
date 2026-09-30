package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const userAgent = "tuna-os-hive-health-probe/1 (+https://github.com/tuna-os/hive/tree/v4/tools/hive-health)"

// maxBody bounds any response the probe reads (/api/status is ~2.5 MB on the
// busiest Hive).
const maxBody = 16 << 20

// Fetched is one GET's outcome.
type Fetched struct {
	URL     string        `json:"url"`
	Status  int           `json:"status"`
	Err     string        `json:"error,omitempty"`
	Latency time.Duration `json:"-"`
	Body    []byte        `json:"-"`
}

// OK reports a 2xx with no transport error.
func (f Fetched) OK() bool { return f.Err == "" && f.Status >= 200 && f.Status < 300 }

// Describe is a one-line, secret-free description of the outcome.
func (f Fetched) Describe() string {
	if f.Err != "" {
		return fmt.Sprintf("GET %s: %s", f.URL, f.Err)
	}
	return fmt.Sprintf("GET %s: HTTP %d (%s)", f.URL, f.Status, f.Latency.Round(time.Millisecond))
}

// Getter performs a GET. Every request the probe makes goes through this
// interface, which has no way to express any method but GET: the probe
// cannot mutate a Hive.
type Getter interface {
	Get(ctx context.Context, rawURL string, headers map[string]string) Fetched
}

// HTTPGetter is the real Getter.
type HTTPGetter struct {
	Client  *http.Client
	Retries int
	Backoff time.Duration
}

// NewHTTPGetter returns a Getter with sane timeouts. Redirects are not
// followed across hosts, so a session cookie can never be replayed to a
// host it was not configured for.
func NewHTTPGetter(timeout time.Duration) *HTTPGetter {
	return &HTTPGetter{
		Client: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) > 0 && req.URL.Host != via[0].URL.Host {
					return errors.New("refusing cross-host redirect")
				}
				if len(via) >= 3 {
					return errors.New("too many redirects")
				}
				return nil
			},
		},
		Retries: 2,
		Backoff: 3 * time.Second,
	}
}

// Get implements Getter with retries on transport errors and 5xx.
func (g *HTTPGetter) Get(ctx context.Context, rawURL string, headers map[string]string) Fetched {
	var last Fetched
	for attempt := 0; attempt <= g.Retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return last
			case <-time.After(g.Backoff * time.Duration(attempt)):
			}
		}
		last = g.once(ctx, rawURL, headers)
		if last.Err == "" && last.Status < 500 {
			return last
		}
	}
	return last
}

func (g *HTTPGetter) once(ctx context.Context, rawURL string, headers map[string]string) Fetched {
	f := Fetched{URL: rawURL}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		f.Err = err.Error()
		return f
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := g.Client.Do(req)
	f.Latency = time.Since(start)
	if err != nil {
		f.Err = scrubURLError(err)
		return f
	}
	defer resp.Body.Close()
	f.Status = resp.StatusCode
	f.Body, err = io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		f.Err = "reading body: " + err.Error()
	}
	return f
}

// scrubURLError keeps the error text but never anything a header carried
// (net/http errors only ever quote the URL, which holds no secret here).
func scrubURLError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

// Raw is everything collected for one Hive, before classification.
type Raw struct {
	Target      Target
	Health      Fetched
	Deep        *DeepHealth
	DeepFetch   Fetched
	Contribute  *ContributeStatus
	Activity    *ContributeActivity
	Status      *StatusDoc
	StatusFetch *Fetched
	// StatusSource: "" (not attempted), "session", "status-file".
	StatusSource string
	Hub          *HubSnapshot
}

// Collector gathers Raw data for each target.
type Collector struct {
	Get Getter
	// Env looks up an environment variable (os.Getenv in production).
	Env func(string) string
	// StatusFiles maps a hive name to a local /api/status JSON file, for
	// operators who fetched it themselves (kubectl exec). Never used in CI.
	StatusFiles map[string]string
}

func decode[T any](f Fetched) (*T, error) {
	if !f.OK() {
		return nil, errors.New(f.Describe())
	}
	var v T
	if err := json.Unmarshal(f.Body, &v); err != nil {
		return nil, fmt.Errorf("GET %s: invalid JSON: %v", f.URL, err)
	}
	return &v, nil
}

// FetchHub fetches the hub snapshot. A failure returns nil: the snapshot only
// enriches the report.
func (c *Collector) FetchHub(ctx context.Context, rawURL string) (*HubSnapshot, Fetched) {
	if rawURL == "" {
		return nil, Fetched{}
	}
	f := c.Get.Get(ctx, rawURL, nil)
	hs, err := decode[HubSnapshot](f)
	if err != nil {
		return nil, f
	}
	return hs, f
}

// Collect fetches one Hive.
func (c *Collector) Collect(ctx context.Context, t Target, hub *HubSnapshot) Raw {
	r := Raw{Target: t, Hub: hub}
	r.Health = c.Get.Get(ctx, t.URL+"/api/health", nil)
	r.DeepFetch = c.Get.Get(ctx, t.URL+"/api/health/deep", nil)
	if d, err := decode[DeepHealth](r.DeepFetch); err == nil {
		r.Deep = d
	}
	if cs, err := decode[ContributeStatus](c.Get.Get(ctx, t.URL+"/api/contribute/status", nil)); err == nil {
		r.Contribute = cs
	}
	if ca, err := decode[ContributeActivity](c.Get.Get(ctx, t.URL+"/api/contribute/activity?limit=10", nil)); err == nil {
		r.Activity = ca
	}

	if path := c.StatusFiles[t.Name]; path != "" {
		r.StatusSource = "status-file"
		b, err := os.ReadFile(path)
		f := Fetched{URL: "file:" + path, Status: 200, Body: b}
		if err != nil {
			f = Fetched{URL: "file:" + path, Err: err.Error()}
		}
		r.StatusFetch = &f
		if sd, err := decode[StatusDoc](f); err == nil {
			r.Status = sd
		}
		return r
	}

	if t.SessionEnv != "" && c.Env != nil {
		if sess := strings.TrimSpace(c.Env(t.SessionEnv)); sess != "" {
			r.StatusSource = "session"
			// The only credentialed request the probe makes: a GET of
			// /api/status on the configured host with the session cookie.
			f := c.Get.Get(ctx, t.URL+"/api/status", map[string]string{"Cookie": "hive_session=" + sess})
			f.Body = []byte(strings.ReplaceAll(string(f.Body), sess, "[REDACTED]"))
			r.StatusFetch = &f
			if sd, err := decode[StatusDoc](f); err == nil {
				r.Status = sd
			}
		}
	}
	return r
}
