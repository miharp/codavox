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
	"context"
	"errors"
	"fmt"
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
		roles    []string
	}{
		listen: ":8150",
		ssldir: puppetca.DefaultSSLDir,
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

	store := publish.NewStore(opts.staging)
	if err := store.Reseal(); err != nil {
		return err
	}

	for env, id := range store.Environments() {
		fmt.Fprintf(os.Stderr, "sealed %s %s\n", env, id)
	}
	fmt.Fprintf(os.Stderr, "listening on %s as %s (roles: %s)\n",
		opts.listen, opts.certname, strings.Join(opts.roles, ", "))

	srv := &publish.Server{Addr: opts.listen, Store: store, TLSConfig: tlsConfig}
	return srv.ListenAndServeTLS()
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
