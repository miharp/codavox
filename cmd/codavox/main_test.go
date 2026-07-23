package main

import "testing"

// The argv[0] names are part of the deployed interface: puppetserver's
// versioned-code.conf points directly at these paths, so renaming one silently
// breaks every compiler that has already been configured.
func TestArgv0Commands(t *testing.T) {
	want := map[string]string{
		"codavox-code-id":      "code-id",
		"codavox-code-content": "code-content",
	}

	for name, cmd := range want {
		got, ok := argv0Commands[name]
		if !ok {
			t.Errorf("argv0Commands missing %q", name)
			continue
		}
		if got != cmd {
			t.Errorf("argv0Commands[%q] = %q, want %q", name, got, cmd)
		}
	}

	if len(argv0Commands) != len(want) {
		t.Errorf("argv0Commands has %d entries, want %d", len(argv0Commands), len(want))
	}
}

func TestRunRejectsUnknownSubcommand(t *testing.T) {
	if err := run("bogus", nil); err == nil {
		t.Fatal("expected error for unknown subcommand, got nil")
	}
}

func TestRunRejectsWrongArgCount(t *testing.T) {
	if err := run("code-id", nil); err == nil {
		t.Error("code-id with no args should error")
	}
	if err := run("code-id", []string{"a", "b"}); err == nil {
		t.Error("code-id with two args should error")
	}
	if err := run("code-content", []string{"env", "id"}); err == nil {
		t.Error("code-content with two args should error")
	}
}
