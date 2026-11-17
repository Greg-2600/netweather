package logging_test

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Greg-2600/netweather/internal/logging"
)

func TestRotatingFileRotatesAndKeeps(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "netweather.log")

	rf, err := logging.NewRotatingFile(path, 1, 2)
	if err != nil {
		t.Fatalf("NewRotatingFile: %v", err)
	}

	chunk := bytes.Repeat([]byte("x"), 256*1024)
	for i := 0; i < 9; i++ { // 2.25 MB through a 1 MB file
		if _, err := rf.Write(chunk); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if err := rf.Close(); err != nil {
		t.Fatal(err)
	}

	for _, suffix := range []string{".1", ".2"} {
		if _, err := os.Stat(path + suffix); err != nil {
			t.Errorf("expected rotated file %s: %v", suffix, err)
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Errorf("rotated file .3 should have been dropped, got err=%v", err)
	}
}

func TestRotatingFileReopensAfterClose(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "again.log")

	rf, err := logging.NewRotatingFile(path, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := rf.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := rf.Write([]byte("x")); err == nil {
		t.Fatal("write after close should fail")
	}
}

func TestRotatingFilePermissions(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("Windows ignores POSIX file modes; the mode is enforced on Linux, which is the release platform")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "sensitive.log")
	rf, err := logging.NewRotatingFile(path, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rf.Write([]byte("target=chase.com\n")); err != nil {
		t.Fatal(err)
	}
	if err := rf.Close(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("log file mode = %o, want 600 (it contains the target list)", perm)
	}
}
