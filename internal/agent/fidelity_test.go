package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/miharp/codavox/internal/testca"
)

// publisher runs the real publish subcommand and can be restarted in place to
// stand in for a fresh deploy: the publisher seals its staging directory at
// startup, so pointing staging at new content and restarting is how a test
// advances the served code_id.
type publisher struct {
	bin     string
	staging string
	addr    string
	ssldir  string
	state   string
	cmd     *exec.Cmd
}

func (p *publisher) url() string { return "https://" + p.addr }

func (p *publisher) restart(t *testing.T) {
	t.Helper()
	p.stop()
	if p.state == "" {
		p.state = t.TempDir()
	}
	cmd := exec.Command(p.bin, "publish",
		"--staging", p.staging,
		"--listen", p.addr,
		"--certname", "puppet.example.com",
		"--ssldir", p.ssldir,
		"--state", p.state,
	)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p.cmd = cmd
}

func (p *publisher) stop() {
	if p.cmd != nil {
		_ = p.cmd.Process.Kill()
		_ = p.cmd.Wait()
		p.cmd = nil
	}
}

// hup signals the running publisher to reseal, without restarting it.
func (p *publisher) hup(t *testing.T) {
	t.Helper()
	if p.cmd == nil {
		t.Fatal("publisher is not running")
	}
	if err := p.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatalf("sending SIGHUP: %v", err)
	}
}

// codeContent reports what this compiler would answer for code-content-command.
func (c compiler) codeContent(t *testing.T, bin, env, codeID, path string) (string, error) {
	t.Helper()
	cmd := exec.Command(bin, "code-content", env, codeID, path)
	cmd.Env = append(os.Environ(), "CODAVOX_ROOT="+c.root, "CODAVOX_ENVIRONMENTPATH="+c.envPath)
	out, err := cmd.Output()
	return string(out), err
}

// versionDirs lists the unpacked version directories retained for env.
func (c compiler) versionDirs(t *testing.T, env string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(c.root, "versions"))
	if err != nil {
		t.Fatalf("reading versions directory: %v", err)
	}
	prefix := env + "_"
	var dirs []string
	for _, e := range entries {
		// Dot-prefixed entries are in-progress extractions, not deployed versions.
		if e.IsDir() && strings.HasPrefix(e.Name(), prefix) && !strings.HasPrefix(e.Name(), ".") {
			dirs = append(dirs, e.Name())
		}
	}
	return dirs
}

