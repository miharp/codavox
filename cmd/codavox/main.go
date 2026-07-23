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
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/miharp/codavox/internal/content"
	"github.com/miharp/codavox/internal/layout"
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
