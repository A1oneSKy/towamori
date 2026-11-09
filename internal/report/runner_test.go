package report

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/towamori/agent/internal/protocol"
)

// fakeSource returns samples with increasing timestamps 1, 2, 3, ...
type fakeSource struct{ n int64 }

func (f *fakeSource) Host() protocol.HostInfo {
	return protocol.HostInfo{Hostname: "web-1", CPUCores: 2}
}
func (f *fakeSource) Sample() (protocol.Sample, []error) {
	f.n++
	return protocol.Sample{TS: f.n}, nil
}

// platform is a scriptable fake of the ingest API.
type platform struct {
	mu        sync.Mutex
	hellos    int
	received  []int64 // sample timestamps, in arrival order
	headers   []http.Header
	nextCodes []int // status codes to answer reports with, then 202
	reply     protocol.ReportResponse
	helloCode int
	retry     string
}

func (p *platform) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.headers = append(p.headers, r.Header.Clone())
	zr, err := gzip.NewReader(r.Body)
	if err != nil {
		http.Error(w, "not gzip", 400)
		return
	}
	raw, _ := io.ReadAll(zr)
	switch r.URL.Path {
	case protocol.HelloPath:
		p.hellos++
		if p.helloCode != 0 {
			w.WriteHeader(p.helloCode)
			return
		}
		var req protocol.HelloRequest
		if json.Unmarshal(raw, &req) != nil || req.Host.Hostname != "web-1" || req.Protocol != 1 {
			http.Error(w, "bad hello", 400)
			return
		}
		json.NewEncoder(w).Encode(protocol.HelloResponse{HostID: "h_1"})
	case protocol.ReportPath:
		var req protocol.ReportRequest
		if json.Unmarshal(raw, &req) != nil || req.HostID != "h_1" {
			http.Error(w, "bad report", 400)
			return
		}
		if len(p.nextCodes) > 0 {
			code := p.nextCodes[0]
			p.nextCodes = p.nextCodes[1:]
			if p.retry != "" {
				w.Header().Set("Retry-After", p.retry)
			}
			w.WriteHeader(code)
			return
		}
		for _, s := range req.Samples {
			p.received = append(p.received, s.TS)
		}
		w.WriteHeader(http.StatusAccepted)
		reply := p.reply
		reply.Accepted = len(req.Samples)
		json.NewEncoder(w).Encode(reply)
	default:
		http.NotFound(w, r)
	}
}

type harness struct {
	r    *Runner
	p    *platform
	now  time.Time
	logs *bytes.Buffer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	p := &platform{}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	h := &harness{p: p, now: time.Unix(1794200000, 0), logs: &bytes.Buffer{}}
	h.r = &Runner{
		Client:   NewClient(srv.URL, "twa_secret_token", protocol.AgentInfo{Version: "0.1.0", OS: "linux", Arch: "amd64"}),
		Source:   &fakeSource{},
		Agent:    protocol.AgentInfo{Version: "0.1.0", OS: "linux", Arch: "amd64"},
		Interval: 10 * time.Second,
		Log:      log.New(h.logs, "", 0),
		now:      func() time.Time { return h.now },
	}
	h.r.init()
	return h
}

func (h *harness) tick(advance time.Duration) {
	h.now = h.now.Add(advance)
	h.r.tick(context.Background())
}

func (h *harness) received() []int64 {
	h.p.mu.Lock()
	defer h.p.mu.Unlock()
	return append([]int64(nil), h.p.received...)
}

