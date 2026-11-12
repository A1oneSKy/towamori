package linkprobe

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/towamori/agent/protocol"
)

func quiet() (*log.Logger, *bytes.Buffer) {
	var b bytes.Buffer
	return log.New(&b, "", 0), &b
}

func local(host string) func(context.Context, string) ([]netip.Addr, error) {
	return func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr(host)}, nil
	}
}

func listener(t *testing.T) (port int, closeFn func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port, func() { l.Close() }
}

func newProber() *Prober {
	lg, _ := quiet()
	return &Prober{Log: lg, Count: 3, Gap: time.Millisecond, Timeout: 500 * time.Millisecond, allowPrivate: true, lookup: local("127.0.0.1")}
}

func TestMeasureReachable(t *testing.T) {
	port, stop := listener(t)
	defer stop()
	p := newProber()
	p.init()
	r := p.measure(context.Background(), protocol.LinkTarget{ID: "tyo", Host: "tyo." + protocol.LinkDomain, Port: port})
	if r.Sent != 3 || r.OK != 3 || r.Err != "" {
		t.Fatalf("%+v", r)
	}
	if r.Avg <= 0 || r.Min > r.Avg || r.Avg > r.Max {
		t.Errorf("times out of order: %+v", r)
	}
}

func TestMeasureFailures(t *testing.T) {
	// Nothing listens: the connection is refused.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	p := newProber()
	p.init()
	tg := protocol.LinkTarget{ID: "fra", Host: "fra." + protocol.LinkDomain, Port: port}
	if r := p.measure(context.Background(), tg); r.OK != 0 || r.Err != protocol.LinkErrRefused || r.Sent != 3 {
		t.Errorf("refused: %+v", r)
	}

	// A connection that never completes is a timeout.
	slow := newProber()
	slow.init()
	slow.dial = func(ctx context.Context, addr string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	slow.Count = 1
	if r := slow.measure(ctx, tg); r.Err != protocol.LinkErrTimeout {
		t.Errorf("timeout: %+v", r)
	}

	// Name lookup fails.
	dns := newProber()
	dns.lookup = func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("no such host") }
	dns.init()
	if r := dns.measure(context.Background(), tg); r.Err != protocol.LinkErrDNS || r.Sent != 3 {
		t.Errorf("dns: %+v", r)
	}
}

func TestPrivateAddressesAreRefused(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "192.168.0.9", "172.16.5.5", "169.254.1.1", "::1", "fe80::1", "100.64.0.1", "100.127.255.254", "0.0.0.0", "fd00::1"} {
		p := &Prober{Log: log.New(&bytes.Buffer{}, "", 0), Count: 1, lookup: local(ip)}
		p.init()
		r := p.measure(context.Background(), protocol.LinkTarget{ID: "x", Host: "x." + protocol.LinkDomain, Port: 8443})
		if r.Err != protocol.LinkErrBlocked {
			t.Errorf("%s was not refused: %+v", ip, r)
		}
	}
	// A public address is fine (nothing is dialled here).
	p := &Prober{Log: log.New(&bytes.Buffer{}, "", 0)}
	if a, ok := p.pick([]netip.Addr{netip.MustParseAddr("2001:db8::1"), netip.MustParseAddr("203.0.113.7")}); !ok || a.String() != "203.0.113.7" {
		t.Errorf("pick = %v %v, want the IPv4 address", a, ok)
	}
}

func TestSetTargetsKeepsOnlyOurs(t *testing.T) {
	lg, out := quiet()
	p := &Prober{Log: lg}
	set := protocol.LinkSet{Rev: "abc", Targets: []protocol.LinkTarget{
		{ID: "tyo", Host: "tyo." + protocol.LinkDomain, Port: 8443},
		{ID: "evil", Host: "example.com", Port: 8443},
		{ID: "evil2", Host: "tyo." + protocol.LinkDomain + ".example.com", Port: 8443},
		{ID: "ssh", Host: "sel." + protocol.LinkDomain, Port: 22},
		{ID: "Up", Host: "up." + protocol.LinkDomain, Port: 8443},
		{ID: "ip", Host: "127.0.0.1", Port: 8443},
	}}
	p.SetTargets(set)
	p.SetTargets(set)
	if got := p.Rev(); got != "abc" {
		t.Errorf("rev %q", got)
	}
	p.mu.Lock()
	n := len(p.set.Targets)
	p.mu.Unlock()
	if n != 1 {
		t.Errorf("kept %d targets, want 1", n)
	}
	if bytes.Count(out.Bytes(), []byte("ignoring evil:")) != 1 {
		t.Errorf("each refused target should be logged once:\n%s", out.String())
	}
}

func TestRoundAndTake(t *testing.T) {
	a, stopA := listener(t)
	defer stopA()
	b, stopB := listener(t)
	defer stopB()
	p := newProber()
	p.SetTargets(protocol.LinkSet{Rev: "r", Targets: []protocol.LinkTarget{
		{ID: "tyo", Host: "tyo." + protocol.LinkDomain, Port: b},
		{ID: "hkg", Host: "hkg." + protocol.LinkDomain, Port: a},
	}})
	if got := p.Take(); got != nil {
		t.Fatalf("results before a round: %v", got)
	}
	p.Round(context.Background())
	got := p.Take()
	if len(got) != 2 || got[0].ID != "hkg" || got[1].ID != "tyo" || got[0].OK != 3 || got[1].OK != 3 {
		t.Fatalf("%+v", got)
	}
	if again := p.Take(); again != nil {
		t.Errorf("results must be handed over once: %v", again)
	}
}

func TestProtocolRules(t *testing.T) {
	ok := protocol.LinkTarget{ID: "tpe", Host: "tpe." + protocol.LinkDomain, Port: 8443}
	if err := protocol.CheckLinkTarget(ok); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []protocol.LinkTarget{
		{ID: "", Host: ok.Host, Port: 8443},
		{ID: "a b", Host: ok.Host, Port: 8443},
		{ID: "tpe", Host: protocol.LinkDomain, Port: 8443},
		{ID: "tpe", Host: "tpe." + protocol.LinkDomain + ":22", Port: 8443},
		{ID: "tpe", Host: "TPE." + protocol.LinkDomain, Port: 8443},
		{ID: "tpe", Host: ok.Host, Port: 1023},
		{ID: "tpe", Host: ok.Host, Port: 65536},
		{ID: "tpe", Host: "x@" + protocol.LinkDomain, Port: 8443},
	} {
		if protocol.CheckLinkTarget(bad) == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	a := protocol.LinkRev([]protocol.LinkTarget{ok, {ID: "tyo", Host: "tyo." + protocol.LinkDomain, Port: 8443}})
	b := protocol.LinkRev([]protocol.LinkTarget{{ID: "tyo", Host: "tyo." + protocol.LinkDomain, Port: 8443}, ok})
	if a != b || len(a) != 12 {
		t.Errorf("revision should not depend on order: %q %q", a, b)
	}
	if c := protocol.LinkRev([]protocol.LinkTarget{{ID: "tpe", Host: ok.Host, Port: 8444}}); c == a {
		t.Error("revision should change with the list")
	}
}
