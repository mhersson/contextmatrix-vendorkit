package claudecode

import "github.com/mhersson/contextmatrix-harness/tools"

// harnessBuiltins maps the harness filesystem tools to Claude Code built-ins.
var harnessBuiltins = map[string]string{"read": "Read", "write": "Write", "edit": "Edit", "bash": "Bash"}

// SplitRegistry maps the harness filesystem tools to Claude Code built-ins
// (read->Read, write->Write, edit->Edit, bash->Bash) and returns the rest
// (grep, glob, git, skill and every backend tool) for the bridge. 2.1.322 has
// no Grep or Glob built-in, so those stay bridged. bridged is nil when nothing
// is left, so Run starts no bridge.
func SplitRegistry(reg *tools.Registry) (builtins []string, bridged *tools.Registry) {
	if reg == nil {
		return nil, nil
	}

	var rest []tools.Tool

	for _, t := range reg.All() {
		if b, ok := harnessBuiltins[t.Name()]; ok {
			builtins = append(builtins, b)

			continue
		}

		rest = append(rest, t)
	}

	if len(rest) == 0 {
		return builtins, nil
	}

	return builtins, tools.NewRegistry(rest...)
}
