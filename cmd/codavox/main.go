// Command codavox distributes versioned Puppet code to OpenVox compilers.
//
// The code-id and code-content subcommands implement puppetserver's
// versioned-code-service contract. Both are invoked by puppetserver as fresh
// processes — code-id on every static catalog compile — so they must stay
// fast and must be silent on success: anything written to stderr is logged at
// ERROR level by puppetserver even when the exit code is zero.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/miharp/codavox/internal/agent"
	"github.com/miharp/codavox/internal/config"
	"github.com/miharp/codavox/internal/content"
	"github.com/miharp/codavox/internal/deploy"
	"github.com/miharp/codavox/internal/deployserver"
	"github.com/miharp/codavox/internal/layout"
	"github.com/miharp/codavox/internal/publish"
	"github.com/miharp/codavox/internal/puppetca"
	"github.com/miharp/codavox/internal/seal"
)

const usage = `codavox — versioned code distribution for OpenVox compilers

Usage:
  codavox code-id <environment>
        Print the code_id currently deployed for an environment.

  codavox code-content <environment> <code-id> <file-path>
        Stream a file as of a specific deployed code version.

  codavox seal <directory> [--manifest] [--archive <file>]
        Print the code_id for a staged environment tree. With --manifest,
        print the canonical manifest instead. With --archive, also write a
        deterministic artifact.

  codavox publish --staging <dir> [--listen <addr>] [--certname <name>]
                  [--ssldir <dir>] [--allow-role <role>] [--allow-certname <cn>]
                  [--certificate-revocation chain|leaf|false]
        Serve environment versions and artifacts to compilers over mutual TLS,
        using the Puppet CA material already on this node. Revoked certificates
        are refused, from the same crl.pem every other Puppet service reads.

  codavox agent --publisher <url> [--interval <dur>] [--once]
                [--certname <name>] [--ssldir <dir>] [--environmentpath <dir>]
                [--keep <n>] [--min-age <dur>] [--prune-environments]
        Poll a publisher and converge this compiler onto the code it serves.
        With --prune-environments, also remove environments the publisher no
        longer serves.

  codavox deploy <environment>... | --all [--wait] [--no-modules]
                 [--r10k <path>] [--r10k-config <file>]
                 [--staging <dir>] [--state <dir>] [--json]
        Run r10k to stage code, then trigger the publisher to reseal. Run this
        on the primary. With --wait, block until the new code_id is served.

  codavox deploy-server [--api-token <file>] [--secret <file>]
                        [--listen <addr>] [--no-tls] [--history <n>]
                        --staging <dir> [--state <dir>]
                        [--r10k <path>] [--r10k-config <file>]
                        [--certname <name>] [--ssldir <dir>]
        Serve the deploy API and/or webhook on the primary. --api-token enables
        POST /v1/deploys and deploy status; --secret enables the push webhook.
        (codavox webhook is an alias serving only the webhook.)

  codavox compilers [--publisher <url>] [--certname <name>] [--ssldir <dir>]
                    [--state <dir>] [--json]
        Print what each compiler is serving, as the compilers themselves report
        it, with the control-repo commit each code_id came from. Run this on
        the publisher, whose own certificate and provenance log it uses.

  codavox provenance <environment> <code-id> [--state <dir>] [--json]
        Print the control-repo commit that produced a code_id, read from the
        publisher's local provenance log. Run this on the publisher.

  codavox version
        Print the codavox version.

The publish, agent, compilers, deploy, deploy-server, and provenance commands
read shared settings from a config file (--config <file>, or CODAVOX_CONFIG,
default /etc/codavox/config.yaml). A flag overrides the file; the file overrides
the built-in default.

Environment:
  CODAVOX_ROOT     Override the deployment root (default %s).
  CODAVOX_CONFIG   Override the config file path (default /etc/codavox/config.yaml).
`

// version is overridden at build time via -ldflags.
var version = "dev"

// argv0Commands maps an invocation name to the subcommand it implies.
//
// puppetserver passes only positional arguments to code-id-command and
// code-content-command, so neither setting can point at a binary that expects
// a subcommand first. Dispatching on argv[0] lets a symlink stand in:
//
//	/usr/bin/codavox-code-id -> codavox
//
// A shell wrapper would also work, but it would add a shell fork to a path
// that runs on every catalog compile. A symlink costs nothing.
var argv0Commands = map[string]string{
	"codavox-code-id":      "code-id",
	"codavox-code-content": "code-content",
}

func main() {
	if cmd, ok := argv0Commands[filepath.Base(os.Args[0])]; ok {
		dispatch(cmd, os.Args[1:])
		return
	}

	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, layout.DefaultRoot)
		os.Exit(2)
	}

	dispatch(os.Args[1], os.Args[2:])
}

