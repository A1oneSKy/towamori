// Command towamori-devserver is a tiny stand-in for the Towamori platform,
// for developing and testing the agent locally. It accepts hello and report
// requests, checks them against the protocol and prints one line per sample.
//
//	towamori-devserver -token dev
//	towamori-agent -endpoint http://127.0.0.1:8787 -token dev
//
// It is not the platform and stores nothing.
package main

import (
	"compress/gzip"
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/towamori/agent/protocol"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8787", "address to listen on")
	token := flag.String("token", "", "token the agent must present (required)")
	interval := flag.Int("interval", 0, "if set, ask the agent to report every N seconds")
	dump := flag.Bool("dump", false, "print every request body")
	flag.Parse()
	if *token == "" {
		fmt.Fprintln(os.Stderr, "towamori-devserver: -token is required")
		os.Exit(2)
	}
	s := &server{token: *token, interval: *interval, dump: *dump, hosts: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc(protocol.HelloPath, s.hello)
	mux.HandleFunc(protocol.ReportPath, s.report)
	log.Printf("listening on http://%s (token %q)", *listen, *token)
	log.Fatal(http.ListenAndServe(*listen, mux))
}

type server struct {
	token    string
	interval int
	dump     bool

	mu    sync.Mutex
	hosts map[string]string // host_id -> hostname
}

// decode checks method, auth and headers, then unzips and decodes the body.
func (s *server) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return false
	}
	auth := r.Header.Get("Authorization")
	if subtle.ConstantTimeCompare([]byte(auth), []byte("Bearer "+s.token)) != 1 {
		log.Printf("%s: rejected, bad token", r.URL.Path)
		http.Error(w, "bad token", http.StatusUnauthorized)
		return false
	}
	if p := r.Header.Get(protocol.HeaderProtocol); p != strconv.Itoa(protocol.Version) {
		http.Error(w, "unsupported protocol "+p, http.StatusUpgradeRequired)
		return false
	}
	var body io.Reader = http.MaxBytesReader(w, r.Body, 4<<20)
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(body)
		if err != nil {
			http.Error(w, "bad gzip", http.StatusBadRequest)
			return false
		}
		defer zr.Close()
		body = zr
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return false
	}
	if s.dump {
		log.Printf("%s body:\n%s", r.URL.Path, raw)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func (s *server) hello(w http.ResponseWriter, r *http.Request) {
	var req protocol.HelloRequest
	if !s.decode(w, r, &req) {
		return
	}
	id := "dev-" + req.Host.Hostname
	s.mu.Lock()
	s.hosts[id] = req.Host.Hostname
	s.mu.Unlock()
	h := req.Host
	log.Printf("hello from %s: agent %s %s/%s, %s, kernel %s, %d cores %s, %s RAM, machine %s",
		h.Hostname, req.Agent.Version, req.Agent.OS, req.Agent.Arch, orDash(h.OSPretty), orDash(h.Kernel),
		h.CPUCores, orDash(h.CPUModel), size(h.MemoryTotal), orDash(h.MachineID))
	writeJSON(w, http.StatusOK, protocol.HelloResponse{
		HostID: id, IntervalSeconds: s.interval, ServerTime: time.Now().Unix(),
	})
}

func (s *server) report(w http.ResponseWriter, r *http.Request) {
	var req protocol.ReportRequest
	if !s.decode(w, r, &req) {
		return
	}
	s.mu.Lock()
	name, ok := s.hosts[req.HostID]
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusAccepted, protocol.ReportResponse{HelloRequired: true})
		return
	}
	if len(req.Samples) == 0 {
		http.Error(w, "no samples", http.StatusBadRequest)
		return
	}
	for _, smp := range req.Samples {
		if problems := check(smp); len(problems) > 0 {
			log.Printf("[%s] sample %d has problems: %s", name, smp.TS, strings.Join(problems, "; "))
		}
		log.Printf("[%s] %s", name, summary(smp))
	}
	writeJSON(w, http.StatusAccepted, protocol.ReportResponse{Accepted: len(req.Samples), IntervalSeconds: s.interval})
}

// check flags values that cannot be right, to catch collector bugs early.
func check(s protocol.Sample) []string {
	var p []string
	if d := time.Since(time.Unix(s.TS, 0)); d > 24*time.Hour || d < -5*time.Minute {
		p = append(p, "timestamp is off by "+d.Round(time.Second).String())
	}
	pct := func(name string, v float64) {
		if v < 0 || v > 100 {
			p = append(p, fmt.Sprintf("%s=%.2f is outside 0-100", name, v))
		}
	}
	if c := s.CPU; c != nil {
		pct("cpu.usage", c.Usage)
		pct("cpu.user", c.User)
		pct("cpu.system", c.System)
		pct("cpu.iowait", c.IOWait)
		pct("cpu.steal", c.Steal)
	}
	if m := s.Mem; m != nil && (m.Used > m.Total || m.Available > m.Total) {
		p = append(p, "memory used/available larger than total")
	}
	for _, d := range s.Disks {
		if d.Used > d.Total {
			p = append(p, "disk "+d.Mount+" used larger than total")
		}
	}
	return p
}

func summary(s protocol.Sample) string {
	var b strings.Builder
	b.WriteString(time.Unix(s.TS, 0).Format("15:04:05"))
	if s.CPU != nil {
		fmt.Fprintf(&b, "  cpu %.1f%%", s.CPU.Usage)
	} else {
		b.WriteString("  cpu -")
	}
	if s.Load != nil {
		fmt.Fprintf(&b, "  load %.2f", s.Load.L1)
	}
	if s.Mem != nil && s.Mem.Total > 0 {
		fmt.Fprintf(&b, "  mem %.0f%% (%s/%s)", float64(s.Mem.Used)/float64(s.Mem.Total)*100, size(s.Mem.Used), size(s.Mem.Total))
	}
	for _, d := range s.Disks {
		if d.Total > 0 {
			fmt.Fprintf(&b, "  %s %.0f%%", d.Mount, float64(d.Used)/float64(d.Total)*100)
		}
	}
	if s.Net != nil {
		fmt.Fprintf(&b, "  net ↓%s/s ↑%s/s", size(uint64(s.Net.RxBps)), size(uint64(s.Net.TxBps)))
	}
	if s.DiskIO != nil {
		fmt.Fprintf(&b, "  io r%s/s w%s/s", size(uint64(s.DiskIO.ReadBps)), size(uint64(s.DiskIO.WriteBps)))
	}
	if s.Procs > 0 {
		fmt.Fprintf(&b, "  procs %d", s.Procs)
	}
	return b.String()
}

func size(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
