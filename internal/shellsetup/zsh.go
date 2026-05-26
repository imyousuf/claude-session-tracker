package shellsetup

import "path/filepath"

// ZshInstaller installs cst hooks into ~/.zshrc. Zsh has native precmd/preexec
// hooks, so no bash-preexec dependency.
type ZshInstaller struct {
	RCPath string
}

func (z ZshInstaller) Shell() Shell { return ShellZsh }

func (z ZshInstaller) RCFilePath() string {
	if z.RCPath != "" {
		return z.RCPath
	}
	return filepath.Join(HomeDir(), ".zshrc")
}

func (z ZshInstaller) EnsureDependencies() error { return nil }

func (z ZshInstaller) Snippet() string {
	return `# Loaded by cst setup-shell. Pushes per-prompt events to the cst daemon.
__cst_preexec() { (cst hook preexec -- "$1" >/dev/null 2>&1 &) }
__cst_precmd()  { (cst hook precmd >/dev/null 2>&1 &) }
typeset -ga preexec_functions precmd_functions
preexec_functions=(${preexec_functions[@]:#__cst_preexec} __cst_preexec)
precmd_functions=(${precmd_functions[@]:#__cst_precmd} __cst_precmd)
`
}