func dispatch(cmd string, args []string) {
	if err := run(cmd, args); err != nil {
		fmt.Fprintf(os.Stderr, "codavox: %v\n", err)
		os.Exit(1)
	}
}

func run(cmd string, args []string) error {
	switch cmd {
	case "code-id":
		return codeID(args)
	case "code-content":
		return codeContent(args)
	case "seal":
		return sealTree(args)
	case "publish":
		return publishServe(args)
	case "agent":
		return agentRun(args)
	case "deploy":
		return deployRun(args)
	case "deploy-server", "webhook":
		// webhook is a compatibility alias: deploy-server with only --secret set
		// serves the webhook route and nothing else.
		return deployServer(args)
	case "compilers":
		return compilersQuery(args)
	case "provenance":
		return provenanceQuery(args)
	case "version":
		fmt.Println(version)
		return nil
	case "-h", "--help", "help":
		_, err := fmt.Fprintf(os.Stdout, usage, layout.DefaultRoot)
		return err
	default:
		return fmt.Errorf("unknown subcommand %q (try 'codavox help')", cmd)
	}
}

func codeID(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("code-id takes exactly one argument: <environment>")
	}

	id, err := layout.New().CurrentCodeID(args[0])
	if err != nil {
		return err
	}

	// puppetserver trims the trailing newline; emitting one keeps the output
	// well-formed for humans and shell callers without affecting it.
	fmt.Println(id)
	return nil
}

func codeContent(args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("code-content takes exactly three arguments: <environment> <code-id> <file-path>")
	}

	// puppetserver streams this straight to the agent, so buffering keeps the
	// syscall count down on large files.
	out := bufio.NewWriter(os.Stdout)
	if err := content.Copy(out, layout.New(), args[0], args[1], args[2]); err != nil {
		return err
	}
	return out.Flush()
}

// sealTree derives the code_id for a staged tree, and optionally writes the
// artifact a compiler would receive.
//
// It only reads the directory. Staging stays r10k's job: codavox not owning
// the deploy keeps the trust boundary small and lets existing r10k workflows
// continue untouched.
func sealTree(args []string) error {
	var (
		dir          string
		wantManifest bool
		archivePath  string
	)

	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--manifest":
			wantManifest = true
		case "--archive":
			i++
			if i >= len(args) {
				return fmt.Errorf("--archive needs a file path")
			}
			archivePath = args[i]
		default:
			if strings.HasPrefix(a, "-") {
				return fmt.Errorf("unknown flag %q", a)
			}
			if dir != "" {
				return fmt.Errorf("seal takes one directory, got %q and %q", dir, a)
			}
			dir = a
		}
	}

	if dir == "" {
		return fmt.Errorf("seal needs a directory: codavox seal <directory>")
	}

	if wantManifest {
		m, err := seal.ManifestString(dir)
		if err != nil {
			return err
		}
		fmt.Println(m)
		return nil
	}

	id, err := seal.CodeID(dir)
	if err != nil {
		return err
	}

	if archivePath != "" {
		// #nosec G304,G703 -- the path is an argument the operator typed
		f, err := os.Create(archivePath)
		if err != nil {
			return fmt.Errorf("creating archive: %w", err)
		}
		if err := seal.WriteArchive(f, dir); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("closing archive: %w", err)
		}
	}

	fmt.Println(id)
	return nil
}

