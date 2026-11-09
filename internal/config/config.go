// Package config resolves agent settings from, in increasing priority:
// built-in defaults, an environment file, process environment variables and
// command-line flags.
package config

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/towamori/agent/internal/protocol"
)

const (
	DefaultEndpoint = "https://ingest.towamori.com"
	DefaultEnvFile  = "/etc/towamori/agent.env"
	DefaultInterval = 10 * time.Second
)

type Config struct {
	Endpoint string
	Token    string
	Interval time.Duration
	EnvFile  string

	Once    bool // collect one sample, print it, exit; no network
	Print   bool // also print every report body to stdout
	Version bool
}

// Parse builds a Config from args (without the program name). getenv is
// os.Getenv in production and a map lookup in tests.
func Parse(args []string, getenv func(string) string, stderr io.Writer) (*Config, error) {
	fs := flag.NewFlagSet("towamori-agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		endpoint = fs.String("endpoint", "", "platform URL (env TOWAMORI_ENDPOINT, default "+DefaultEndpoint+")")
		token    = fs.String("token", "", "agent token from the dashboard (env TOWAMORI_TOKEN)")
		interval = fs.Duration("interval", 0, "how often to collect, 5s-300s (env TOWAMORI_INTERVAL, default 10s)")
		envFile  = fs.String("env-file", DefaultEnvFile, "file with KEY=VALUE settings; ignored if missing")
		once     = fs.Bool("once", false, "collect one sample, print the JSON that would be sent, and exit (no network)")
		print    = fs.Bool("print", false, "also print every report body to stdout")
		version  = fs.Bool("version", false, "print the version and exit")
	)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	c := &Config{EnvFile: *envFile, Once: *once, Print: *print, Version: *version}
	if c.Version {
		return c, nil
	}

	fileVals := map[string]string{}
	if c.EnvFile != "" {
		vals, err := ReadEnvFile(c.EnvFile)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		fileVals = vals
	}
	lookup := func(key string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return fileVals[key]
	}

	c.Endpoint = firstNonEmpty(*endpoint, lookup("TOWAMORI_ENDPOINT"), DefaultEndpoint)
	c.Endpoint = strings.TrimRight(c.Endpoint, "/")
	c.Token = firstNonEmpty(*token, lookup("TOWAMORI_TOKEN"))

	c.Interval = DefaultInterval
	if *interval != 0 {
		c.Interval = *interval
	} else if v := lookup("TOWAMORI_INTERVAL"); v != "" {
		d, err := parseInterval(v)
		if err != nil {
			return nil, fmt.Errorf("TOWAMORI_INTERVAL: %w", err)
		}
		c.Interval = d
	}
	min, max := time.Duration(protocol.MinInterval)*time.Second, time.Duration(protocol.MaxInterval)*time.Second
	if c.Interval < min || c.Interval > max {
		return nil, fmt.Errorf("interval must be between %s and %s, got %s", min, max, c.Interval)
	}

	if c.Once {
		return c, nil
	}
	if c.Token == "" {
		return nil, errors.New("no token: pass -token or set TOWAMORI_TOKEN (see " + DefaultEnvFile + ")")
	}
	if err := CheckEndpoint(c.Endpoint); err != nil {
		return nil, err
	}
	return c, nil
}

// parseInterval accepts "15s", "1m" or a bare number of seconds.
func parseInterval(v string) (time.Duration, error) {
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	return time.ParseDuration(v)
}

// CheckEndpoint refuses plain HTTP to anything but the loopback interface, so
// the token is never sent unencrypted over a network.
func CheckEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return fmt.Errorf("endpoint %q is not a valid URL", endpoint)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
		return fmt.Errorf("endpoint %q uses plain http; only https is allowed except for localhost", endpoint)
	default:
		return fmt.Errorf("endpoint %q must start with https://", endpoint)
	}
}

// ReadEnvFile parses KEY=VALUE lines. Blank lines and lines starting with #
// are ignored; values may be wrapped in single or double quotes.
func ReadEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		s = strings.TrimPrefix(s, "export ")
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE", path, line)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	return out, sc.Err()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Redact shows only the first few characters of a token, for logs.
func Redact(token string) string {
	if len(token) <= 8 {
		return "****"
	}
	return token[:6] + "…"
}
