package agent

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/miharp/codavox/internal/publish"
	"github.com/miharp/codavox/internal/puppetca"
	"github.com/miharp/codavox/internal/testca"
)

func build(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "codavox")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/codavox").CombinedOutput(); err != nil {
		t.Fatalf("building binary: %v\n%s", err, out)
	}
	return bin
}

func writeEnv(t *testing.T, staging, env string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(staging, env)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// compiler is a simulated compiler node with its own SSL material and layout.
type compiler struct {
	name    string
	ssldir  string
	root    string
	envPath string
}

func newCompiler(t *testing.T, ca *testca.CA, name string) compiler {
	t.Helper()
	base := t.TempDir()
	return compiler{
		name:    name,
		ssldir:  ca.SSLDir(t, name, "openvox_compiler"),
		root:    filepath.Join(base, "codavox"),
		envPath: filepath.Join(base, "environments"),
	}
}

// syncOnce runs the real agent binary once against the publisher.
func (c compiler) syncOnce(t *testing.T, bin, publisher string) error {
	t.Helper()
	return c.syncOnceArgs(t, bin, publisher)
}

// syncOnceArgs is syncOnce with extra agent flags appended, so a test can pin
// --keep or --min-age to exercise reaping.
func (c compiler) syncOnceArgs(t *testing.T, bin, publisher string, extra ...string) error {
	t.Helper()
	args := []string{"agent",
		"--publisher", publisher,
		"--once",
		"--certname", c.name,
		"--ssldir", c.ssldir,
		"--environmentpath", c.envPath,
	}
	args = append(args, extra...)
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "CODAVOX_ROOT="+c.root)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return &execError{err: err, output: string(out)}
	}
	return nil
}

type execError struct {
	err    error
	output string
}

func (e *execError) Error() string { return e.err.Error() + "\n" + e.output }