// publishServe runs the publisher.
//
// Certificates are not configurable as raw paths: they are derived from the
// node's certname and ssldir, so the publisher uses the same Puppet CA
// material as everything else on the box. Introducing a separate keypair here
// would create a second trust root to rotate and revoke.
func publishServe(args []string) error {
	opts := struct {
		staging    string
		listen     string
		certname   string
		ssldir     string
		state      string
		revocation string
		roles      []string
		certnames  []string
	}{
		listen: ":" + defaultPublishPort,
		ssldir: puppetca.DefaultSSLDir,
		state:  defaultStateDir(),
	}

	cfg, err := config.Load(configPath(args))
	if err != nil {
		return err
	}
	overlay(&opts.staging, cfg.Staging)
	overlay(&opts.state, cfg.State)
	overlay(&opts.ssldir, cfg.SSLDir)
	overlay(&opts.certname, cfg.Certname)
	overlay(&opts.listen, cfg.Publish.Listen)
	overlay(&opts.revocation, cfg.Publish.CertificateRevocation)
	if len(cfg.Publish.AllowRoles) > 0 {
		opts.roles = cfg.Publish.AllowRoles
	}
	if len(cfg.Publish.AllowCertnames) > 0 {
		opts.certnames = cfg.Publish.AllowCertnames
	}

	for i := 0; i < len(args); i++ {
		next := func() (string, error) {
			i++
			if i >= len(args) {
				return "", fmt.Errorf("%s needs a value", args[i-1])
			}
			return args[i], nil
		}
		var err error
		switch args[i] {
		case "--config":
			_, err = next() // already loaded; consume the value
		case "--staging":
			opts.staging, err = next()
		case "--listen":
			opts.listen, err = next()
		case "--certname":
			opts.certname, err = next()
		case "--ssldir":
			opts.ssldir, err = next()
		case "--state":
			opts.state, err = next()
		case "--allow-role":
			var r string
			if r, err = next(); err == nil {
				opts.roles = append(opts.roles, r)
			}
		case "--allow-certname":
			var c string
			if c, err = next(); err == nil {
				opts.certnames = append(opts.certnames, c)
			}
		case "--certificate-revocation":
			opts.revocation, err = next()
		default:
			return fmt.Errorf("unknown argument %q", args[i])
		}
		if err != nil {
			return err
		}
	}

	if opts.staging == "" {
		return fmt.Errorf("publish needs --staging <dir>")
	}
	if opts.certname == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("determining certname: %w", err)
		}
		opts.certname = hostname
	}
	// Only default the role when nothing else authorizes anyone: an operator who
	// listed certnames deliberately should not silently also admit a whole class
	// of nodes.
	if len(opts.roles) == 0 && len(opts.certnames) == 0 {
		opts.roles = []string{"openvox_compiler"}
	}

	revocation, err := puppetca.ParseRevocationMode(opts.revocation)
	if err != nil {
		return err
	}

	paths := puppetca.Paths{SSLDir: opts.ssldir, CertName: opts.certname}
	tlsConfig, rev, err := paths.ServerTLS(puppetca.ServerPolicy{
		AllowedRoles:     opts.roles,
		AllowedCertnames: opts.certnames,
		Revocation:       revocation,
	})
	if err != nil {
		return err
	}

	store := publish.NewStore(opts.staging, publish.ArtifactsDir(opts.state))

	provLog, err := publish.OpenLog(filepath.Join(opts.state, provenanceFile))
	if err != nil {
		return err
	}
	store.EnableProvenance(provLog)

	// A single broken environment must not keep the publisher from starting: the
	// rest of the estate still needs its code, and refusing to come up would turn
	// one bad module into a fleet-wide outage. It is reported loudly and the
	// environments that did seal are served.
	if err := store.Reseal(); err != nil {
		if !errors.Is(err, publish.ErrPartialReseal) {
			return err
		}
		fmt.Fprintf(os.Stderr, "codavox: %v\n", err)
	}

	for env, id := range store.Environments() {
		fmt.Fprintf(os.Stderr, "sealed %s %s\n", env, id)
	}
	fmt.Fprintf(os.Stderr, "listening on %s as %s (roles: %s, certnames: %s, revocation: %s)\n",
		opts.listen, opts.certname,
		joinOrNone(opts.roles), joinOrNone(opts.certnames), revocation)

	// Record the pid so a deploy can signal this publisher to reseal.
	pidPath := publish.PidFilePath(opts.state)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o700); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil { // #nosec G306
		return fmt.Errorf("writing pidfile: %w", err)
	}
	defer func() { _ = os.Remove(pidPath) }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// SIGHUP triggers a reseal. codavox does not own the deploy — it observes a
	// staging directory r10k controls — so it cannot hook "r10k finished" in
	// process the way PE's Code Manager does. An operator, or r10k's postrun
	// hook, sends SIGHUP after a deploy completes. Because the signal fires only
	// once the deploy has returned, the tree is quiescent and no reseal ever
	// observes a half-written deploy.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				// A partial failure still updated every environment that sealed,
				// so report the problem and go on to list what is now served
				// rather than leaving the operator guessing which took effect.
				err := store.Reseal()
				if err != nil {
					fmt.Fprintf(os.Stderr, "reseal failed: %v\n", err)
					if !errors.Is(err, publish.ErrPartialReseal) {
						continue
					}
				}
				for env, id := range store.Environments() {
					fmt.Fprintf(os.Stderr, "resealed %s %s\n", env, id)
				}
			}
		}
	}()

	// rev.Check is applied per request, not only at handshake: an agent polls
	// over one keep-alive connection and would otherwise keep its access until
	// that connection happened to drop.
	srv := &publish.Server{
		Addr:      opts.listen,
		Store:     store,
		TLSConfig: tlsConfig,
		PeerCheck: rev.Check,
		// In memory and best effort: a restart empties it and a healthy fleet
		// refills it within one poll interval. Persisting it would create a
		// second store of state the environment symlink already owns.
		Peers: publish.NewPeers(),
	}
	return srv.Serve(ctx)
}

