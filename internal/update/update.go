package update

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	// MaxArchive and MaxBinary bound what is downloaded and unpacked.
	MaxArchive = 40 << 20
	MaxBinary  = 64 << 20
	maxSig     = 4096

	// maxAttempts is how many times an updated build may be started without
	// getting a report through before the agent goes back.
	maxAttempts = 3

	// UpdatedEnv is set on the process that was started by an update, so that
	// it does not look for another one.
	UpdatedEnv = "TOWAMORI_UPDATED"
)

// Updater replaces the running agent with a newer signed build.
type Updater struct {
	StateDir string // where builds are kept; "" means this agent cannot update itself
	Current  string // the version that is running
	OS, Arch string
	Key      ed25519.PublicKey
	Log      *log.Logger

	// HTTP is the client for downloads; nil means a default one. Downloads
	// use https (plain http only to this machine, like the endpoint).
	HTTP *http.Client
	// Exec starts the new build in place of this process. It is syscall.Exec,
	// and only returns when that failed.
	Exec func(path string, argv []string, env []string) error
}

// New is an Updater for this process. stateDir is where the service manager
// lets it write (systemd's STATE_DIRECTORY).
func New(stateDir, current string, logger *log.Logger) *Updater {
	u := &Updater{StateDir: stateDir, Current: current, OS: goos(), Arch: goarch(), Log: logger, Exec: syscall.Exec}
	if key, err := PublicKey(); err == nil {
		u.Key = key
	}
	return u
}

// StateDirFromEnv picks the folder updates are kept in: TOWAMORI_STATE_DIR,
// else what systemd calls STATE_DIRECTORY.
func StateDirFromEnv(getenv func(string) string) string {
	for _, k := range []string{"TOWAMORI_STATE_DIR", "STATE_DIRECTORY"} {
		if v := getenv(k); v != "" {
			p, _, _ := strings.Cut(v, ":")
			return p
		}
	}
	return ""
}

// Supported reports whether this agent can update itself: it has a folder it
// may write to, a release key, and is a release build.
func (u *Updater) Supported() bool {
	if u == nil || u.StateDir == "" || len(u.Key) == 0 || !ValidVersion(u.Current) {
		return false
	}
	if err := os.MkdirAll(u.binDir(), 0o755); err != nil {
		return false
	}
	f, err := os.CreateTemp(u.binDir(), ".probe-*")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}

func (u *Updater) binDir() string    { return filepath.Join(u.StateDir, "bin") }
func (u *Updater) statePath() string { return filepath.Join(u.StateDir, "update.json") }
func (u *Updater) logf(format string, a ...any) {
	if u.Log != nil {
		u.Log.Printf(format, a...)
	}
}

// state is what is kept between runs.
type state struct {
	Version   string   `json:"version,omitempty"` // the staged build, "" when none
	Binary    string   `json:"binary,omitempty"`  // file names inside bin/
	Sig       string   `json:"sig,omitempty"`
	Attempts  int      `json:"attempts,omitempty"`
	Failed    []string `json:"failed,omitempty"` // versions that did not work; not offered again
	LastError string   `json:"last_error,omitempty"`
	ErrorFor  string   `json:"error_for,omitempty"`
}

func (u *Updater) load() state {
	var st state
	if raw, err := os.ReadFile(u.statePath()); err == nil {
		_ = json.Unmarshal(raw, &st)
	}
	return st
}

func (u *Updater) save(st state) error {
	raw, _ := json.Marshal(st)
	tmp := u.statePath() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, u.statePath())
}

// Failure takes the last failed update (if any) so that it can be reported
// to the platform once.
func (u *Updater) Failure() (version, msg string) {
	if u == nil || u.StateDir == "" {
		return "", ""
	}
	st := u.load()
	if st.LastError == "" {
		return "", ""
	}
	v, m := st.ErrorFor, st.LastError
	st.LastError, st.ErrorFor = "", ""
	_ = u.save(st)
	return v, m
}

