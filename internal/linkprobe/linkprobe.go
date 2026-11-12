// Package linkprobe measures how well this machine reaches Towamori's probe
// locations: it opens a plain TCP connection to each one, closes it again at
// once and times it. Nothing is sent or read, so the only thing that leaves
// the machine is a connection attempt, and the only thing kept is a few
// numbers per location.
//
// The list of locations comes from the platform, but only names under
// protocol.LinkDomain with a port of 1024 or more are ever used, and a name
// that resolves to a private or local address is refused. A platform cannot
// use this to make the agent knock on someone else's door.
package linkprobe

import (
	"context"
	"errors"
	"log"
	"math/rand"
	"net"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/towamori/agent/protocol"
)

const (
	DefaultEvery   = time.Minute
	DefaultCount   = 5
	DefaultGap     = 250 * time.Millisecond
	DefaultTimeout = 3 * time.Second
)

// Prober owns the target list and the results waiting to be reported.
type Prober struct {
	Log *log.Logger

	Every   time.Duration // between rounds
	Count   int           // connection attempts per location per round
	Gap     time.Duration // between attempts to the same location
	Timeout time.Duration // per attempt (and per name lookup)

	// Hooks for tests; nil means the real network.
	lookup       func(ctx context.Context, host string) ([]netip.Addr, error)
	dial         func(ctx context.Context, addr string) (net.Conn, error)
	allowPrivate bool

	mu      sync.Mutex
	set     protocol.LinkSet
	pending []protocol.LinkResult
	warned  map[string]bool
}

func (p *Prober) init() {
	if p.Every == 0 {
		p.Every = DefaultEvery
	}
	if p.Count == 0 {
		p.Count = DefaultCount
	}
	if p.Gap == 0 {
		p.Gap = DefaultGap
	}
	if p.Timeout == 0 {
		p.Timeout = DefaultTimeout
	}
	if p.Log == nil {
		p.Log = log.Default()
	}
	if p.lookup == nil {
		p.lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	if p.dial == nil {
		d := &net.Dialer{Timeout: p.Timeout}
		p.dial = func(ctx context.Context, addr string) (net.Conn, error) { return d.DialContext(ctx, "tcp", addr) }
	}
	p.mu.Lock()
	if p.warned == nil {
		p.warned = map[string]bool{}
	}
	p.mu.Unlock()
}

// Rev is the revision of the list currently held ("" before the first one).
func (p *Prober) Rev() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.set.Rev
}

// SetTargets replaces the list. Entries that are not ours to measure are
// dropped (and logged once); the revision is kept as the platform sent it so
// that it is not asked for the same list again.
func (p *Prober) SetTargets(set protocol.LinkSet) {
	p.init()
	var ok []protocol.LinkTarget
	for _, t := range set.Targets {
		if len(ok) >= protocol.MaxLinkTargets {
			p.warnOnce("too many", "link targets: ignoring entries past "+strconv.Itoa(protocol.MaxLinkTargets))
			break
		}
		if err := protocol.CheckLinkTarget(t); err != nil {
			p.warnOnce(t.ID+t.Host, "link targets: ignoring "+t.ID+": "+err.Error())
			continue
		}
		ok = append(ok, t)
	}
	p.mu.Lock()
	changed := p.set.Rev != set.Rev
	p.set = protocol.LinkSet{Rev: set.Rev, Targets: ok}
	p.mu.Unlock()
	if changed {
		p.Log.Printf("measuring the link to %d probe location(s)", len(ok))
	}
}

func (p *Prober) warnOnce(key, msg string) {
	p.mu.Lock()
	seen := p.warned[key]
	p.warned[key] = true
	p.mu.Unlock()
	if !seen {
		p.Log.Print(msg)
	}
}

// Take returns the results of the latest finished round, once.
func (p *Prober) Take() []protocol.LinkResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.pending
	p.pending = nil
	return out
}