// agentRun polls a publisher and converges this node onto it.
func agentRun(args []string) error {
	// The agent writes the very symlink code-id reads back, so both must resolve
	// the environment path the same way. Seeding from layout.New() rather than
	// from the bare constant keeps CODAVOX_ENVIRONMENTPATH meaning the same thing
	// to both: otherwise the agent would deploy under the compiled-in default
	// while code-id reported from the overridden path, which is exactly the
	// two-sources-of-truth divergence this design exists to rule out.
	base := layout.New()

	opts := struct {
		publisher string
		certname  string
		ssldir    string
		envPath   string
		interval  time.Duration
		keep      int
		minAge    time.Duration
		once      bool
		prune     bool
	}{
		ssldir:   puppetca.DefaultSSLDir,
		envPath:  base.EnvironmentPath,
		interval: agent.DefaultInterval,
		keep:     agent.DefaultKeep,
		minAge:   agent.DefaultMinAge,
	}

	cfg, err := config.Load(configPath(args))
	if err != nil {
		return err
	}
	if cfg.Agent.PruneEnvironments {
		opts.prune = true
	}
	overlay(&opts.publisher, cfg.Agent.Publisher)
	overlay(&opts.certname, cfg.Certname)
	overlay(&opts.ssldir, cfg.SSLDir)
	overlay(&opts.envPath, cfg.EnvironmentPath)
	if cfg.Agent.Keep > 0 {
		opts.keep = cfg.Agent.Keep
	}
	if cfg.Agent.Interval != "" {
		if opts.interval, err = time.ParseDuration(cfg.Agent.Interval); err != nil {
			return fmt.Errorf("config agent.interval: %w", err)
		}
	}
	if cfg.Agent.MinAge != "" {
		if opts.minAge, err = time.ParseDuration(cfg.Agent.MinAge); err != nil {
			return fmt.Errorf("config agent.min_age: %w", err)
		}
	}

	for i := 0; i < len(args); i++ {
		next := func() (string, error) {
			i++
			if i >= len(args) {
				return "", fmt.Errorf("%s needs a value", args[i-1])
			}
			return args[i], nil
		}
		var err error
		var v string
		switch args[i] {
		case "--config":
			_, err = next()
		case "--publisher":
			opts.publisher, err = next()
		case "--certname":
			opts.certname, err = next()
		case "--ssldir":
			opts.ssldir, err = next()
		case "--environmentpath":
			opts.envPath, err = next()
		case "--once":
			opts.once = true
		case "--prune-environments":
			opts.prune = true
		case "--interval":
			if v, err = next(); err == nil {
				opts.interval, err = time.ParseDuration(v)
			}
		case "--min-age":
			if v, err = next(); err == nil {
				opts.minAge, err = time.ParseDuration(v)
			}
		case "--keep":
			if v, err = next(); err == nil {
				opts.keep, err = strconv.Atoi(v)
			}
		default:
			return fmt.Errorf("unknown argument %q", args[i])
		}
		if err != nil {
			return err
		}
	}

	if opts.publisher == "" {
		return fmt.Errorf("agent needs --publisher <url>")
	}
	if opts.certname == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("determining certname: %w", err)
		}
		opts.certname = hostname
	}

	paths := puppetca.Paths{SSLDir: opts.ssldir, CertName: opts.certname}
	tlsConfig, err := paths.ClientTLS()
	if err != nil {
		return err
	}

	a, err := agent.New(agent.Config{
		BaseURL: opts.publisher,
		Layout: layout.Layout{
			Root:            base.Root,
			EnvironmentPath: opts.envPath,
		},
		Client: &http.Client{
			Timeout:   30 * time.Minute, // environments can be large
			Transport: &http.Transport{TLSClientConfig: tlsConfig},
		},
		Interval: opts.interval,
		Keep:     opts.keep,
		MinAge:   opts.minAge,
		Prune:    opts.prune,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if opts.once {
		return a.Once(ctx)
	}

	if err := a.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// deployRun runs r10k to stage code and triggers the publisher to reseal.
//
// This is the operator-facing deploy verb, familiar from Code Manager's
// puppet-code deploy. The orchestration lives in internal/deploy so a webhook
// receiver or deploy API can later reuse it rather than reimplementing r10k
// invocation and the reseal trigger.
func deployRun(args []string) error {
	opts := struct {
		envs       []string
		all        bool
		wait       bool
		noModules  bool
		r10k       string
		r10kConfig string
		staging    string
		state      string
		asJSON     bool
	}{
		state: defaultStateDir(),
	}

	cfg, err := config.Load(configPath(args))
	if err != nil {
		return err
	}
	overlay(&opts.staging, cfg.Staging)
	overlay(&opts.state, cfg.State)
	overlay(&opts.r10k, cfg.R10k)
	overlay(&opts.r10kConfig, cfg.R10kConfig)

	for i := 0; i < len(args); i++ {
		next := func() (string, error) {
			i++
			if i >= len(args) {
				return "", fmt.Errorf("%s needs a value", args[i-1])
			}
			return args[i], nil
		}
		var err error
		switch a := args[i]; a {
		case "--config":
			_, err = next()
		case "--all":
			opts.all = true
		case "--wait":
			opts.wait = true
		case "--no-modules":
			opts.noModules = true
		case "--json":
			opts.asJSON = true
		case "--r10k":
			opts.r10k, err = next()
		case "--r10k-config":
			opts.r10kConfig, err = next()
		case "--staging":
			opts.staging, err = next()
		case "--state":
			opts.state, err = next()
		default:
			if strings.HasPrefix(a, "-") {
				return fmt.Errorf("unknown flag %q", a)
			}
			opts.envs = append(opts.envs, a)
		}
		if err != nil {
			return err
		}
	}

	if opts.staging == "" {
		return fmt.Errorf("deploy needs --staging <dir> (r10k's basedir, the same the publisher serves)")
	}

	results, runErr := deploy.Run(deploy.Config{
		R10kPath:   opts.r10k,
		R10kConfig: opts.r10kConfig,
		StagingDir: opts.staging,
		StateDir:   opts.state,
		Modules:    !opts.noModules,
	}, opts.envs, opts.all, opts.wait)

	if err := printDeployResults(results, opts.wait, opts.asJSON); err != nil {
		return err
	}
	return runErr
}

func printDeployResults(results []deploy.Result, waited, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if results == nil {
			results = []deploy.Result{}
		}
		return enc.Encode(results)
	}

	for _, r := range results {
		switch {
		case r.Err != "":
			fmt.Printf("%s\tfailed\t%s\n", r.Env, r.Err)
		case waited && r.Serving:
			fmt.Printf("%s\tdeployed\t%s\t%s\tserving\n", r.Env, r.CodeID, commitTag(r.Commit))
		case waited:
			fmt.Printf("%s\tdeployed\t%s\t%s\tnot serving\n", r.Env, r.CodeID, commitTag(r.Commit))
		default:
			fmt.Printf("%s\tdeployed\t%s\t%s\n", r.Env, r.CodeID, commitTag(r.Commit))
		}
	}
	return nil
}

func commitTag(commit string) string {
	if commit == "" {
		return "(no commit)"
	}
	if len(commit) > 7 {
		commit = commit[:7]
	}
	return "(commit " + commit + ")"
}

// deployServer serves the deploy API and/or the webhook.
//
// Unlike the publisher's artifact API, its callers — CI over the API, GitHub or
// GitLab over the webhook — cannot present a Puppet certificate, so it
// authenticates a bearer token or a shared secret rather than mutual TLS. Both
// front doors feed one deploy queue and one history.
func deployServer(args []string) error {
	opts := struct {
		apiTokenFile string
		secretFile   string
		listen       string
		noTLS        bool
		staging      string
		state        string
		r10k         string
		r10kConfig   string
		certname     string
		ssldir       string
		history      int
	}{
		listen:  ":8170",
		ssldir:  puppetca.DefaultSSLDir,
		state:   defaultStateDir(),
		history: 100,
	}

	cfg, err := config.Load(configPath(args))
	if err != nil {
		return err
	}
	overlay(&opts.apiTokenFile, cfg.DeployServer.APIToken)
	overlay(&opts.secretFile, cfg.DeployServer.Secret)
	overlay(&opts.listen, cfg.DeployServer.Listen)
	overlay(&opts.staging, cfg.Staging)
	overlay(&opts.state, cfg.State)
	overlay(&opts.r10k, cfg.R10k)
	overlay(&opts.r10kConfig, cfg.R10kConfig)
	overlay(&opts.certname, cfg.Certname)
	overlay(&opts.ssldir, cfg.SSLDir)
	if cfg.DeployServer.History > 0 {
		opts.history = cfg.DeployServer.History
	}

	for i := 0; i < len(args); i++ {
		next := func() (string, error) {
			i++
			if i >= len(args) {
				return "", fmt.Errorf("%s needs a value", args[i-1])
			}
			return args[i], nil
		}
		var err error
		var v string
		switch args[i] {
		case "--config":
			_, err = next()
		case "--api-token":
			opts.apiTokenFile, err = next()
		case "--secret":
			opts.secretFile, err = next()
		case "--listen":
			opts.listen, err = next()
		case "--no-tls":
			opts.noTLS = true
		case "--staging":
			opts.staging, err = next()
		case "--state":
			opts.state, err = next()
		case "--r10k":
			opts.r10k, err = next()
		case "--r10k-config":
			opts.r10kConfig, err = next()
		case "--certname":
			opts.certname, err = next()
		case "--ssldir":
			opts.ssldir, err = next()
		case "--history":
			if v, err = next(); err == nil {
				opts.history, err = strconv.Atoi(v)
			}
		default:
			return fmt.Errorf("unknown argument %q", args[i])
		}
		if err != nil {
			return err
		}
	}

	if opts.staging == "" {
		return fmt.Errorf("deploy-server needs --staging <dir>")
	}
	apiToken, err := readSecretFile(opts.apiTokenFile)
	if err != nil {
		return err
	}
	secret, err := readSecretFile(opts.secretFile)
	if err != nil {
		return err
	}
	if len(apiToken) == 0 && len(secret) == 0 {
		return fmt.Errorf("deploy-server needs --api-token, --secret, or both")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	srv := deployserver.New(deployserver.Config{
		Deployer: deployserver.Runner{Config: deploy.Config{
			R10kPath:   opts.r10k,
			R10kConfig: opts.r10kConfig,
			StagingDir: opts.staging,
			StateDir:   opts.state,
			Modules:    true,
		}},
		APIToken:   apiToken,
		Secret:     secret,
		MaxHistory: opts.history,
		Logger:     logger,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go srv.Start(ctx)

	httpSrv := &http.Server{
		Addr:              opts.listen,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	roles := deployServerRoles(apiToken, secret)
	if opts.noTLS {
		fmt.Fprintf(os.Stderr, "deploy-server listening on %s (%s, no TLS)\n", opts.listen, roles)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}

	if opts.certname == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("determining certname: %w", err)
		}
		opts.certname = hostname
	}
	tlsConfig, err := (puppetca.Paths{SSLDir: opts.ssldir, CertName: opts.certname}).ServerCertTLS()
	if err != nil {
		return err
	}
	httpSrv.TLSConfig = tlsConfig
	fmt.Fprintf(os.Stderr, "deploy-server listening on %s as %s (%s)\n", opts.listen, opts.certname, roles)
	if err := httpSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// readSecretFile reads and trims a credential file. An empty path yields no
// credential (that route stays disabled); a named file that is empty is an
// error, since it was clearly meant to enable something.
func readSecretFile(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path) // #nosec G304 -- operator-supplied credential path
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return nil, fmt.Errorf("credential file %s is empty", path)
	}
	return b, nil
}

func deployServerRoles(apiToken, secret []byte) string {
	switch {
	case len(apiToken) > 0 && len(secret) > 0:
		return "api + webhook"
	case len(apiToken) > 0:
		return "api"
	default:
		return "webhook"
	}
}

// joinOrNone renders a list for the startup line, so an empty one reads as a
// deliberate "none" rather than a blank the operator has to interpret.
func joinOrNone(v []string) string {
	if len(v) == 0 {
		return "none"
	}
	return strings.Join(v, ", ")
}

// provenanceFile is the publisher's provenance log, relative to the state dir.
const provenanceFile = "provenance.jsonl"

// configPath scans args for --config so the file can be loaded before the flag
// loop runs, since the file seeds the defaults the flags then override.
func configPath(args []string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--config" {
			return args[i+1]
		}
	}
	return ""
}