// Apply downloads, verifies and installs version from base (a https URL of a
// folder that has towamori-agent_<os>_<arch>.tar.gz and a .sig next to it),
// then starts it in place of this process. It only returns when something
// went wrong, or in tests where Exec returns.
func (u *Updater) Apply(ctx context.Context, version, base string) error {
	if !u.Supported() {
		return errors.New("this agent cannot update itself (no writable state folder or not a release build)")
	}
	if !ValidVersion(version) {
		return fmt.Errorf("the platform offered %q, which is not a release number", version)
	}
	if !Newer(version, u.Current) {
		return fmt.Errorf("%s is not newer than %s", version, u.Current)
	}
	st := u.load()
	for _, f := range st.Failed {
		if f == version {
			return fmt.Errorf("%s already failed to start on this server; not trying again", version)
		}
	}
	root, err := u.checkBase(base)
	if err != nil {
		return err
	}
	file := fmt.Sprintf("towamori-agent_%s_%s.tar.gz", u.OS, u.Arch)

	sigRaw, err := u.fetch(ctx, root+"/"+file+".sig", maxSig)
	if err != nil {
		return fmt.Errorf("downloading the signature: %w", err)
	}
	stmt, err := Parse(u.Key, sigRaw)
	if err != nil {
		return fmt.Errorf("the signature is not valid: %w", err)
	}
	if stmt.Version != version || stmt.OS != u.OS || stmt.Arch != u.Arch {
		return fmt.Errorf("the signature is for %s %s/%s, not %s %s/%s", stmt.Version, stmt.OS, stmt.Arch, version, u.OS, u.Arch)
	}

	if err := os.MkdirAll(u.binDir(), 0o755); err != nil {
		return err
	}
	archive, err := os.CreateTemp(u.binDir(), ".dl-*")
	if err != nil {
		return err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	sum := sha256.New()
	if err := u.fetchTo(ctx, root+"/"+file, io.MultiWriter(archive, sum), MaxArchive); err != nil {
		return fmt.Errorf("downloading %s: %w", file, err)
	}
	if hex.EncodeToString(sum.Sum(nil)) != stmt.ArchiveSHA {
		return errors.New("the download does not match the signed checksum; discarded")
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	name := fmt.Sprintf("towamori-agent-%s", version)
	bin, err := extractBinary(archive, u.binDir(), name+".tmp", stmt.BinarySHA)
	if err != nil {
		return err
	}
	final := filepath.Join(u.binDir(), name)
	if err := os.Rename(bin, final); err != nil {
		os.Remove(bin)
		return err
	}
	sigName := name + ".sig"
	if err := os.WriteFile(filepath.Join(u.binDir(), sigName), sigRaw, 0o600); err != nil {
		return err
	}
	st.Version, st.Binary, st.Sig, st.Attempts = version, name, sigName, 0
	if err := u.save(st); err != nil {
		return err
	}
	// Starting the new build replaces this process, so deferred clean-up would never run.
	archive.Close()
	os.Remove(archive.Name())
	u.logf("update: %s verified and installed; restarting into it", version)
	return u.start(final, []string{UpdatedEnv + "=1"})
}

// start runs the build at path in place of this process.
func (u *Updater) start(path string, extraEnv []string) error {
	argv := append([]string{path}, os.Args[1:]...)
	env := append(os.Environ(), extraEnv...)
	if err := u.Exec(path, argv, env); err != nil {
		return fmt.Errorf("could not start %s: %w", filepath.Base(path), err)
	}
	return nil
}

// Relaunch is called first thing when the agent starts. If an update was
// installed earlier and this process is an older build (systemd always starts
// the one install.sh put in /usr/local/bin), it re-checks the signature and
// starts the newer build instead. It returns when there is nothing to do or
// the staged build cannot be used.
func (u *Updater) Relaunch() {
	if u == nil || u.StateDir == "" || os.Getenv(UpdatedEnv) != "" || len(u.Key) == 0 {
		return
	}
	st := u.load()
	if st.Version == "" {
		return
	}
	if !Newer(st.Version, u.Current) {
		// A reinstall put something at least as new there; the staged build is stale.
		u.discard(&st, "")
		return
	}
	if st.Attempts >= maxAttempts {
		u.discard(&st, fmt.Sprintf("%s did not report in %d tries; went back to %s", st.Version, maxAttempts, u.Current))
		return
	}
	bin, err := u.verifyStaged(st)
	if err != nil {
		u.discard(&st, fmt.Sprintf("%s failed its check (%v); went back to %s", st.Version, err, u.Current))
		return
	}
	st.Attempts++
	if err := u.save(st); err != nil {
		u.logf("update: cannot save state: %v", err)
		return
	}
	if err := u.start(bin, []string{UpdatedEnv + "=1"}); err != nil {
		u.logf("update: %v; running %s instead", err, u.Current)
	}
}

// verifyStaged checks the staged build against its signature again.
func (u *Updater) verifyStaged(st state) (string, error) {
	if st.Binary == "" || st.Sig == "" || strings.ContainsAny(st.Binary+st.Sig, "/\\") {
		return "", errors.New("incomplete record")
	}
	sigRaw, err := os.ReadFile(filepath.Join(u.binDir(), st.Sig))
	if err != nil {
		return "", err
	}
	stmt, err := Parse(u.Key, sigRaw)
	if err != nil {
		return "", err
	}
	if stmt.Version != st.Version || stmt.OS != u.OS || stmt.Arch != u.Arch {
		return "", errors.New("the signed statement is for something else")
	}
	path := filepath.Join(u.binDir(), st.Binary)
	got, err := fileSHA(path)
	if err != nil {
		return "", err
	}
	if got != stmt.BinarySHA {
		return "", errors.New("the program does not match its signed checksum")
	}
	return path, nil
}

// discard forgets the staged build; reason is kept to be reported once.
func (u *Updater) discard(st *state, reason string) {
	if st.Binary != "" && !strings.ContainsAny(st.Binary, "/\\") {
		os.Remove(filepath.Join(u.binDir(), st.Binary))
	}
	if st.Sig != "" && !strings.ContainsAny(st.Sig, "/\\") {
		os.Remove(filepath.Join(u.binDir(), st.Sig))
	}
	if reason != "" {
		st.Failed = append(st.Failed, st.Version)
		st.LastError, st.ErrorFor = reason, st.Version
		u.logf("update: %s", reason)
	}
	st.Version, st.Binary, st.Sig, st.Attempts = "", "", "", 0
	_ = u.save(*st)
}

// Confirm is called after the first report that the platform accepted. It
// tells the next start that the build the agent is running works.
func (u *Updater) Confirm() {
	if u == nil || u.StateDir == "" {
		return
	}
	st := u.load()
	if st.Version == "" || st.Version != u.Current {
		return
	}
	if st.Attempts != 0 {
		st.Attempts = 0
		_ = u.save(st)
	}
	// Older staged builds and half-finished downloads are no longer needed.
	entries, _ := os.ReadDir(u.binDir())
	for _, e := range entries {
		n := e.Name()
		if (strings.HasPrefix(n, "towamori-agent-") && !strings.HasPrefix(n, st.Binary)) || strings.HasPrefix(n, ".dl-") || strings.HasPrefix(n, ".probe-") {
			os.Remove(filepath.Join(u.binDir(), n))
		}
	}
}

// ---- downloads ----

func (u *Updater) checkBase(base string) (string, error) {
	p, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || p.Host == "" || p.User != nil || p.RawQuery != "" || p.Fragment != "" {
		return "", errors.New("the platform offered a bad download address")
	}
	if !allowedScheme(p) {
		return "", errors.New("downloads must use https")
	}
	return p.String(), nil
}

// allowedScheme is https, or http to this machine (for development).
func allowedScheme(p *url.URL) bool {
	if p.Scheme == "https" {
		return true
	}
	if p.Scheme != "http" {
		return false
	}
	h := p.Hostname()
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func (u *Updater) client() *http.Client {
	if u.HTTP != nil {
		return u.HTTP
	}
	return &http.Client{
		Timeout: 10 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			if !allowedScheme(req.URL) {
				return errors.New("redirect to a non-https address")
			}
			return nil
		},
	}
}

func (u *Updater) fetch(ctx context.Context, address string, max int64) ([]byte, error) {
	var buf limitedBuffer
	buf.max = max
	if err := u.fetchTo(ctx, address, &buf, max); err != nil {
		return nil, err
	}
	return buf.b, nil
}

type limitedBuffer struct {
	b   []byte
	max int64
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if int64(len(l.b)+len(p)) > l.max {
		return 0, errors.New("too large")
	}
	l.b = append(l.b, p...)
	return len(p), nil
}

func (u *Updater) fetchTo(ctx context.Context, address string, w io.Writer, max int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "towamori-agent/"+u.Current+" ("+u.OS+"/"+u.Arch+") update")
	resp, err := u.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > max {
		return errors.New("too large")
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, max+1))
	if err != nil {
		return err
	}
	if n > max {
		return errors.New("too large")
	}
	return nil
}

