package shellsetup

import (
	"fmt"
	"path/filepath"
)

// FishInstaller installs cst hooks into ~/.config/fish/config.fish.
type FishInstaller struct {
	RCPath string
}

func (f FishInstaller) Shell() Shell { return ShellFish }

func (f FishInstaller) RCFilePath() string {
	if f.RCPath != "" {
		return f.RCPath
	}
	return filepath.Join(HomeDir(), ".config", "fish", "config.fish")
}

func (f FishInstaller) EnsureDependencies() error { return nil }

func (f FishInstaller) Snippet() string {
	return `# Loaded by cst setup-shell.
function __cst_preexec --on-event fish_preexec
    cst hook preexec -- $argv > /dev/null 2>&1 &
    disown 2>/dev/null
end
function __cst_precmd --on-event fish_prompt
    cst hook precmd > /dev/null 2>&1 &
    disown 2>/dev/null
end
`
}

// NewInstallerForShell returns the right Installer for the named shell.
func NewInstallerForShell(s Shell) (Installer, error) {
	switch s {
	case ShellBash:
		return BashInstaller{}, nil
	case ShellZsh:
		return ZshInstaller{}, nil
	case ShellFish:
		return FishInstaller{}, nil
	default:
		return nil, fmt.Errorf("unsupported shell: %q", s)
	}
}