// overlay sets *dst to v when v is non-empty. It applies a config value on top
// of a built-in default, before flags are parsed.
func overlay(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

// defaultStateDir is where the publisher keeps its provenance log. It lives
// under the codavox root but is publisher-only diagnostic state: it never
// reaches a compiler and never feeds code-id, which stays a single symlink read.
func defaultStateDir() string {
	return filepath.Join(layout.New().Root, "state")
}

// listenPort extracts the port from a listen address, so a publisher moved off
// :8150 can be reached without also passing --publisher. An address that does
// not parse falls back to the default rather than failing: the caller can always
// name the publisher explicitly, and refusing to run over a malformed config
// line would be a worse answer than trying the port everyone uses.
func listenPort(listen string) string {
	if _, port, err := net.SplitHostPort(listen); err == nil && port != "" {
		return port
	}
	return defaultPublishPort
}

// defaultPublishPort is the publisher's port, matching the default --listen.
const defaultPublishPort = "8150"

// compilersQuery prints what each compiler is serving.
//
// It reads the publisher's fleet view over mutual TLS using this node's own
// certificate, which the publisher always admits. Run on the publisher, it
// therefore needs no configuration beyond what publishing already needs.
//
// The output is what each compiler reported about itself, read from the same
// symlink its code-id reads — so `codavox compilers` on the publisher and
// `codavox code-id` on a compiler answer the same question, and must agree.
// What they cannot share is freshness: a report is as old as that compiler's
// last poll, which last_poll states rather than hides.
func compilersQuery(args []string) error {
	var (
		publisher string
		certname  string
		ssldir    = puppetca.DefaultSSLDir
		state     = defaultStateDir()
		asJSON    bool
	)

	cfg, err := config.Load(configPath(args))
	if err != nil {
		return err
	}
	overlay(&ssldir, cfg.SSLDir)
	overlay(&certname, cfg.Certname)
	overlay(&state, cfg.State)
	overlay(&publisher, cfg.Agent.Publisher)

	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--config":
			i++
			if i >= len(args) {
				return fmt.Errorf("--config needs a value")
			}
		case "--publisher":
			i++
			if i >= len(args) {
				return fmt.Errorf("--publisher needs a value")
			}
			publisher = args[i]
		case "--certname":
			i++
			if i >= len(args) {
				return fmt.Errorf("--certname needs a value")
			}
			certname = args[i]
		case "--ssldir":
			i++
			if i >= len(args) {
				return fmt.Errorf("--ssldir needs a value")
			}
			ssldir = args[i]
		case "--state":
			i++
			if i >= len(args) {
				return fmt.Errorf("--state needs a value")
			}
			state = args[i]
		case "--json":
			asJSON = true
		default:
			return fmt.Errorf("unknown argument %q", a)
		}
	}

	if certname == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("determining certname: %w", err)
		}
		certname = hostname
	}
	if publisher == "" {
		// The publisher's own name, not localhost: the certificate it presents
		// is issued for its certname, so localhost would fail verification.
		port := listenPort(cfg.Publish.Listen)
		publisher = "https://" + net.JoinHostPort(certname, port)
	}

	tlsConfig, err := puppetca.Paths{SSLDir: ssldir, CertName: certname}.ClientTLS()
	if err != nil {
		return err
	}
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsConfig},
	}
	defer client.CloseIdleConnections()

	url := strings.TrimSuffix(publisher, "/") + publish.CompilersPath
	//nolint:gosec // the URL is the operator's own --publisher or config value
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("querying %s: %w", publisher, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("querying %s: %s", publisher, resp.Status)
	}

	var peers []publish.Peer
	if err := json.NewDecoder(resp.Body).Decode(&peers); err != nil {
		return fmt.Errorf("decoding the fleet view: %w", err)
	}

	// The provenance join is this command's, not the API's: /v1/compilers is
	// readable by any authorized compiler, and the log stays publisher-only.
	commits := commitsFor(state, peers)

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(fleetRecords(peers, commits))
	}

	if len(peers) == 0 {
		// Not an error. A publisher that has just started has seen nobody yet,
		// and saying so is the correct answer.
		fmt.Fprintln(os.Stderr, "no compilers have polled this publisher")
		return nil
	}

	return printFleet(os.Stdout, peers, commits, time.Now().UTC())
}