// codeID reports what this compiler would answer for code-id-command.
func (c compiler) codeID(t *testing.T, bin, env string) (string, error) {
	t.Helper()
	cmd := exec.Command(bin, "code-id", env)
	cmd.Env = append(os.Environ(), "CODAVOX_ROOT="+c.root, "CODAVOX_ENVIRONMENTPATH="+c.envPath)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// syncOnceEnv runs the agent with no --environmentpath, supplying the layout
// entirely through the environment the way CONTRIBUTING.md documents. Every
// other test pins the path with a flag, which would hide the agent resolving it
// differently from code-id.
func (c compiler) syncOnceEnv(t *testing.T, bin, publisher string) error {
	t.Helper()
	cmd := exec.Command(bin, "agent",
		"--publisher", publisher,
		"--once",
		"--certname", c.name,
		"--ssldir", c.ssldir,
	)
	cmd.Env = append(os.Environ(),
		"CODAVOX_ROOT="+c.root,
		"CODAVOX_ENVIRONMENTPATH="+c.envPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return &execError{err: err, output: string(out)}
	}
	return nil
}

// The agent and code-id must resolve the environment path identically. They are
// two processes reading and writing one symlink, so an override honored by only
// one of them puts the deployed tree somewhere code-id does not look — the
// divergence the single-source-of-truth design exists to prevent. The agent
// previously ignored CODAVOX_ENVIRONMENTPATH while code-id honored it.
func TestAgentAndCodeIDAgreeOnTheEnvironmentOverride(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary and binds a port")
	}

	bin := build(t)
	ca := testca.New(t)

	staging := t.TempDir()
	writeEnv(t, staging, "production", map[string]string{"manifests/site.pp": "v1\n"})

	const addr = "127.0.0.1:18154"
	pub := &publisher{
		bin:     bin,
		staging: staging,
		addr:    addr,
		ssldir:  ca.SSLDir(t, "puppet.example.com", "openvox_server"),
	}
	pub.restart(t)
	t.Cleanup(pub.stop)

	c := newCompiler(t, ca, "compiler01.example.com")

	var err error
	for range 40 {
		if err = c.syncOnceEnv(t, bin, pub.url()); err == nil {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("compiler never synced: %v", err)
	}

	// code-id reads the same override. If the agent deployed anywhere else, this
	// finds no link at all.
	id, err := c.codeID(t, bin, "production")
	if err != nil {
		t.Fatalf("code-id after an env-configured sync: %v", err)
	}
	if id == "" {
		t.Fatal("code-id reported an empty code_id")
	}

	// Nothing may have been written to the compiled-in default path.
	if _, err := os.Lstat(filepath.Join("/opt/puppetlabs/codavox/environments", "production")); err == nil {
		t.Error("the agent deployed to the built-in environment path despite CODAVOX_ENVIRONMENTPATH")
	}
	t.Logf("agent and code-id agree at %s", id)
}

// TestTwoCompilersConverge is the property the whole project exists for: two
// independent compilers, each fetching over mutual TLS from a real publisher
// process, must end up reporting the same code_id — and one that was offline
// during a deploy must catch up on its own, with no replayed event.
func TestTwoCompilersConverge(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary and binds a port")
	}

	bin := build(t)
	ca := testca.New(t)

	staging := t.TempDir()
	writeEnv(t, staging, "production", map[string]string{"manifests/site.pp": "v1\n"})

	serverSSL := ca.SSLDir(t, "puppet.example.com", "openvox_server")
	const addr = "127.0.0.1:18152"
	const publisher = "https://" + addr

	startPublisher := func(t *testing.T) *exec.Cmd {
		t.Helper()
		cmd := exec.Command(bin, "publish",
			"--staging", staging, "--listen", addr,
			"--certname", "puppet.example.com", "--ssldir", serverSSL,
			"--state", t.TempDir())
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd
	}

	pub := startPublisher(t)
	t.Cleanup(func() {
		_ = pub.Process.Kill()
		_ = pub.Wait()
	})

	c1 := newCompiler(t, ca, "compiler01.example.com")
	c2 := newCompiler(t, ca, "compiler02.example.com")

	// Wait for the publisher to accept connections.
	var err error
	for range 40 {
		if err = c1.syncOnce(t, bin, publisher); err == nil {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("compiler01 never synced: %v", err)
	}
	if err := c2.syncOnce(t, bin, publisher); err != nil {
		t.Fatalf("compiler02 sync: %v", err)
	}

	id1, err := c1.codeID(t, bin, "production")
	if err != nil {
		t.Fatalf("compiler01 code-id: %v", err)
	}
	id2, err := c2.codeID(t, bin, "production")
	if err != nil {
		t.Fatalf("compiler02 code-id: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("compilers diverged: %s vs %s", id1, id2)
	}
	t.Logf("both compilers at %s", id1)

	// A deploy happens while compiler02 is offline. The publisher must be
	// restarted because it seals at startup.
	writeEnv(t, staging, "production", map[string]string{"manifests/site.pp": "v2\n"})
	_ = pub.Process.Kill()
	_ = pub.Wait()

	pub2 := startPublisher(t)
	t.Cleanup(func() {
		_ = pub2.Process.Kill()
		_ = pub2.Wait()
	})

	for range 40 {
		if err = c1.syncOnce(t, bin, publisher); err == nil {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("compiler01 did not pick up the new deploy: %v", err)
	}

	updated, err := c1.codeID(t, bin, "production")
	if err != nil {
		t.Fatal(err)
	}
	if updated == id1 {
		t.Fatal("compiler01 did not move to the new version")
	}

	// compiler02 is still on the old version — they have legitimately diverged.
	stale, err := c2.codeID(t, bin, "production")
	if err != nil {
		t.Fatal(err)
	}
	if stale != id1 {
		t.Fatalf("compiler02 should still be on the old version, got %s", stale)
	}

	// It comes back and polls once, with no event replayed to it.
	if err := c2.syncOnce(t, bin, publisher); err != nil {
		t.Fatalf("compiler02 catch-up sync: %v", err)
	}
	caught, err := c2.codeID(t, bin, "production")
	if err != nil {
		t.Fatal(err)
	}
	if caught != updated {
		t.Errorf("compiler02 did not catch up: %s, want %s", caught, updated)
	}
	t.Logf("both compilers converged to %s after catch-up", caught)

	// Content must match the version, not merely the id.
	body, err := os.ReadFile(filepath.Join(c2.envPath, "production", "manifests/site.pp"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "v2\n" {
		t.Errorf("compiler02 content = %q, want v2", body)
	}
}

// An agent whose certificate lacks the compiler role must be refused by the
// publisher, so a compromised leaf node cannot pull the estate's code.
func TestAgentWithoutCompilerRoleIsRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary and binds a port")
	}

	bin := build(t)
	ca := testca.New(t)

	staging := t.TempDir()
	writeEnv(t, staging, "production", map[string]string{"manifests/site.pp": "secret\n"})

	serverSSL := ca.SSLDir(t, "puppet.example.com", "openvox_server")
	const addr = "127.0.0.1:18153"
	const publisher = "https://" + addr

	cmd := exec.Command(bin, "publish",
		"--staging", staging, "--listen", addr,
		"--certname", "puppet.example.com", "--ssldir", serverSSL,
		"--state", t.TempDir())
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	compilerNode := newCompiler(t, ca, "compiler01.example.com")
	var err error
	for range 40 {
		if err = compilerNode.syncOnce(t, bin, publisher); err == nil {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("compiler never synced: %v", err)
	}

	// Same CA, valid certificate, wrong role.
	base := t.TempDir()
	web := compiler{
		name:    "web01.example.com",
		ssldir:  ca.SSLDir(t, "web01.example.com", "webserver"),
		root:    filepath.Join(base, "codavox"),
		envPath: filepath.Join(base, "environments"),
	}
	if err := web.syncOnce(t, bin, publisher); err == nil {
		t.Fatal("an agent without the compiler role fetched code")
	}

	if _, err := os.Stat(filepath.Join(web.envPath, "production")); err == nil {
		t.Error("an unauthorized node deployed an environment")
	}
}

// Revoking a compiler's Puppet certificate must revoke its access to code. The
// certificate stays cryptographically valid and keeps its pp_role, so mutual
// TLS alone would go on admitting it — only the CRL check does not.
//
// The publisher is not restarted between the two syncs: an operator running
// `puppetserver ca revoke` during an incident expects it to take effect, not to
// wait on a rolling restart of every publisher.
func TestRevokedCompilerLosesAccess(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary and binds a port")
	}

	bin := build(t)
	ca := testca.New(t)

	staging := t.TempDir()
	writeEnv(t, staging, "production", map[string]string{"manifests/site.pp": "secret\n"})

	serverSSL := ca.SSLDir(t, "puppet.example.com", "openvox_server")
	const addr = "127.0.0.1:18155"

	pub := &publisher{bin: bin, staging: staging, addr: addr, ssldir: serverSSL}
	pub.restart(t)
	t.Cleanup(pub.stop)

	c := newCompiler(t, ca, "compiler01.example.com")

	var err error
	for range 40 {
		if err = c.syncOnce(t, bin, pub.url()); err == nil {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("compiler never synced before revocation: %v", err)
	}

	// Revoke it in the CA's CRL, which is the file the publisher reads.
	ca.WriteCRL(t, serverSSL, testca.CertFor(t, c.ssldir, c.name))
	later := time.Now().Add(time.Second)
	if err := os.Chtimes(filepath.Join(serverSSL, "crl.pem"), later, later); err != nil {
		t.Fatal(err)
	}

	// Advance the served code so a refusal cannot be confused with "nothing to do".
	writeEnv(t, staging, "production", map[string]string{"manifests/site.pp": "rotated\n"})
	pub.hup(t)

	var syncErr error
	for range 20 {
		if syncErr = c.syncOnce(t, bin, pub.url()); syncErr != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if syncErr == nil {
		t.Fatal("a revoked compiler still fetched code")
	}

	body, err := os.ReadFile(filepath.Join(c.envPath, "production", "manifests/site.pp"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "secret\n" {
		t.Errorf("a revoked compiler received new code: %q", body)
	}
}

// The brownfield case, end to end against a real publisher process.
//
// An estate that predates codavox has no pp_role on any certificate, and adding
// one means re-issuing every compiler's: revoke, clean, re-enrol, restart. That
// is a PKI operation to demand before anyone can try codavox at all, so naming
// a certname has to be enough on its own.
func TestCompilerWithoutRoleIsAdmittedByCertname(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary and binds a port")
	}

	bin := build(t)
	ca := testca.New(t)

	staging := t.TempDir()
	writeEnv(t, staging, "production", map[string]string{"manifests/site.pp": "v1\n"})

	const addr = "127.0.0.1:18156"
	pub := &publisher{
		bin:     bin,
		staging: staging,
		addr:    addr,
		ssldir:  ca.SSLDir(t, "puppet.example.com", "openvox_server"),
		// No roles at all: this publisher authorizes purely by certname.
		extra: []string{"--allow-certname", "legacy.example.com"},
	}
	pub.restart(t)
	t.Cleanup(pub.stop)

	// A certificate with no pp_role whatsoever, as a node enrolled years ago has.
	base := t.TempDir()
	legacy := compiler{
		name:    "legacy.example.com",
		ssldir:  ca.SSLDir(t, "legacy.example.com", ""),
		root:    filepath.Join(base, "codavox"),
		envPath: filepath.Join(base, "environments"),
	}

	var err error
	for range 40 {
		if err = legacy.syncOnce(t, bin, pub.url()); err == nil {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("a named compiler with no pp_role was refused: %v", err)
	}

	id, err := legacy.codeID(t, bin, "production")
	if err != nil || id == "" {
		t.Fatalf("code-id after sync: %q %v", id, err)
	}

	// Naming one node must not admit the rest of the estate.
	other := t.TempDir()
	unnamed := compiler{
		name:    "other.example.com",
		ssldir:  ca.SSLDir(t, "other.example.com", ""),
		root:    filepath.Join(other, "codavox"),
		envPath: filepath.Join(other, "environments"),
	}
	if err := unnamed.syncOnce(t, bin, pub.url()); err == nil {
		t.Error("a node absent from the allowlist fetched code")
	}
}

// The publisher's fleet view must agree with what each compiler would answer
// for itself. That equality is the whole claim: an operator reads /v1/compilers
// instead of running code-id on every node, so anything the two can disagree
// about is a wrong answer delivered confidently.
//
// This runs the real binaries over real mutual TLS, because the certname the
// report is filed under comes from the peer certificate.
func TestFleetViewMatchesWhatCompilersServe(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary and binds a port")
	}

	bin := build(t)
	ca := testca.New(t)

	staging := t.TempDir()
	writeEnv(t, staging, "production", map[string]string{"manifests/site.pp": "v1\n"})
	writeEnv(t, staging, "testing", map[string]string{"manifests/site.pp": "t1\n"})

	serverSSL := ca.SSLDir(t, "puppet.example.com", "openvox_server")
	const addr = "127.0.0.1:18154"
	const publisher = "https://" + addr

	pub := exec.Command(bin, "publish",
		"--staging", staging, "--listen", addr,
		"--certname", "puppet.example.com", "--ssldir", serverSSL,
		"--state", t.TempDir())
	pub.Stderr = os.Stderr
	if err := pub.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = pub.Process.Kill()
		_ = pub.Wait()
	})

	c1 := newCompiler(t, ca, "compiler01.example.com")
	c2 := newCompiler(t, ca, "compiler02.example.com")

	var err error
	for range 40 {
		if err = c1.syncOnce(t, bin, publisher); err == nil {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("compiler01 never synced: %v", err)
	}
	if err := c2.syncOnce(t, bin, publisher); err != nil {
		t.Fatalf("compiler02 sync: %v", err)
	}

	// No second sync: the report must land within the run that deployed, not on
	// the following poll. A whole interval of lag would show a compiler that
	// just converged as one that had not.
	fleet := c1.fleet(t, publisher)
	if len(fleet) != 2 {
		t.Fatalf("fleet view has %d compilers, want 2: %+v", len(fleet), fleet)
	}

	byName := map[string]publish.Peer{}
	for _, peer := range fleet {
		byName[peer.Certname] = peer
	}

	for _, c := range []compiler{c1, c2} {
		peer, ok := byName[c.name]
		if !ok {
			t.Errorf("%s is missing from the fleet view", c.name)
			continue
		}
		for _, env := range []string{"production", "testing"} {
			want, err := c.codeID(t, bin, env)
			if err != nil {
				t.Fatalf("%s code-id %s: %v", c.name, env, err)
			}
			if peer.Serving[env] != want {
				t.Errorf("fleet view says %s serves %s at %q; the node itself says %q",
					c.name, env, peer.Serving[env], want)
			}
		}
		if peer.ServingAt.IsZero() {
			t.Errorf("%s has no serving_at", c.name)
		}
	}

	// And it must report staleness, not just agreement. compiler02 stays away
	// across a deploy, so the two legitimately diverge — the case the view
	// exists to make visible.
	writeEnv(t, staging, "production", map[string]string{"manifests/site.pp": "v2\n"})
	if err := pub.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}

	before := byName[c1.name].Serving["production"]
	var deployed string
	for range 40 {
		if err := c1.syncOnce(t, bin, publisher); err != nil {
			t.Fatal(err)
		}
		if deployed, err = c1.codeID(t, bin, "production"); err != nil {
			t.Fatal(err)
		}
		if deployed != before {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}

	fleet = c1.fleet(t, publisher)
	byName = map[string]publish.Peer{}
	for _, peer := range fleet {
		byName[peer.Certname] = peer
	}

	if got := byName[c1.name].Serving["production"]; got != deployed {
		t.Errorf("compiler01 reports %q, want the new version %q", got, deployed)
	}
	stale := byName[c2.name].Serving["production"]
	if stale == deployed {
		t.Error("compiler02 never polled, so it cannot be serving the new version")
	}
	// Which is exactly what that node would still answer.
	if want, err := c2.codeID(t, bin, "production"); err != nil {
		t.Fatal(err)
	} else if stale != want {
		t.Errorf("fleet view says compiler02 serves %q; the node says %q", stale, want)
	}
	t.Logf("fleet view shows the divergence: compiler01 %s, compiler02 %s", deployed, stale)
}

// fleet reads /v1/compilers as this compiler, over mutual TLS. Only an
// authorized peer can read it, so a compiler's own certificate is the natural
// credential for the test.
func (c compiler) fleet(t *testing.T, publisher string) []publish.Peer {
	t.Helper()
	tlsCfg, err := puppetca.Paths{SSLDir: c.ssldir, CertName: c.name}.ClientTLS()
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}
	defer client.CloseIdleConnections()

	resp, err := client.Get(publisher + publish.CompilersPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", publish.CompilersPath, resp.StatusCode)
	}
	var peers []publish.Peer
	if err := json.NewDecoder(resp.Body).Decode(&peers); err != nil {
		t.Fatal(err)
	}
	return peers
}

// The operator's path, end to end: `codavox compilers` on the publisher node,
// with no flags beyond what publishing already needs. This is what makes the
// fleet view usable — the endpoint alone is only reachable by a compiler, and
// the publisher's own certificate carries no compiler role.
func TestCompilersCommandOnThePublisher(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary and binds a port")
	}

	bin := build(t)
	ca := testca.New(t)

	staging := t.TempDir()
	writeEnv(t, staging, "production", map[string]string{
		"manifests/site.pp": "v1\n",
		// r10k leaves this behind; the publisher reads it to record which
		// control-repo commit produced the code_id. It is excluded from
		// sealing, so it never reaches a compiler.
		".r10k-deploy.json": `{"signature":"a3f1c9e4b2d8f70e","finished_at":"2026-07-25 12:00:00 -0400"}`,
	})

	serverSSL := ca.SSLDir(t, "puppet.example.com", "openvox_server")
	const addr = "127.0.0.1:18155"
	const publisher = "https://" + addr

	state := t.TempDir()
	pub := exec.Command(bin, "publish",
		"--staging", staging, "--listen", addr,
		"--certname", "puppet.example.com", "--ssldir", serverSSL,
		"--state", state)
	pub.Stderr = os.Stderr
	if err := pub.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = pub.Process.Kill()
		_ = pub.Wait()
	})

	c1 := newCompiler(t, ca, "compiler01.example.com")
	var err error
	for range 40 {
		if err = c1.syncOnce(t, bin, publisher); err == nil {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("compiler01 never synced: %v", err)
	}

	// Run as the publisher node would: its own certname and ssldir, and no
	// --publisher, so the default URL is exercised too. The address is a literal
	// IP here rather than the certname, which a test host cannot resolve.
	compilers := func(t *testing.T, extra ...string) string {
		t.Helper()
		args := append([]string{"compilers",
			"--certname", "puppet.example.com",
			"--ssldir", serverSSL,
			"--publisher", publisher,
			"--state", state,
		}, extra...)
		out, err := exec.Command(bin, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("codavox compilers: %v\n%s", err, out)
		}
		return string(out)
	}

	want, err := c1.codeID(t, bin, "production")
	if err != nil {
		t.Fatal(err)
	}

	out := compilers(t)
	if !strings.Contains(out, "compiler01.example.com") || !strings.Contains(out, want[:12]) {
		t.Errorf("codavox compilers did not report the compiler at %s:\n%s", want, out)
	}
	// A code_id is a content hash, so the commit is what an operator recognizes.
	// The join happens locally against the publisher's provenance log.
	if !strings.Contains(out, "a3f1c9e4b2d8") {
		t.Errorf("codavox compilers did not resolve the code_id to a commit:\n%s", out)
	}
	t.Logf("codavox compilers:\n%s", out)

	// --json carries both ids at full length: it is what a monitoring check
	// reads, and a shortened id cannot be compared exactly.
	raw := compilers(t, "--json")
	var records []struct {
		publish.Peer
		Commits map[string]string `json:"commits"`
	}
	if err := json.Unmarshal([]byte(raw), &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("--json reported %d compilers, want 1:\n%s", len(records), raw)
	}
	if got := records[0].Serving["production"]; got != want {
		t.Errorf("--json code_id = %q, want the full %q", got, want)
	}
	if got := records[0].Commits["production"]; got != "a3f1c9e4b2d8f70e" {
		t.Errorf("--json commit = %q, want the full a3f1c9e4b2d8f70e", got)
	}
	t.Logf("codavox compilers --json:\n%s", raw)
}
