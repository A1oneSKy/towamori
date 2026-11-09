package report

import (
	"context"
	"errors"
	"log"
	"math/rand"
	"net/http"
	"time"

	"github.com/towamori/agent/internal/protocol"
)

// Source produces readings; collect.Collector implements it.
type Source interface {
	Host() protocol.HostInfo
	Sample() (protocol.Sample, []error)
}

const (
	DefaultMaxBuffer = 360 // one hour at the default 10 s interval
	DefaultMaxBatch  = 60

	authRetry     = 5 * time.Minute
	upgradeRetry  = time.Hour
	defaultLimit  = time.Minute
	minBackoff    = 2 * time.Second
	maxBackoff    = 5 * time.Minute
	shutdownFlush = 5 * time.Second
)

// Runner is the agent's main loop: collect, buffer, send.
type Runner struct {
	Client   *Client
	Source   Source
	Agent    protocol.AgentInfo
	Interval time.Duration
	Log      *log.Logger

	MaxBuffer int
	MaxBatch  int
	now       func() time.Time

	buf         []protocol.Sample
	hostID      string
	needHello   bool
	nextAttempt time.Time
	backoff     time.Duration
	batch       int
	failing     bool
	seenErrs    map[string]bool
}

func (r *Runner) init() {
	if r.MaxBuffer == 0 {
		r.MaxBuffer = DefaultMaxBuffer
	}
	if r.MaxBatch == 0 {
		r.MaxBatch = DefaultMaxBatch
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.Log == nil {
		r.Log = log.Default()
	}
	r.batch = r.MaxBatch
	r.needHello = true
	r.seenErrs = map[string]bool{}
}

// Run collects every Interval until ctx is cancelled, then makes one last
// attempt to send what is buffered.
func (r *Runner) Run(ctx context.Context) error {
	r.init()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			if len(r.buf) > 0 && !r.needHello {
				fctx, cancel := context.WithTimeout(context.Background(), shutdownFlush)
				r.flush(fctx)
				cancel()
			}
			return nil
		case <-timer.C:
		}
		r.tick(ctx)
		timer.Reset(r.Interval)
	}
}

// tick takes one sample and, unless we are backing off, tries to send.
func (r *Runner) tick(ctx context.Context) {
	s, errs := r.Source.Sample()
	for _, err := range errs {
		// Log each distinct collection problem once, not every 10 seconds.
		if msg := err.Error(); !r.seenErrs[msg] {
			r.seenErrs[msg] = true
			r.Log.Printf("collect: %v", err)
		}
	}
	r.push(s)

	if r.now().Before(r.nextAttempt) {
		return
	}
	if r.needHello {
		if err := r.hello(ctx); err != nil {
			r.fail(err, false)
			return
		}
	}
	r.flush(ctx)
}

func (r *Runner) push(s protocol.Sample) {
	r.buf = append(r.buf, s)
	if over := len(r.buf) - r.MaxBuffer; over > 0 {
		r.buf = append(r.buf[:0], r.buf[over:]...)
	}
}

func (r *Runner) hello(ctx context.Context) error {
	resp, err := r.Client.Hello(ctx, protocol.HelloRequest{
		Protocol: protocol.Version,
		Agent:    r.Agent,
		Host:     r.Source.Host(),
	})
	if err != nil {
		return err
	}
	r.hostID = resp.HostID
	r.needHello = false
	r.Log.Printf("connected to platform as host %q", r.hostID)
	r.applyInterval(resp.IntervalSeconds)
	return nil
}

// flush sends buffered samples oldest first until the buffer is empty or a
// request fails.
func (r *Runner) flush(ctx context.Context) {
	for len(r.buf) > 0 {
		n := r.batch
		if n > len(r.buf) {
			n = len(r.buf)
		}
		resp, err := r.Client.Report(ctx, protocol.ReportRequest{
			Protocol: protocol.Version,
			HostID:   r.hostID,
			Samples:  r.buf[:n],
		})
		if err != nil {
			if r.fail(err, true) {
				r.buf = append(r.buf[:0], r.buf[n:]...)
				continue
			}
			return
		}
		r.buf = append(r.buf[:0], r.buf[n:]...)
		if r.failing {
			r.Log.Printf("reporting resumed")
			r.failing = false
		}
		r.backoff = 0
		r.batch = r.MaxBatch
		r.applyInterval(resp.IntervalSeconds)
		if resp.HelloRequired {
			r.needHello = true
			return
		}
	}
}

// fail decides when to try again. For a report it returns true when the
// batch can never succeed and should be dropped.
func (r *Runner) fail(err error, isReport bool) (drop bool) {
	now := r.now()
	r.failing = true
	var se *StatusError
	if !errors.As(err, &se) {
		r.backoffFor(now)
		r.Log.Printf("cannot reach platform: %v (retrying in %s, %d samples buffered)", err, r.backoff.Round(time.Second), len(r.buf))
		return false
	}
	switch {
	case se.Code == http.StatusUnauthorized || se.Code == http.StatusForbidden:
		r.needHello = true
		r.nextAttempt = now.Add(authRetry)
		r.Log.Printf("token rejected (HTTP %d); check TOWAMORI_TOKEN. Retrying in %s", se.Code, authRetry)
	case se.Code == http.StatusRequestEntityTooLarge && isReport:
		if r.batch > 1 {
			r.batch /= 2
		}
		r.nextAttempt = now
	case se.Code == http.StatusUpgradeRequired:
		r.nextAttempt = now.Add(upgradeRetry)
		r.Log.Printf("this agent version is no longer supported by the platform; please upgrade towamori-agent")
	case se.Code == http.StatusTooManyRequests:
		wait := se.RetryAfter
		if wait == 0 {
			wait = defaultLimit
		}
		r.nextAttempt = now.Add(wait)
		r.Log.Printf("rate limited; waiting %s", wait)
	case se.Code >= 400 && se.Code < 500 && isReport:
		r.Log.Printf("platform rejected a batch (%v); dropping it", se)
		return true
	default:
		r.backoffFor(now)
		r.Log.Printf("%v (retrying in %s, %d samples buffered)", se, r.backoff.Round(time.Second), len(r.buf))
	}
	return false
}

func (r *Runner) backoffFor(now time.Time) {
	if r.backoff == 0 {
		r.backoff = minBackoff
	} else {
		r.backoff *= 2
	}
	if r.backoff > maxBackoff {
		r.backoff = maxBackoff
	}
	// ±20 % jitter so a platform outage does not end in a thundering herd.
	jitter := time.Duration((rand.Float64()*0.4 - 0.2) * float64(r.backoff))
	r.nextAttempt = now.Add(r.backoff + jitter)
}

func (r *Runner) applyInterval(seconds int) {
	s := protocol.ClampInterval(seconds)
	if s == 0 {
		return
	}
	d := time.Duration(s) * time.Second
	if d != r.Interval {
		r.Log.Printf("platform set the reporting interval to %s", d)
		r.Interval = d
	}
}