// fleetRecord is one compiler as `codavox compilers --json` reports it: the
// publisher's own record, with the commits this command resolved locally.
//
// Peer is embedded rather than wrapped, so the JSON is the endpoint's shape
// plus one field. A tool written against /v1/compilers keeps working against
// this output, and a tool written against this output does not silently break
// when pointed at the endpoint — it just sees no commits.
type fleetRecord struct {
	publish.Peer
	// Commits maps an environment to the control-repo commit that produced the
	// code_id in Serving. An environment with no recorded provenance is absent
	// rather than empty: a missing record is honestly missing, never another
	// version's commit.
	Commits map[string]string `json:"commits,omitempty"`
}

// fleetRecords pairs each peer with the commits resolved for what it serves.
//
// Full ids throughout, unlike the table: anything consuming JSON is matching
// exactly, not reading by eye.
func fleetRecords(peers []publish.Peer, commits map[string]string) []fleetRecord {
	out := make([]fleetRecord, 0, len(peers))
	for _, p := range peers {
		rec := fleetRecord{Peer: p}
		for env, codeID := range p.Serving {
			commit := commits[env+"\x00"+codeID]
			if commit == "" {
				continue
			}
			if rec.Commits == nil {
				rec.Commits = map[string]string{}
			}
			rec.Commits[env] = commit
		}
		out = append(out, rec)
	}
	return out
}