// syncReady polls syncOnceArgs until the (possibly just-restarted) publisher is
// accepting connections, then leaves the compiler converged onto it.
func syncReady(t *testing.T, c compiler, bin, publisher string, extra ...string) {
	t.Helper()
	var err error
	for range 40 {
		if err = c.syncOnceArgs(t, bin, publisher, extra...); err == nil {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("compiler never synced: %v", err)
}

// TestSIGHUPTriggersReseal checks the post-deploy trigger. r10k deploys new
// content into staging and signals the *running* publisher with SIGHUP — no
// restart — the way an r10k postrun hook would. The publisher must reseal and
// begin advertising the new code_id, and a compiler must then converge onto it.
func TestSIGHUPTriggersReseal(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary and binds a port")
	}

	bin := build(t)
	ca := testca.New(t)
	staging := t.TempDir()
	serverSSL := ca.SSLDir(t, "puppet.example.com", "openvox_server")

	pub := &publisher{bin: bin, staging: staging, addr: "127.0.0.1:18157", ssldir: serverSSL, state: t.TempDir()}
	t.Cleanup(pub.stop)
	c := newCompiler(t, ca, "compiler01.example.com")

	writeEnv(t, staging, "production", map[string]string{"manifests/site.pp": "v1\n"})
	pub.restart(t)
	syncReady(t, c, bin, pub.url())
	id1, err := c.codeID(t, bin, "production")
	if err != nil {
		t.Fatal(err)
	}

	// New deploy into the same running publisher, then the postrun signal.
	writeEnv(t, staging, "production", map[string]string{"manifests/site.pp": "v2\n"})
	pub.hup(t)

	// The reseal is asynchronous, so poll: converge and check whether the id
	// moved. Without the SIGHUP handler the publisher would still advertise id1
	// forever and this would time out.
	var id2 string
	for range 40 {
		_ = c.syncOnceArgs(t, bin, pub.url())
		if id2, err = c.codeID(t, bin, "production"); err == nil && id2 != id1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if id2 == id1 {
		t.Fatal("publisher did not reseal on SIGHUP; the compiler never saw the new version")
	}

	body, err := os.ReadFile(filepath.Join(c.envPath, "production", "manifests/site.pp"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "v2\n" {
		t.Errorf("content = %q, want v2 after a SIGHUP reseal", body)
	}
}

// TestContentFidelity is the guarantee that separates codavox from the shell
// baseline it replaces. After a compiler moves to a new version, code-content
// for the *previous* code_id must still return the previous version's bytes —
// the file an in-flight agent run, holding a catalog stamped with that id, will
// ask for — and a code_id that was never deployed must fail loudly rather than
// fall back to whatever is current. The baseline code_content.sh serves current
// content in both of those cases, which is the silent-wrong-version failure
// static catalogs exist to prevent.
func TestContentFidelity(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary and binds a port")
	}

	bin := build(t)
	ca := testca.New(t)
	staging := t.TempDir()
	serverSSL := ca.SSLDir(t, "puppet.example.com", "openvox_server")

	pub := &publisher{bin: bin, staging: staging, addr: "127.0.0.1:18154", ssldir: serverSSL}
	t.Cleanup(pub.stop)
	c := newCompiler(t, ca, "compiler01.example.com")

	// Deploy v1 and converge onto it.
	writeEnv(t, staging, "production", map[string]string{"manifests/site.pp": "v1\n"})
	pub.restart(t)
	syncReady(t, c, bin, pub.url())
	old, err := c.codeID(t, bin, "production")
	if err != nil {
		t.Fatal(err)
	}

	// Deploy v2 and converge onto it. The old version is now superseded but,
	// being recent, is still retained on disk.
	writeEnv(t, staging, "production", map[string]string{"manifests/site.pp": "v2\n"})
	pub.restart(t)
	syncReady(t, c, bin, pub.url())
	current, err := c.codeID(t, bin, "production")
	if err != nil {
		t.Fatal(err)
	}
	if current == old {
		t.Fatal("compiler did not move to the new version")
	}

	// The compiler now serves v2, but a catalog compiled against `old` must
	// still resolve to v1's bytes, not v2's.
	body, err := c.codeContent(t, bin, "production", old, "manifests/site.pp")
	if err != nil {
		t.Fatalf("code-content for the superseded version failed: %v", err)
	}
	if body != "v1\n" {
		t.Errorf("code-content for superseded %s = %q, want v1 — served the wrong version", old, body)
	}

	// The current id resolves to v2, so the two versions are genuinely distinct
	// on disk and the check above was not a tautology.
	body, err = c.codeContent(t, bin, "production", current, "manifests/site.pp")
	if err != nil {
		t.Fatal(err)
	}
	if body != "v2\n" {
		t.Errorf("code-content for current %s = %q, want v2", current, body)
	}

	// A syntactically valid code_id that was never deployed must fail, not fall
	// back to the current version. This is the case the baseline gets wrong.
	if out, err := c.codeContent(t, bin, "production", "deadbeefdeadbeef", "manifests/site.pp"); err == nil {
		t.Fatalf("code-content for an undeployed code_id succeeded (%q); it must fail rather than serve current content", out)
	}
}

// TestReapingRetainsInFlightVersions checks the two halves of the reaper. A
// superseded version young enough that an agent run could still be applying it
// is never deleted, even under keep pressure; once that age protection lapses,
// the reaper does bound the retained set. Deleting a tree an in-flight run
// still requests turns a successful run into a failed one, so the age floor is
// a correctness property, not a disk-space optimization.
func TestReapingRetainsInFlightVersions(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary and binds a port")
	}

	bin := build(t)
	ca := testca.New(t)
	serverSSL := ca.SSLDir(t, "puppet.example.com", "openvox_server")

	// deploy advances the publisher to new content, converges the compiler with
	// the given agent flags, and returns the resulting code_id. The short sleep
	// gives each version directory a distinct mtime so the reaper's newest-first
	// ordering is stable.
	deploy := func(t *testing.T, pub *publisher, c compiler, body string, extra ...string) string {
		t.Helper()
		writeEnv(t, pub.staging, "production", map[string]string{"manifests/site.pp": body})
		pub.restart(t)
		syncReady(t, c, bin, pub.url(), extra...)
		id, err := c.codeID(t, bin, "production")
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
		return id
	}

	// With the default --min-age (2h), every version deployed during the test is
	// too young to reap, so --keep 1 must not shrink the retained set below the
	// versions an in-flight run might still hold.
	t.Run("young versions survive keep pressure", func(t *testing.T) {
		staging := t.TempDir()
		pub := &publisher{bin: bin, staging: staging, addr: "127.0.0.1:18155", ssldir: serverSSL}
		t.Cleanup(pub.stop)
		c := newCompiler(t, ca, "compiler01.example.com")

		for _, v := range []string{"v1\n", "v2\n", "v3\n"} {
			deploy(t, pub, c, v, "--keep", "1")
		}

		if dirs := c.versionDirs(t, "production"); len(dirs) != 3 {
			t.Errorf("retained %d versions, want 3 — a version an in-flight run may hold was reaped: %v", len(dirs), dirs)
		}
	})

	// With the age floor removed (--min-age 1ns), the reaper keeps the current
	// version plus --keep superseded ones and drops the rest, so retention stays
	// bounded and the oldest tree is genuinely gone.
	t.Run("old versions past keep are reaped", func(t *testing.T) {
		staging := t.TempDir()
		pub := &publisher{bin: bin, staging: staging, addr: "127.0.0.1:18156", ssldir: serverSSL}
		t.Cleanup(pub.stop)
		c := newCompiler(t, ca, "compiler01.example.com")

		var ids []string
		for _, v := range []string{"v1\n", "v2\n", "v3\n"} {
			ids = append(ids, deploy(t, pub, c, v, "--keep", "1", "--min-age", "1ns"))
		}

		// current (v3) + 1 retained superseded (v2) = 2; v1 must be gone.
		if dirs := c.versionDirs(t, "production"); len(dirs) != 2 {
			t.Errorf("retained %d versions, want 2 (current + keep 1): %v", len(dirs), dirs)
		}
		if out, err := c.codeContent(t, bin, "production", ids[0], "manifests/site.pp"); err == nil {
			t.Errorf("oldest version %s still served (%q); it should have been reaped", ids[0], out)
		}
	})
}
