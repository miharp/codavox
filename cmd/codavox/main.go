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

	"github.com/miharp/codavox/internal/content"
	"github.com/miharp/codavox/internal/layout"
)

const usage = `codavox — versioned code distribution for OpenVox compilers

Usage:
  codavox code-id <environment>
        Print the code_id currently deployed for an environment.

  codavox code-content <environment> <code-id> <file-path>
        Stream a file as of a specific deployed code version.

  codavox version
        Print the codavox version.

Environment:
  CODAVOX_ROOT   Override the deployment root (default %s).
`

// version is overridden at build time via -ldflags.
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, layout.DefaultRoot)
		os.Exit(2)
	}

	if err := run(os.Args[1], os.Args[2:]); err != nil {
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
	case "version":
		fmt.Println(version)
		return nil
	case "-h", "--help", "help":
		fmt.Fprintf(os.Stdout, usage, layout.DefaultRoot)
		return nil
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