// commitsFor maps each (environment, code_id) a compiler reported to the
// control-repo commit that produced it.
//
// Best effort, like provenance itself: an unreadable log or an id r10k left no
// record for simply has no commit, and the row still shows the code_id. An
// absent commit is the honest answer, never another version's.
func commitsFor(state string, peers []publish.Peer) map[string]string {
	log, err := publish.OpenLog(filepath.Join(state, provenanceFile))
	if err != nil {
		return nil
	}
	commits := map[string]string{}
	for _, p := range peers {
		for env, codeID := range p.Serving {
			key := env + "\x00" + codeID
			if _, done := commits[key]; done {
				continue
			}
			// Most recently sealed first, so an id produced by several commits
			// shows the newest — the one an operator just deployed.
			if records := log.Lookup(env, codeID); len(records) > 0 {
				commits[key] = records[0].Commit
			}
		}
	}
	return commits
}

// shortID is how many hex characters of a code_id the table shows. Full ids are
// 64 characters, which would push every other column off a terminal; this is
// enough to compare rows by eye, and --json carries the whole thing for anything
// that needs to match exactly.
const shortID = 12

// printFleet renders the fleet view as a table, one row per compiler and
// environment. commits maps "environment\x00code_id" to a control-repo commit,
// and may be nil.
//
// The publisher's current versions are not shown beside them: this command
// reports what compilers said, and the publisher's own answer is one line of
// `curl` away. Marking a row "stale" here would mean deciding how far behind is
// too far, which depends on the poll interval and the deploy cadence — an
// operator's judgment, not this command's.
func printFleet(w io.Writer, peers []publish.Peer, commits map[string]string, now time.Time) error {
	// Writes to a tabwriter are buffered, so errors surface from Flush.
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "COMPILER\tENVIRONMENT\tCODE_ID\tCOMMIT\tLAST POLL")
	for _, p := range peers {
		age := "never"
		if !p.LastPoll.IsZero() {
			age = now.Sub(p.LastPoll).Truncate(time.Second).String() + " ago"
		}
		if len(p.Serving) == 0 {
			// A compiler that polls but reports nothing is either newly
			// enrolled with nothing deployed yet, or running an agent older
			// than this feature. Both are worth seeing, so it gets a row.
			_, _ = fmt.Fprintf(tw, "%s\t-\t(not reported)\t-\t%s\n", p.Certname, age)
			continue
		}
		envs := make([]string, 0, len(p.Serving))
		for env := range p.Serving {
			envs = append(envs, env)
		}
		sort.Strings(envs)
		for _, env := range envs {
			codeID := p.Serving[env]
			commit := commits[env+"\x00"+codeID]
			if commit == "" {
				commit = "-"
			} else if len(commit) > shortID {
				commit = commit[:shortID]
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
				p.Certname, env, truncate(codeID, shortID), commit, age)
		}
	}
	return tw.Flush()
}

