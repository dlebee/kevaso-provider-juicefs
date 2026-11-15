//go:build linux

package juicefs

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Tests on a real Linux host with the juicefs command, for the two audit findings the fakes can't
// answer (K-052). No cloud and no database server: the file system's metadata is a SQLite
// file and its data a local folder (juicefs's own sqlite3:// and --storage file). They need root,
// /dev/fuse and juicefs (1.4 or later) on PATH, and run only with KEVASO_JUICEFS_REAL=1:
//
//	sudo KEVASO_JUICEFS_REAL=1 go test -count=1 -v -run 'TestRealHost' ./juicefs/
//
// The mounts are made with the provider's own arguments (FormatArgs, MountArgs), so they test what
// the node side does.

// realFS formats a JuiceFS file system for one test and mounts its root (as the controller side
// does); it returns the file system and the root's mount point.
func realFS(t *testing.T) (FS, string) {
	t.Helper()
	if os.Getenv("KEVASO_JUICEFS_REAL") == "" {
		t.Skip("set KEVASO_JUICEFS_REAL=1 on a Linux host with juicefs, /dev/fuse and root")
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root (FUSE mounts)")
	}
	if _, err := exec.LookPath("juicefs"); err != nil {
		t.Skip("no juicefs on PATH")
	}
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("no /dev/fuse")
	}
	dir := t.TempDir()
	fs := FS{Name: "kvt", Metadata: "sqlite3://" + filepath.Join(dir, "meta.db"), Storage: "file", Bucket: filepath.Join(dir, "bucket") + "/"}
	juicefs(t, "", FormatArgs(fs)...)
	root := filepath.Join(dir, "root")
	mount(t, fs, root, MountOptions{CacheDir: filepath.Join(dir, "cache-root"), Log: filepath.Join(dir, "root.log")})
	return fs, root
}

func juicefs(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	out, err := run(cwd, args...)
	if err != nil {
		t.Fatalf("juicefs %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func run(cwd string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, "juicefs", args...)
	c.Dir = cwd
	var b bytes.Buffer
	c.Stdout, c.Stderr = &b, &b
	err := c.Run()
	return b.String(), err
}

// mount mounts fs at dir with the provider's arguments, in the background (as the plugin does),
// and unmounts it when the test ends.
func mount(t *testing.T, fs FS, dir string, o MountOptions) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	juicefs(t, "", MountArgs(fs, dir, o)...)
	waitMounted(t, dir)
	t.Cleanup(func() { _, _ = run("", "umount", "--force", dir) })
}

func waitMounted(t *testing.T, dir string) {
	t.Helper()
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if m, err := (Exec{}).Mounted(dir); err == nil && m {
			return
		}
	}
	t.Fatalf("%s never mounted", dir)
}

// K-052 (suspected): a silo's mount is a sub-folder of the class's file system (--subdir) with
// JuiceFS's own files renamed .jfs.* and hidden from listings (--prefix-internal --hide-internal),
// but the control file is still there by name. If it answers about inodes outside the sub-folder,
// root in a silo reaches a sibling volume. This test asks it, from volume a's mount, about a file
// of volume b, by inode, the way `juicefs info --inode` does: the test fails if the answer
// describes b's file. Not covered: messages that change or delete (rmr) by inode, which the juicefs
// command only sends for paths; a raw message to the control file could still try.
func TestRealHostControlFileDoesNotReachASiblingVolume(t *testing.T) {
	fs, root := realFS(t)
	for _, d := range []string{"volumes/a/data", "volumes/b/data"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(root, "volumes/b/data/secret-of-b")
	if err := os.WriteFile(secret, []byte("sibling's secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(secret)
	if err != nil {
		t.Fatal(err)
	}
	ino := strconv.FormatUint(st.Sys().(*syscall.Stat_t).Ino, 10)

	silo := filepath.Join(t.TempDir(), "silo-a")
	mount(t, fs, silo, MountOptions{Subdir: "volumes/a/data", CacheDir: filepath.Join(t.TempDir(), "cache-a")})
	entries, _ := os.ReadDir(silo)
	for _, e := range entries {
		if strings.Contains(e.Name(), "control") {
			t.Errorf("the control file is listed in the silo's volume: %s", e.Name())
		}
	}
	var control string
	for _, n := range []string{".jfs.control", ".control"} {
		if f, err := os.OpenFile(filepath.Join(silo, n), os.O_RDWR, 0); err == nil {
			_ = f.Close()
			control = n
			break
		}
	}
	t.Logf("control file openable by name, read-write, as root at the silo's volume root: %q", control)

	out, err := run(silo, "info", "--inode", ino)
	t.Logf("juicefs info --inode %s, in volume a's mount: err=%v\n%s", ino, err, out)
	switch {
	case strings.Contains(out, "secret-of-b") || strings.Contains(out, "volumes/b"):
		t.Errorf("K-052: volume a's mount answered about volume b's file (inode %s) through its control file", ino)
	case control == "" || (err != nil && strings.Contains(out, "control")):
		t.Skipf("inconclusive: the command didn't reach the control file (output above)")
	case err == nil:
		t.Errorf("K-052: juicefs info --inode %s from volume a's mount succeeded; read its output above", ino)
	}
}
