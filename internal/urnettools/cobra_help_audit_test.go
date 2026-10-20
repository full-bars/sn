package urnettools

import (
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// TestCobraHelpAudit walks the whole cobra tree and fails on the help gaps
// that make a command invisible or unusable: an empty Short (the one-line
// index), an empty Long (the command's own page), a leaf command with no
// Example (real invocations), a flag with no Usage text, and duplicate
// command names among siblings. Cobra's own builtins (help, completion) are
// exempt: their pages are the framework's.
func TestCobraHelpAudit(t *testing.T) {
	builtinExempt := map[string]string{
		"help":       "cobra builtin",
		"completion": "cobra builtin",
	}

	var gaps []string
	var walk func(path string, command *cobra.Command)
	walk = func(path string, command *cobra.Command) {
		name := command.Name()
		if path != "" {
			name = path + " " + command.Name()
		}
		if _, exempt := builtinExempt[command.Name()]; !exempt && !command.Hidden {
			if strings.TrimSpace(command.Short) == "" {
				gaps = append(gaps, name+": empty Short")
			}
			if strings.TrimSpace(command.Long) == "" {
				gaps = append(gaps, name+": empty Long")
			}
			subcommands := command.Commands()
			isLeaf := len(subcommands) == 0
			// the root itself carries the curated menu, not an Example
			if isLeaf && path != "" && strings.TrimSpace(command.Example) == "" {
				gaps = append(gaps, name+": missing Example")
			}
			checkFlags := func(flags *pflag.FlagSet) {
				if flags == nil {
					return
				}
				flags.VisitAll(func(flag *pflag.Flag) {
					if flag.Hidden {
						return
					}
					if strings.TrimSpace(flag.Usage) == "" {
						gaps = append(gaps, name+" --"+flag.Name+": flag without Usage")
					}
				})
			}
			checkFlags(command.LocalFlags())

			seen := map[string]bool{}
			for _, sub := range subcommands {
				if seen[sub.Name()] {
					gaps = append(gaps, name+": duplicate command name "+sub.Name())
				}
				seen[sub.Name()] = true
			}
		}
		for _, sub := range command.Commands() {
			walk(name, sub)
		}
	}
	walk("", buildRootCmd())

	if len(gaps) > 0 {
		sort.Strings(gaps)
		t.Errorf("cobra help audit: %d gaps\n  %s", len(gaps), strings.Join(gaps, "\n  "))
	}
}
