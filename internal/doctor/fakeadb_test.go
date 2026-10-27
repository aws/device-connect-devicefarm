package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The fake adb is this test binary. installFakeAdb copies it into a directory with a fakeAdbSpec
// beside it; TestMain plays the spec when the binary starts as the fake.

// fakeAdbSpec says what a fake adb does.
type fakeAdbSpec struct {
	Output   string        // printed to stdout
	ExitCode int           // what the fake exits with
	Marker   string        // file created when the fake runs: proof it ran
	HangFor  time.Duration // sleep before exit
	Child    bool          // leave a child holding stdout after exiting
}

const (
	// fakeAdbEnv, set by installFakeAdb, tells this binary to run as the fake adb.
	fakeAdbEnv = "DOCTOR_TEST_FAKE_ADB"
	// fakeAdbChildEnv marks the child a fake adb leaves behind; it only holds stdout for a while.
	fakeAdbChildEnv = "DOCTOR_TEST_FAKE_ADB_CHILD"
)

// TestMain runs the tests, the fake adb, or the fake's child.
func TestMain(m *testing.M) {
	switch {
	case os.Getenv(fakeAdbChildEnv) != "":
		time.Sleep(5 * time.Second)
	case os.Getenv(fakeAdbEnv) != "":
		os.Exit(runFakeAdb())
	default:
		os.Exit(m.Run())
	}
}

// runFakeAdb plays the spec beside this binary (symlinks resolved) and returns the exit code. A
// child's pid goes to a file beside the spec, so the test can end it.
func runFakeAdb() int {
	self, err := filepath.EvalSymlinks(os.Args[0])
	if err != nil {
		self = os.Args[0]
	}
	var spec fakeAdbSpec
	specBytes, err := os.ReadFile(self + ".json")
	if err == nil {
		err = json.Unmarshal(specBytes, &spec)
	}
	if err == nil && spec.Marker != "" {
		err = os.WriteFile(spec.Marker, nil, 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake adb:", err)
		return 2
	}
	fmt.Print(spec.Output)
	if spec.Child {
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), fakeAdbChildEnv+"=1")
		child.Stdout = os.Stdout // inherited, so the child keeps the pipe after the fake exits
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "fake adb:", err)
			return 2
		}
		if err := os.WriteFile(self+".child", []byte(strconv.Itoa(child.Process.Pid)), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "fake adb:", err)
			return 2
		}
	}
	time.Sleep(spec.HangFor)
	return spec.ExitCode
}

// installFakeAdb puts this test binary in dir as adb (adb.exe on Windows) with spec beside it and
// returns its path. A hard link off Windows, where a running image cannot be unlinked; else a copy.
func installFakeAdb(t *testing.T, dir string, spec fakeAdbSpec) string {
	t.Helper()
	t.Setenv(fakeAdbEnv, "1")
	executable, err := os.Executable()
	require.NoError(t, err)
	name := "adb"
	if runtime.GOOS == "windows" {
		name = "adb.exe"
	}
	path := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	if runtime.GOOS == "windows" || os.Link(executable, path) != nil {
		content, err := os.ReadFile(executable)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, content, 0o755))
	}
	specBytes, err := json.Marshal(spec)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path+".json", specBytes, 0o644))
	t.Cleanup(func() { killFakeChild(path + ".child") })
	return path
}

// killFakeChild ends the child a fake adb recorded in pidFile, so its directory can go.
func killFakeChild(pidFile string) {
	content, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(content)))
	if err != nil {
		return
	}
	if process, err := os.FindProcess(pid); err == nil {
		_ = process.Kill()
	}
}

// isolateLocalAdb hides every adb from the row: the SDK variables empty, PATH and the home and
// local-app-data directories at empty ones.
func isolateLocalAdb(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	t.Setenv("ANDROID_HOME", "")
	t.Setenv("ANDROID_SDK_ROOT", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", t.TempDir())
}

// fakeAdbOnPath isolates the row, then puts a fake adb alone on PATH and returns its path.
func fakeAdbOnPath(t *testing.T, spec fakeAdbSpec) string {
	t.Helper()
	isolateLocalAdb(t)
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	return installFakeAdb(t, dir, spec)
}

// sdkPlatformTools makes an SDK root, exports it in variable and returns its platform-tools dir.
func sdkPlatformTools(t *testing.T, variable string) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv(variable, root)
	return filepath.Join(root, "platform-tools")
}
