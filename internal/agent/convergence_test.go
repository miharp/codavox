package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	cmd := exec.Command(bin, "agent",
		"--publisher", publisher,
		"--once",
		"--certname", c.name,
		"--ssldir", c.ssldir,
		"--environmentpath", c.envPath,
	)
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
			"--certname", "puppet.example.com", "--ssldir", serverSSL)
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
		"--certname", "puppet.example.com", "--ssldir", serverSSL)
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