func equal(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestHelloThenReportWithHeaders(t *testing.T) {
	h := newHarness(t)
	h.tick(0)
	h.tick(10 * time.Second)
	if h.p.hellos != 1 {
		t.Errorf("hellos = %d, want 1", h.p.hellos)
	}
	if got := h.received(); !equal(got, []int64{1, 2}) {
		t.Errorf("received %v", got)
	}
	hd := h.p.headers[0]
	checks := map[string]string{
		"Authorization":         "Bearer twa_secret_token",
		"Content-Encoding":      "gzip",
		"Content-Type":          "application/json",
		"User-Agent":            "towamori-agent/0.1.0 (linux/amd64)",
		protocol.HeaderProtocol: "1",
	}
	for k, want := range checks {
		if got := hd.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	if strings.Contains(h.logs.String(), "twa_secret_token") {
		t.Error("token must never appear in logs")
	}
}

func TestBuffersWhileDownAndResendsInOrder(t *testing.T) {
	h := newHarness(t)
	h.tick(0) // hello + sample 1
	h.p.nextCodes = []int{503, 503}
	h.tick(10 * time.Second) // sample 2 -> 503, backoff ~2s
	h.tick(10 * time.Second) // sample 3 -> 503, backoff ~4s
	if got := h.received(); !equal(got, []int64{1}) {
		t.Fatalf("while down received %v", got)
	}
	h.tick(10 * time.Second) // sample 4 -> platform back
	if got := h.received(); !equal(got, []int64{1, 2, 3, 4}) {
		t.Errorf("after recovery received %v, want 1..4 once each in order", got)
	}
	if !strings.Contains(h.logs.String(), "reporting resumed") {
		t.Errorf("expected a recovery log line, got:\n%s", h.logs)
	}
}

func TestBackoffSkipsSendingUntilDue(t *testing.T) {
	h := newHarness(t)
	h.tick(0)
	h.p.nextCodes = []int{500, 500, 500, 500}
	for i := 0; i < 4; i++ {
		h.tick(10 * time.Second)
	}
	// Backoff is now about 16 s; a tick 1 s later must not hit the platform.
	before := len(h.p.headers)
	h.tick(time.Second)
	if len(h.p.headers) != before {
		t.Error("sent a request while backing off")
	}
}

func TestBufferKeepsNewestSamples(t *testing.T) {
	h := newHarness(t)
	h.r.MaxBuffer = 5
	h.p.helloCode = 503
	for i := 0; i < 8; i++ {
		h.tick(10 * time.Minute) // always past the backoff, always failing hello
	}
	h.p.helloCode = 0
	h.tick(10 * time.Minute)
	if got := h.received(); !equal(got, []int64{5, 6, 7, 8, 9}) {
		t.Errorf("received %v, want the newest 5", got)
	}
}

func TestBatchesAreLimited(t *testing.T) {
	h := newHarness(t)
	h.r.MaxBatch = 3
	h.r.batch = 3
	h.p.helloCode = 503
	for i := 0; i < 7; i++ {
		h.tick(10 * time.Minute)
	}
	h.p.helloCode = 0
	before := len(h.p.headers)
	h.tick(10 * time.Minute) // 8 samples buffered -> 3 + 3 + 2
	if reports := len(h.p.headers) - before - 1; reports != 3 {
		t.Errorf("sent %d report requests, want 3", reports)
	}
	if got := h.received(); len(got) != 8 || got[0] != 1 || got[7] != 8 {
		t.Errorf("received %v", got)
	}
}

func TestUnauthorizedWaitsAndSaysHelloAgain(t *testing.T) {
	h := newHarness(t)
	h.tick(0)
	h.p.nextCodes = []int{401}
	h.tick(10 * time.Second)
	if !h.r.needHello || h.r.nextAttempt.Sub(h.now) != authRetry {
		t.Errorf("after 401: needHello=%v wait=%s", h.r.needHello, h.r.nextAttempt.Sub(h.now))
	}
	if !strings.Contains(h.logs.String(), "token rejected") {
		t.Errorf("expected a token log line:\n%s", h.logs)
	}
	h.tick(authRetry)
	if h.p.hellos != 2 {
		t.Errorf("hellos = %d, want 2", h.p.hellos)
	}
	if got := h.received(); !equal(got, []int64{1, 2, 3}) {
		t.Errorf("received %v", got)
	}
}

func TestRateLimitHonoursRetryAfter(t *testing.T) {
	h := newHarness(t)
	h.tick(0)
	h.p.nextCodes = []int{429}
	h.p.retry = "120"
	h.tick(10 * time.Second)
	if wait := h.r.nextAttempt.Sub(h.now); wait != 120*time.Second {
		t.Errorf("wait = %s, want 2m0s", wait)
	}
}

func TestBadRequestDropsTheBatch(t *testing.T) {
	h := newHarness(t)
	h.tick(0)
	h.p.nextCodes = []int{422}
	h.tick(10 * time.Second) // sample 2 dropped
	h.tick(10 * time.Second)
	if got := h.received(); !equal(got, []int64{1, 3}) {
		t.Errorf("received %v, want [1 3]", got)
	}
}

func TestTooLargeHalvesTheBatch(t *testing.T) {
	h := newHarness(t)
	h.tick(0)
	h.p.nextCodes = []int{413}
	h.tick(10 * time.Second)
	if h.r.batch != DefaultMaxBatch/2 {
		t.Errorf("batch = %d, want %d", h.r.batch, DefaultMaxBatch/2)
	}
	h.tick(10 * time.Second)
	if got := h.received(); !equal(got, []int64{1, 2, 3}) {
		t.Errorf("received %v", got)
	}
	if h.r.batch != DefaultMaxBatch {
		t.Errorf("batch should reset after success, got %d", h.r.batch)
	}
}

func TestPlatformCanOnlyChangeTheIntervalWithinBounds(t *testing.T) {
	h := newHarness(t)
	h.tick(0)
	h.p.reply = protocol.ReportResponse{IntervalSeconds: 1}
	h.tick(10 * time.Second)
	if h.r.Interval != 5*time.Second {
		t.Errorf("interval = %s, want clamped to 5s", h.r.Interval)
	}
	h.p.reply = protocol.ReportResponse{IntervalSeconds: 100000}
	h.tick(10 * time.Second)
	if h.r.Interval != 300*time.Second {
		t.Errorf("interval = %s, want clamped to 5m0s", h.r.Interval)
	}
}

func TestHelloRequiredTriggersHello(t *testing.T) {
	h := newHarness(t)
	h.tick(0)
	h.p.reply = protocol.ReportResponse{HelloRequired: true}
	h.tick(10 * time.Second)
	h.p.reply = protocol.ReportResponse{}
	h.tick(10 * time.Second)
	if h.p.hellos != 2 {
		t.Errorf("hellos = %d, want 2", h.p.hellos)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var leaked bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked = true
		}
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	c := NewClient(redirector.URL, "twa_secret_token", protocol.AgentInfo{Version: "t"})
	_, err := c.Hello(context.Background(), protocol.HelloRequest{Protocol: 1})
	var se *StatusError
	if err == nil || !asStatus(err, &se) || se.Code != http.StatusTemporaryRedirect {
		t.Errorf("want a 307 StatusError, got %v", err)
	}
	if leaked {
		t.Error("token was sent to the redirect target")
	}
}

func asStatus(err error, se **StatusError) bool {
	s, ok := err.(*StatusError)
	if ok {
		*se = s
	}
	return ok
}

func TestPrintShowsBodies(t *testing.T) {
	h := newHarness(t)
	var out bytes.Buffer
	h.r.Client.Print = &out
	h.tick(0)
	if !strings.Contains(out.String(), "POST /v1/agent/hello") || !strings.Contains(out.String(), "POST /v1/agent/report") {
		t.Errorf("print output:\n%s", out.String())
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	h := newHarness(t)
	h.r.now = time.Now
	h.r.Interval = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { h.r.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if got := h.received(); len(got) < 2 {
		t.Errorf("expected a few samples delivered, got %v", got)
	}
}