// ---- unpacking ----

// extractBinary writes the file named towamori-agent from a .tar.gz into dir
// (as name, mode 0755) and checks its SHA-256. Every other entry is ignored.
func extractBinary(r io.Reader, dir, name, wantSHA string) (string, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return "", fmt.Errorf("the archive is not a gzip file: %w", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return "", errors.New("the archive has no towamori-agent in it")
		}
		if err != nil {
			return "", err
		}
		if h.Typeflag != tar.TypeReg || h.Name != "towamori-agent" {
			continue
		}
		if h.Size <= 0 || h.Size > MaxBinary {
			return "", errors.New("the program in the archive has an unreasonable size")
		}
		path := filepath.Join(dir, name)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
		if err != nil {
			return "", err
		}
		sum := sha256.New()
		n, err := io.Copy(io.MultiWriter(f, sum), io.LimitReader(tr, MaxBinary+1))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil && (n != h.Size || n > MaxBinary) {
			err = errors.New("the program in the archive is the wrong size")
		}
		if err == nil && hex.EncodeToString(sum.Sum(nil)) != wantSHA {
			err = errors.New("the program in the archive does not match the signed checksum")
		}
		if err != nil {
			os.Remove(path)
			return "", err
		}
		return path, nil
	}
}

// BinarySHA is the SHA-256 of the towamori-agent file in a .tar.gz, for the
// signing tool.
func BinarySHA(r io.Reader) (string, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return "", err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return "", errors.New("no towamori-agent in the archive")
		}
		if err != nil {
			return "", err
		}
		if h.Typeflag == tar.TypeReg && h.Name == "towamori-agent" {
			sum := sha256.New()
			if _, err := io.Copy(sum, io.LimitReader(tr, MaxBinary+1)); err != nil {
				return "", err
			}
			return hex.EncodeToString(sum.Sum(nil)), nil
		}
	}
}

func fileSHA(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}
