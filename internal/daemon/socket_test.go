package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestSocketPathPrefersXDGRuntimeDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	got := SocketPath()
	want := filepath.Join(dir, "cst-daemon-"+strconv.Itoa(os.Getuid())+".sock")
	if got != want {
		t.Errorf("SocketPath = %q, want %q", got, want)
	}
}

func TestSocketPathFallbackToTmp(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	got := SocketPath()
	want := filepath.Join("/tmp", "cst-daemon-"+strconv.Itoa(os.Getuid())+".sock")
	if got != want {
		t.Errorf("SocketPath = %q, want %q", got, want)
	}
}

func TestCheckSocketDirOwnershipOK(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "sock")
	if err := CheckSocketDirOwnership(sock); err != nil {
		t.Errorf("own dir should pass: %v", err)
	}
}

func TestCheckSocketDirOwnershipMissingDirOK(t *testing.T) {
	// Directory doesn't exist yet — function should not error.
	if err := CheckSocketDirOwnership("/does/not/exist/sock"); err != nil {
		t.Errorf("missing dir should be OK (will be created): %v", err)
	}
}

func TestPIDFileRoundtrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	pid, err := ReadPIDFile()
	if err != nil || pid != 0 {
		t.Fatalf("initial: pid=%d err=%v", pid, err)
	}

	if err := WritePIDFile(); err != nil {
		t.Fatalf("WritePIDFile: %v", err)
	}
	pid, err = ReadPIDFile()
	if err != nil {
		t.Fatalf("ReadPIDFile: %v", err)
	}
	if pid != os.Getpid() {
		t.Errorf("pid = %d, want %d", pid, os.Getpid())
	}

	if err := RemovePIDFile(); err != nil {
		t.Fatalf("RemovePIDFile: %v", err)
	}
	pid, _ = ReadPIDFile()
	if pid != 0 {
		t.Errorf("after remove pid = %d", pid)
	}
}

func TestRemovePIDFilePreservesReplacementDaemonPID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	path := PIDFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	replacementPID := os.Getpid() + 1
	if err := os.WriteFile(path, []byte(strconv.Itoa(replacementPID)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := RemovePIDFile(); err != nil {
		t.Fatalf("RemovePIDFile: %v", err)
	}
	pid, err := ReadPIDFile()
	if err != nil || pid != replacementPID {
		t.Fatalf("replacement pid file changed: pid=%d err=%v", pid, err)
	}
}
