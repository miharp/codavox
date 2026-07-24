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
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/miharp/codavox/internal/agent"
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
                  [--ssldir <dir>] [--allow-role <role>]
        Serve environment versions and artifacts to compilers over mutual TLS,
        using the Puppet CA material already on this node.

  codavox agent --publisher <url> [--interval <dur>] [--once]
                [--certname <name>] [--ssldir <dir>] [--environmentpath <dir>]
                [--keep <n>] [--min-age <dur>]
        Poll a publisher and converge this compiler onto the code it serves.

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

  codavox provenance <environment> <code-id> [--state <dir>] [--json]
        Print the control-repo commit that produced a code_id, read from the
        publisher's local provenance log. Run this on the publisher.

  codavox version
        Print the codavox version.

Environment:
  CODAVOX_ROOT   Override the deployment root (default %s).
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
		staging  string
		listen   string
		certname string
		ssldir   string
		state    string
		roles    []string
	}{
		listen: ":8150",
		ssldir: puppetca.DefaultSSLDir,
		state:  defaultStateDir(),
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
	if len(opts.roles) == 0 {
		opts.roles = []string{"openvox_compiler"}
	}

	paths := puppetca.Paths{SSLDir: opts.ssldir, CertName: opts.certname}
	tlsConfig, err := paths.ServerTLS(opts.roles...)
	if err != nil {
		return err
	}

	store := publish.NewStore(opts.staging, publish.ArtifactsDir(opts.state))

	provLog, err := publish.OpenLog(filepath.Join(opts.state, provenanceFile))
	if err != nil {
		return err
	}
	store.EnableProvenance(provLog)

	if err := store.Reseal(); err != nil {
		return err
	}

	for env, id := range store.Environments() {
		fmt.Fprintf(os.Stderr, "sealed %s %s\n", env, id)
	}
	fmt.Fprintf(os.Stderr, "listening on %s as %s (roles: %s)\n",
		opts.listen, opts.certname, strings.Join(opts.roles, ", "))

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
				if err := store.Reseal(); err != nil {
					fmt.Fprintf(os.Stderr, "reseal failed: %v\n", err)
					continue
				}
				for env, id := range store.Environments() {
					fmt.Fprintf(os.Stderr, "resealed %s %s\n", env, id)
				}
			}
		}
	}()

	srv := &publish.Server{Addr: opts.listen, Store: store, TLSConfig: tlsConfig}
	return srv.Serve(ctx)
}

// agentRun polls a publisher and converges this node onto it.
func agentRun(args []string) error {
	opts := struct {
		publisher string
		certname  string
		ssldir    string
		envPath   string
		interval  time.Duration
		keep      int
		minAge    time.Duration
		once      bool
	}{
		ssldir:   puppetca.DefaultSSLDir,
		envPath:  layout.DefaultEnvironmentPath,
		interval: agent.DefaultInterval,
		keep:     agent.DefaultKeep,
		minAge:   agent.DefaultMinAge,
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
			Root:            layout.New().Root,
			EnvironmentPath: opts.envPath,
		},
		Client: &http.Client{
			Timeout:   30 * time.Minute, // environments can be large
			Transport: &http.Transport{TLSClientConfig: tlsConfig},
		},
		Interval: opts.interval,
		Keep:     opts.keep,
		MinAge:   opts.minAge,
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

// provenanceFile is the publisher's provenance log, relative to the state dir.
const provenanceFile = "provenance.jsonl"

// defaultStateDir is where the publisher keeps its provenance log. It lives
// under the codavox root but is publisher-only diagnostic state: it never
// reaches a compiler and never feeds code-id, which stays a single symlink read.
func defaultStateDir() string {
	return filepath.Join(layout.New().Root, "state")
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

	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
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
