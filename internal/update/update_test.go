package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func archiveWith(t *testing.T, program []byte, extra ...[2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, f := range append([][2]string{{"LICENSE", "x"}}, extra...) {
		tw.WriteHeader(&tar.Header{Name: f[0], Mode: 0o644, Size: int64(len(f[1])), Typeflag: tar.TypeReg})
		tw.Write([]byte(f[1]))
	}
	tw.WriteHeader(&tar.Header{Name: "towamori-agent", Mode: 0o755, Size: int64(len(program)), Typeflag: tar.TypeReg})
	tw.Write(program)
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

type release struct {
	pub     ed25519.PublicKey
	priv    ed25519.PrivateKey
	archive []byte
	sig     []byte
	stmt    Statement
	program []byte
}

func newRelease(t *testing.T, version string) *release {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	return releaseWith(t, pub, priv, version)
}

// releaseWith makes a release signed by the given key, so that several can be
// told apart by one agent.
func releaseWith(t *testing.T, pub ed25519.PublicKey, priv ed25519.PrivateKey, version string) *release {
	t.Helper()
	prog := []byte("#!/bin/sh\necho new build " + version + "\n")
	arc := archiveWith(t, prog)
	st := Statement{Version: version, OS: goos(), Arch: goarch(), ArchiveSHA: sha(arc), BinarySHA: sha(prog)}
	sig, err := Sign(priv, st)
	if err != nil {
		t.Fatal(err)
	}
	return &release{pub: pub, priv: priv, archive: arc, sig: sig, stmt: st, program: prog}
}

func (r *release) server(t *testing.T) *httptest.Server {
	t.Helper()
	name := "/dl/towamori-agent_" + goos() + "_" + goarch() + ".tar.gz"
	mux := http.NewServeMux()
	mux.HandleFunc(name, func(w http.ResponseWriter, _ *http.Request) { w.Write(r.archive) })
	mux.HandleFunc(name+".sig", func(w http.ResponseWriter, _ *http.Request) { w.Write(r.sig) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

type exec struct {
	path string
	argv []string
	env  []string
	n    int
}

func (e *exec) fn(path string, argv, env []string) error {
	e.path, e.argv, e.env = path, argv, env
	e.n++
	return nil
}

func updater(t *testing.T, r *release, current string) (*Updater, *exec) {
	t.Helper()
	e := &exec{}
	return &Updater{StateDir: t.TempDir(), Current: current, OS: goos(), Arch: goarch(), Key: r.pub, Exec: e.fn}, e
}

func TestSignAndParse(t *testing.T) {
	r := newRelease(t, "0.3.0")
	got, err := Parse(r.pub, r.sig)
	if err != nil || got != r.stmt {
		t.Fatalf("Parse = %+v, %v; want %+v", got, err, r.stmt)
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := Parse(other, r.sig); err == nil {
		t.Error("a signature made with another key was accepted")
	}
	bad := bytes.Replace(r.sig, []byte("version=0.3.0"), []byte("version=9.9.9"), 1)
	if _, err := Parse(r.pub, bad); err == nil {
		t.Error("a statement that was edited after signing was accepted")
	}
	// A signature is for statements only: signing an arbitrary text must not verify as one.
	text := []byte("hello\nsignature=")
	if _, err := Parse(r.pub, text); err == nil {
		t.Error("garbage was accepted")
	}
	if _, err := Parse(r.pub, nil); err == nil {
		t.Error("empty file was accepted")
	}
	// Extra fields smuggled in before the signature line change the signed text.
	extra := bytes.Replace(r.sig, []byte("signature="), []byte("note=hi\nsignature="), 1)
	if _, err := Parse(r.pub, extra); err == nil {
		t.Error("an unsigned extra field was accepted")
	}
}

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"0.3.0", "0.2.0", true}, {"0.2.0", "0.3.0", false}, {"0.2.0", "0.2.0", false},
		{"1.0.0", "0.9.9", true}, {"0.10.0", "0.9.0", true}, {"0.3.0", "dev", false}, {"dev", "0.3.0", false},
		{"0.3.0", "0.3.0-dev", false}, {"", "0.1.0", false},
	} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestApplyInstallsAndStarts(t *testing.T) {
	r := newRelease(t, "0.3.0")
	srv := r.server(t)
	u, e := updater(t, r, "0.2.0")
	if err := u.Apply(context.Background(), "0.3.0", srv.URL+"/dl"); err != nil {
		t.Fatal(err)
	}
	if e.n != 1 || filepath.Base(e.path) != "towamori-agent-0.3.0" {
		t.Fatalf("exec = %+v", e)
	}
	if !contains(e.env, UpdatedEnv+"=1") {
		t.Error("the new process is not told it was started by an update")
	}
	got, _ := os.ReadFile(e.path)
	if !bytes.Equal(got, r.program) {
		t.Error("installed program differs from the one in the archive")
	}
	if st := u.load(); st.Version != "0.3.0" || st.Attempts != 0 {
		t.Errorf("state = %+v", st)
	}
	if info, _ := os.Stat(e.path); info.Mode()&0o111 == 0 {
		t.Error("program is not executable")
	}
	// Nothing but the program and its signature may be left behind: the process is replaced
	// by the new build, so a deferred clean-up would never run.
	entries, _ := os.ReadDir(filepath.Join(u.StateDir, "bin"))
	if len(entries) != 2 {
		t.Errorf("files left in bin/: %v", entries)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestApplyRefuses(t *testing.T) {
	r := newRelease(t, "0.3.0")
	srv := r.server(t)
	ctx := context.Background()

	t.Run("not newer", func(t *testing.T) {
		u, e := updater(t, r, "0.3.0")
		if err := u.Apply(ctx, "0.3.0", srv.URL+"/dl"); err == nil || e.n != 0 {
			t.Errorf("err=%v exec=%d", err, e.n)
		}
	})
	t.Run("not a release number", func(t *testing.T) {
		u, e := updater(t, r, "0.2.0")
		if err := u.Apply(ctx, "latest", srv.URL+"/dl"); err == nil || e.n != 0 {
			t.Errorf("err=%v exec=%d", err, e.n)
		}
	})
	t.Run("signature for another version", func(t *testing.T) {
		u, e := updater(t, r, "0.2.0")
		if err := u.Apply(ctx, "0.4.0", srv.URL+"/dl"); err == nil || e.n != 0 {
			t.Errorf("err=%v exec=%d", err, e.n)
		}
	})
	t.Run("wrong key", func(t *testing.T) {
		u, e := updater(t, r, "0.2.0")
		other, _, _ := ed25519.GenerateKey(rand.Reader)
		u.Key = other
		if err := u.Apply(ctx, "0.3.0", srv.URL+"/dl"); err == nil || e.n != 0 {
			t.Errorf("err=%v exec=%d", err, e.n)
		}
	})
	t.Run("archive swapped after signing", func(t *testing.T) {
		evil := *r
		evil.archive = archiveWith(t, []byte("malware"))
		bad := evil.server(t)
		u, e := updater(t, r, "0.2.0")
		err := u.Apply(ctx, "0.3.0", bad.URL+"/dl")
		if err == nil || e.n != 0 {
			t.Fatalf("err=%v exec=%d", err, e.n)
		}
		if entries, _ := os.ReadDir(filepath.Join(u.StateDir, "bin")); len(entries) != 0 {
			t.Errorf("left files behind: %v", entries)
		}
	})
	t.Run("program inside does not match", func(t *testing.T) {
		// The archive checksum is right (signed that way) but the program in it is not the signed one.
		evil := *r
		evil.archive = archiveWith(t, []byte("malware"))
		st := r.stmt
		st.ArchiveSHA = sha(evil.archive)
		evil.sig, _ = Sign(r.priv, st) // BinarySHA still names the honest program
		bad := evil.server(t)
		u, e := updater(t, r, "0.2.0")
		if err := u.Apply(ctx, "0.3.0", bad.URL+"/dl"); err == nil || e.n != 0 {
			t.Errorf("err=%v exec=%d", err, e.n)
		}
	})
	t.Run("plain http to somewhere else", func(t *testing.T) {
		u, e := updater(t, r, "0.2.0")
		for _, base := range []string{"http://updates.example.com/dl", "ftp://x/dl", "https://user:pw@x/dl", "https://x/dl?a=b", "/dl", ""} {
			if err := u.Apply(ctx, "0.3.0", base); err == nil || e.n != 0 {
				t.Errorf("%q: err=%v exec=%d", base, err, e.n)
			}
		}
	})
	t.Run("no state folder", func(t *testing.T) {
		u, e := updater(t, r, "0.2.0")
		u.StateDir = ""
		if err := u.Apply(ctx, "0.3.0", srv.URL+"/dl"); err == nil || e.n != 0 {
			t.Errorf("err=%v exec=%d", err, e.n)
		}
		if u.Supported() {
			t.Error("Supported without a state folder")
		}
	})
	t.Run("archive with the program missing", func(t *testing.T) {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(zw)
		tw.WriteHeader(&tar.Header{Name: "../towamori-agent", Mode: 0o755, Size: 1, Typeflag: tar.TypeReg})
		tw.Write([]byte("x"))
		tw.Close()
		zw.Close()
		evil := *r
		evil.archive = buf.Bytes()
		st := r.stmt
		st.ArchiveSHA = sha(evil.archive)
		evil.sig, _ = Sign(r.priv, st)
		bad := evil.server(t)
		u, e := updater(t, r, "0.2.0")
		if err := u.Apply(ctx, "0.3.0", bad.URL+"/dl"); err == nil || e.n != 0 {
			t.Errorf("err=%v exec=%d", err, e.n)
		}
	})
}

func TestRelaunchAndRollback(t *testing.T) {
	r := newRelease(t, "0.3.0")
	srv := r.server(t)
	u, e := updater(t, r, "0.2.0")
	if err := u.Apply(context.Background(), "0.3.0", srv.URL+"/dl"); err != nil {
		t.Fatal(err)
	}
	e.n = 0

	// Process started by the update itself does not look again.
	t.Setenv(UpdatedEnv, "1")
	u.Relaunch()
	if e.n != 0 {
		t.Fatal("relaunched from the updated process")
	}
	t.Setenv(UpdatedEnv, "")
	os.Unsetenv(UpdatedEnv)

	// systemd starts the old build: it moves to the new one, three times, then gives up.
	for i := 1; i <= maxAttempts; i++ {
		u.Relaunch()
		if e.n != i {
			t.Fatalf("attempt %d: exec count %d", i, e.n)
		}
		if st := u.load(); st.Attempts != i {
			t.Fatalf("attempt %d: attempts=%d", i, st.Attempts)
		}
	}
	u.Relaunch()
	if e.n != maxAttempts {
		t.Fatal("kept trying after the limit")
	}
	st := u.load()
	if st.Version != "" || len(st.Failed) != 1 || st.Failed[0] != "0.3.0" || st.LastError == "" {
		t.Fatalf("state after giving up: %+v", st)
	}
	if v, msg := u.Failure(); v != "0.3.0" || !strings.Contains(msg, "went back") {
		t.Errorf("Failure() = %q, %q", v, msg)
	}
	if v, _ := u.Failure(); v != "" {
		t.Error("the failure was reported twice")
	}
	// It is not offered again.
	if err := u.Apply(context.Background(), "0.3.0", srv.URL+"/dl"); err == nil {
		t.Error("tried a version that already failed")
	}
}

func TestRelaunchRejectsTamperedBuild(t *testing.T) {
	r := newRelease(t, "0.3.0")
	srv := r.server(t)
	u, e := updater(t, r, "0.2.0")
	if err := u.Apply(context.Background(), "0.3.0", srv.URL+"/dl"); err != nil {
		t.Fatal(err)
	}
	e.n = 0
	st := u.load()
	if err := os.WriteFile(filepath.Join(u.binDir(), st.Binary), []byte("malware"), 0o755); err != nil {
		t.Fatal(err)
	}
	u.Relaunch()
	if e.n != 0 {
		t.Fatal("started a program that does not match its signature")
	}
	if st := u.load(); st.Version != "" || st.LastError == "" {
		t.Errorf("state = %+v", st)
	}
}

func TestConfirmAndStaleState(t *testing.T) {
	r := newRelease(t, "0.3.0")
	srv := r.server(t)
	u, e := updater(t, r, "0.2.0")
	if err := u.Apply(context.Background(), "0.3.0", srv.URL+"/dl"); err != nil {
		t.Fatal(err)
	}
	u.Relaunch() // attempts = 1
	// Now we are the updated process.
	u.Current = "0.3.0"
	u.Confirm()
	if st := u.load(); st.Attempts != 0 || st.Version != "0.3.0" {
		t.Errorf("after confirm: %+v", st)
	}
	// A reinstall puts an equal or newer built-in binary there: the staged one is dropped quietly.
	u.Current = "0.3.0"
	e.n = 0
	u.Relaunch()
	if e.n != 0 {
		t.Error("relaunched into a build that is not newer")
	}
	if st := u.load(); st.Version != "" || st.LastError != "" {
		t.Errorf("stale state not cleaned: %+v", st)
	}
}

func TestStateDirFromEnv(t *testing.T) {
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if got := StateDirFromEnv(get(map[string]string{"STATE_DIRECTORY": "/var/lib/towamori"})); got != "/var/lib/towamori" {
		t.Errorf("got %q", got)
	}
	if got := StateDirFromEnv(get(map[string]string{"TOWAMORI_STATE_DIR": "/x", "STATE_DIRECTORY": "/y:/z"})); got != "/x" {
		t.Errorf("got %q", got)
	}
	if got := StateDirFromEnv(get(map[string]string{"STATE_DIRECTORY": "/a:/b"})); got != "/a" {
		t.Errorf("got %q", got)
	}
	if got := StateDirFromEnv(get(nil)); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestFailedUpdateGoesBackToThePreviousBuild(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	r1, r2 := releaseWith(t, pub, priv, "0.3.1"), releaseWith(t, pub, priv, "0.3.2")
	s1, s2 := r1.server(t), r2.server(t)
	u, e := updater(t, r1, "0.3.0") // the installed build is 0.3.0
	ctx := context.Background()

	if err := u.Apply(ctx, "0.3.1", s1.URL+"/dl"); err != nil {
		t.Fatal(err)
	}
	// Now the process is 0.3.1 and has reported: it works.
	u.Current = "0.3.1"
	u.Confirm()
	if err := u.Apply(ctx, "0.3.2", s2.URL+"/dl"); err != nil {
		t.Fatal(err)
	}
	if st := u.load(); st.Version != "0.3.2" || st.Prev == nil || st.Prev.Version != "0.3.1" {
		t.Fatalf("state after the second update: %+v", st)
	}

	// 0.3.2 never gets a report through; systemd keeps starting the installed 0.3.0.
	u.Current = "0.3.0"
	e.n = 0
	for i := 1; i <= maxAttempts; i++ {
		u.Relaunch()
		if filepath.Base(e.path) != "towamori-agent-0.3.2" || e.n != i {
			t.Fatalf("attempt %d: exec %q (%d)", i, e.path, e.n)
		}
	}
	u.Relaunch() // gives up on 0.3.2 and starts 0.3.1 instead of falling all the way back
	if filepath.Base(e.path) != "towamori-agent-0.3.1" {
		t.Fatalf("started %q, want 0.3.1", e.path)
	}
	st := u.load()
	if st.Version != "0.3.1" || st.Prev != nil || len(st.Failed) != 1 || st.Failed[0] != "0.3.2" || !strings.Contains(st.LastError, "went back to 0.3.1") {
		t.Fatalf("state after the fall back: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(u.binDir(), "towamori-agent-0.3.2")); err == nil {
		t.Error("the failed build was kept")
	}
	// 0.3.1 comes up and reports: nothing else is left lying around.
	u.Current = "0.3.1"
	u.Confirm()
	entries, _ := os.ReadDir(u.binDir())
	if len(entries) != 2 {
		t.Errorf("files in bin/: %v", entries)
	}
}