// Run measures every Every until ctx is cancelled. The first round waits a
// little, with some jitter, so that agents started together do not all knock
// on the same door at the same second.
func (p *Prober) Run(ctx context.Context) {
	p.init()
	wait := 10*time.Second + time.Duration(rand.Int63n(int64(20*time.Second)))
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		p.Round(ctx)
		wait = p.Every + time.Duration(rand.Int63n(int64(5*time.Second)))
	}
}

// Round measures every target once (all targets at the same time) and keeps
// the results for Take.
func (p *Prober) Round(ctx context.Context) {
	p.init()
	p.mu.Lock()
	targets := append([]protocol.LinkTarget(nil), p.set.Targets...)
	p.mu.Unlock()
	if len(targets) == 0 {
		return
	}
	results := make([]protocol.LinkResult, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = p.measure(ctx, t)
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}
	sort.Slice(results, func(i, j int) bool { return results[i].ID < results[j].ID })
	p.mu.Lock()
	p.pending = results
	p.mu.Unlock()
}

func (p *Prober) measure(ctx context.Context, t protocol.LinkTarget) protocol.LinkResult {
	res := protocol.LinkResult{ID: t.ID}
	lctx, cancel := context.WithTimeout(ctx, p.Timeout)
	addrs, err := p.lookup(lctx, t.Host)
	cancel()
	if err != nil || len(addrs) == 0 {
		res.Sent, res.Err = p.Count, protocol.LinkErrDNS
		return res
	}
	addr, ok := p.pick(addrs)
	if !ok {
		res.Sent, res.Err = p.Count, protocol.LinkErrBlocked
		return res
	}
	target := net.JoinHostPort(addr.String(), strconv.Itoa(t.Port))

	var sum, lo, hi float64
	var lastErr error
	for i := 0; i < p.Count; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return res
			case <-time.After(p.Gap):
			}
		}
		res.Sent++
		start := time.Now()
		c, err := p.dial(ctx, target)
		ms := float64(time.Since(start)) / float64(time.Millisecond)
		if err != nil {
			lastErr = err
			continue
		}
		_ = c.Close()
		if res.OK == 0 || ms < lo {
			lo = ms
		}
		if ms > hi {
			hi = ms
		}
		sum += ms
		res.OK++
	}
	if res.OK == 0 {
		res.Err = classify(lastErr)
		return res
	}
	res.Min, res.Avg, res.Max = round1(lo), round1(sum/float64(res.OK)), round1(hi)
	return res
}

// pick chooses the address to measure: IPv4 first, because that is what the
// probe locations publish and what most machines can reach; and only a public
// one.
func (p *Prober) pick(addrs []netip.Addr) (netip.Addr, bool) {
	var v4, v6 []netip.Addr
	for _, a := range addrs {
		a = a.Unmap()
		if !p.allowPrivate && !public(a) {
			continue
		}
		if a.Is4() {
			v4 = append(v4, a)
		} else {
			v6 = append(v6, a)
		}
	}
	if len(v4) > 0 {
		return v4[0], true
	}
	if len(v6) > 0 {
		return v6[0], true
	}
	return netip.Addr{}, false
}

// cgnat is the shared address space of carrier-grade NAT, which Tailscale and
// many providers' internal networks also use.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// public reports whether a is a routable address on the open internet.
func public(a netip.Addr) bool {
	return a.IsGlobalUnicast() && !a.IsPrivate() && !a.IsLinkLocalUnicast() && !cgnat.Contains(a)
}

func classify(err error) string {
	switch {
	case err == nil:
		return protocol.LinkErrOther
	case errors.Is(err, syscall.ECONNREFUSED):
		return protocol.LinkErrRefused
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		return protocol.LinkErrUnreachable
	case errors.Is(err, os.ErrDeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		return protocol.LinkErrTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return protocol.LinkErrTimeout
	}
	return protocol.LinkErrOther
}

func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }
