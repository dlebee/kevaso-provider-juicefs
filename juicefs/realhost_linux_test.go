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
// answer (K-052, K-053). No cloud and no database server: the file system's metadata is a SQLite
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

// mountForeground mounts fs at dir with the provider's arguments but in the foreground ("-d"
// dropped), so the test holds the JuiceFS client's process: on a host, that client.
func mountForeground(t *testing.T, fs FS, dir string, o MountOptions) *exec.Cmd {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var args []string
	for _, a := range MountArgs(fs, dir, o) {
		if a != "-d" {
			args = append(args, a)
		}
	}
	c := exec.Command("juicefs", args...)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = c.Process.Signal(syscall.SIGCONT)
		_, _ = run("", "umount", "--force", dir)
		_ = c.Process.Kill()
		_, _ = c.Process.Wait()
	})
	waitMounted(t, dir)
	return c
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

// K-053 (suspected): the fence kills a silo's processes, not the host's JuiceFS client. Data the
// silo wrote and the client had not committed is committed when that client runs again, possibly
// after the silo started on another host and wrote there. Here one host's client is frozen
// (SIGSTOP: it can do nothing, as when it can't reach the metadata) while its silo's writer is
// killed (the fence); the volume is mounted on a second host, written and closed; then the first
// client runs again. The test fails if the first host's late write ends up in the file. Not
// covered: a real partition from a networked metadata database (a frozen client is the nearest
// one host can do with SQLite), and a client that loses its session to the database's timeout
// first, which is what JuiceFS's sessions are meant to catch.
func TestRealHostNoLateWriteAfterAFence(t *testing.T) {
	fs, root := realFS(t)
	if err := os.MkdirAll(filepath.Join(root, "volumes/a/data"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	h1, h2 := filepath.Join(dir, "host1"), filepath.Join(dir, "host2")
	client1 := mountForeground(t, fs, h1, MountOptions{Subdir: "volumes/a/data", CacheDir: filepath.Join(dir, "cache1")})
	mount(t, fs, h2, MountOptions{Subdir: "volumes/a/data", CacheDir: filepath.Join(dir, "cache2")})

	// The silo on host 1 writes and keeps the file open (nothing synced).
	writer := exec.Command("sh", "-c", `exec 3>"$1/db"; printf 'LATE-FROM-HOST-1' >&3; exec sleep 600`, "sh", h1)
	if err := writer.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	// Host 1 is cut off (its client frozen), and its silo fenced (killed).
	if err := client1.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	_ = writer.Process.Kill()
	go func() { _, _ = writer.Process.Wait() }() // its close may wait on the frozen client

	// The silo starts on host 2 and writes the file.
	if err := os.WriteFile(filepath.Join(h2, "db"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(h2, "db")); err != nil || string(b) != "new" {
		t.Fatalf("host 2's write: %q %v", b, err)
	}
	// Host 1 comes back.
	if err := client1.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Second)
	_, _ = run("", "umount", h1)
	time.Sleep(2 * time.Second) // past the second host's attribute cache
	b, err := os.ReadFile(filepath.Join(root, "volumes/a/data/db"))
	t.Logf("the file after host 1 came back: %q %v", b, err)
	if strings.Contains(string(b), "LATE") {
		t.Errorf("K-053: host 1's write from before the fence landed after host 2's: %q", b)
	}
}
