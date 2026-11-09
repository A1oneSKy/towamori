// Command towamori-agent reports host metrics to Towamori.
//
// It only makes outbound HTTPS requests, never listens on a port and never
// accepts commands. Run `towamori-agent --once` to see exactly what it sends.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/towamori/agent/internal/collect"
	"github.com/towamori/agent/internal/config"
	"github.com/towamori/agent/internal/report"
	"github.com/towamori/agent/internal/version"
	"github.com/towamori/agent/protocol"
)

func main() {
	cfg, err := config.Parse(os.Args[1:], os.Getenv, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "towamori-agent:", err)
		os.Exit(2)
	}
	if cfg.Version {
		fmt.Println(version.Version)
		return
	}

	agent := protocol.AgentInfo{Version: version.Version, OS: runtime.GOOS, Arch: runtime.GOARCH}
	col := collect.New(cfg.HostRoot)

	if cfg.Once {
		if err := once(col, agent); err != nil {
			fmt.Fprintln(os.Stderr, "towamori-agent:", err)
			os.Exit(1)
		}
		return
	}

	logger := log.New(os.Stderr, "", log.LstdFlags)
	logger.Printf("towamori-agent %s starting: endpoint=%s token=%s interval=%s host-root=%s",
		version.Version, cfg.Endpoint, config.Redact(cfg.Token), cfg.Interval, cfg.HostRoot)

	client := report.NewClient(cfg.Endpoint, cfg.Token, agent)
	if cfg.Print {
		client.Print = os.Stdout
	}
	r := &report.Runner{
		Client:   client,
		Source:   col,
		Agent:    agent,
		Interval: cfg.Interval,
		Log:      logger,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	_ = r.Run(ctx)
	logger.Printf("stopped")
}

// once prints the hello and report bodies the agent would send, without
// touching the network. Two readings one second apart are taken so that
// rates (CPU, disk and network throughput) are filled in.
func once(col *collect.Collector, agent protocol.AgentInfo) error {
	col.Sample()
	time.Sleep(time.Second)
	s, errs := col.Sample()
	for _, err := range errs {
		fmt.Fprintln(os.Stderr, "note:", err)
	}
	out := struct {
		Hello  protocol.HelloRequest  `json:"hello"`
		Report protocol.ReportRequest `json:"report"`
	}{
		Hello:  protocol.HelloRequest{Protocol: protocol.Version, Agent: agent, Host: col.Host()},
		Report: protocol.ReportRequest{Protocol: protocol.Version, Samples: []protocol.Sample{s}},
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