// truncate shortens a hex id for display.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// provenanceQuery prints the control-repo provenance recorded for a code_id.
//
// It reads the publisher's local log directly and does no network I/O, so it
// must be run on the publisher. An empty result is reported honestly rather
// than as an error: provenance is best-effort, so its absence must never be
// dressed up as some other version's history.
func provenanceQuery(args []string) error {
	var (
		state      = defaultStateDir()
		asJSON     bool
		positional []string
	)

	cfg, err := config.Load(configPath(args))
	if err != nil {
		return err
	}
	overlay(&state, cfg.State)

	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--config":
			i++
			if i >= len(args) {
				return fmt.Errorf("--config needs a value")
			}
		case "--state":
			i++
			if i >= len(args) {
				return fmt.Errorf("--state needs a value")
			}
			state = args[i]
		case "--json":
			asJSON = true
		default:
			if strings.HasPrefix(a, "-") {
				return fmt.Errorf("unknown flag %q", a)
			}
			positional = append(positional, a)
		}
	}

	if len(positional) != 2 {
		return fmt.Errorf("provenance takes two arguments: <environment> <code-id>")
	}
	env, codeID := positional[0], positional[1]
	if err := layout.ValidateEnvironment(env); err != nil {
		return err
	}
	if err := layout.ValidateCodeID(codeID); err != nil {
		return err
	}

	log, err := publish.OpenLog(filepath.Join(state, provenanceFile))
	if err != nil {
		return err
	}
	records := log.Lookup(env, codeID)

	if asJSON {
		if records == nil {
			records = []publish.Provenance{}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(records)
	}

	if len(records) == 0 {
		fmt.Fprintf(os.Stderr, "no provenance recorded for %s in %s\n", codeID, env)
		return nil
	}
	for _, p := range records {
		commit := p.Commit
		if commit == "" {
			commit = "(unknown commit)"
		}
		deployed := p.DeployedAt
		if deployed == "" {
			deployed = "unknown"
		}
		fmt.Printf("%s\tdeployed %s\tsealed %s\n", commit, deployed, p.SealedAt.Format(time.RFC3339))
	}
	return nil
}
