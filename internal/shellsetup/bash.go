package shellsetup

import (
	"fmt"
	"path/filepath"
)

// BashInstaller installs cst hooks into ~/.bashrc.
type BashInstaller struct {
	// RCPath overrides the rc file path (for tests). Defaults to ~/.bashrc.
	RCPath string
	// CstDirPath overrides ~/.cst (for tests). Defaults to ~/.cst.
	CstDirPath string
}

func (b BashInstaller) Shell() Shell { return ShellBash }

func (b BashInstaller) RCFilePath() string {
	if b.RCPath != "" {
		return b.RCPath
	}
	return filepath.Join(HomeDir(), ".bashrc")
}

func (b BashInstaller) cstDir() string {
	if b.CstDirPath != "" {
		return b.CstDirPath
	}
	return filepath.Join(HomeDir(), ".cst")
}

func (b BashInstaller) EnsureDependencies() error {
	_, err := EnsureBashPreexec(b.cstDir())
	return err
}

func (b BashInstaller) Snippet() string {
	preexecPath := filepath.Join(b.cstDir(), "bash-preexec.sh")
	return fmt.Sprintf(`# Loaded by cst setup-shell. Pushes per-prompt events to the cst daemon
# (sub-millisecond cost; daemon does the actual work).
if [ -f %q ]; then
    source %q
fi
__cst_preexec() { (cst hook preexec "$1" >/dev/null 2>&1 &) }
__cst_precmd()  { (cst hook precmd  >/dev/null 2>&1 &) }
# Avoid double-registration if .bashrc is sourced twice.
case " ${preexec_functions[*]} " in
    *" __cst_preexec "*) ;;
    *) preexec_functions+=('__cst_preexec') ;;
esac
case " ${precmd_functions[*]} " in
    *" __cst_precmd "*) ;;
    *) precmd_functions+=('__cst_precmd') ;;
esac
`, preexecPath, preexecPath)
}
