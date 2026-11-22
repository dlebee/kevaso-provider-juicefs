package juicefs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// FS is one JuiceFS file system as the juicefs command needs it: where its metadata and data are,
// and the credentials to reach them. The credentials only ever go in the command's environment
// (ACCESS_KEY, SECRET_KEY, META_PASSWORD, as JuiceFS reads them), never in its arguments, so
// they don't show in ps or in logs.
type FS struct {
	Name     string // the file system's name (juicefs format … NAME)
	Metadata string // the metadata engine's URL, without a password
	Storage  string // the object store's type, e.g. s3
	Bucket   string // its URL
	Env      []string
}

// MountOptions are how a mount is made.
type MountOptions struct {
	Subdir       string // mount this folder of the file system as the root ("": all of it)
	CacheDir     string
	CacheSizeMiB int
	// Log is the background mount's log file (juicefs's default, /var/log/juicefs.log, is read-only
	// to the controller side's unit).
	Log string
}

// Runner runs the juicefs command. Exec is the real one; tests use a fake.
type Runner interface {
	// Status reports whether the file system has been formatted (juicefs status).
	Status(ctx context.Context, fs FS) (formatted bool, err error)
	// Format formats it (juicefs format --no-update): only ever called when Status said it wasn't.
	Format(ctx context.Context, fs FS) error
	// Mount mounts it at dir, in the background (juicefs mount -d).
	Mount(ctx context.Context, fs FS, dir string, o MountOptions) error
	// Umount unmounts what is at dir (umount(2), as root; not fusermount).
	Umount(ctx context.Context, dir string) error
	// Mounted reports whether a JuiceFS mount is at dir.
	Mounted(dir string) (bool, error)
	// SetQuota holds a folder of the file system (its path from the root, e.g. /volumes/UID/NAME)
	// to capacityGiB (juicefs quota set --capacity): writes past it fail with "quota exceeded".
	// Setting it again changes it at once, on every client (it grows online).
	SetQuota(ctx context.Context, fs FS, dir string, capacityGiB int64) error
}

// Exec runs the juicefs binary.
type Exec struct {
	Binary string // default "juicefs" on PATH
	// MountInfo is the mount table to read (default /proc/self/mountinfo).
	MountInfo string
	// Command runs one command and returns its combined output (default: os/exec); tests replace it.
	Command func(ctx context.Context, name string, args, env []string) (out []byte, code int, err error)
	// Unmount unmounts one mount point (default: umount(2) with no flags); tests replace it.
	Unmount func(dir string) error
}

func (e Exec) run(ctx context.Context, env []string, args ...string) ([]byte, int, error) {
	bin := e.Binary
	if bin == "" {
		bin = "juicefs"
	}
	cmd := e.Command
	if cmd == nil {
		cmd = runCommand
	}
	return cmd(ctx, bin, args, env)
}

func runCommand(ctx context.Context, name string, args, env []string) ([]byte, int, error) {
	c := exec.CommandContext(ctx, name, args...)
	c.Env = append([]string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}, env...)
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	err := c.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out.Bytes(), ee.ExitCode(), nil
	}
	return out.Bytes(), 0, err
}

func failed(what string, out []byte, code int, err error) error {
	if err != nil {
		return fmt.Errorf("juicefs %s: %w", what, err)
	}
	if code != 0 {
		return fmt.Errorf("juicefs %s: exit %d: %s", what, code, lastLines(out, 5))
	}
	return nil
}

// lastLines keeps an error short: juicefs logs a lot before the line that matters.
func lastLines(b []byte, n int) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// Status implements Runner.
func (e Exec) Status(ctx context.Context, fs FS) (bool, error) {
	out, code, err := e.run(ctx, fs.Env, "status", fs.Metadata)
	if err == nil && code != 0 && strings.Contains(string(out), "not formatted") {
		return false, nil
	}
	if err := failed("status", out, code, err); err != nil {
		return false, err
	}
	return true, nil
}

// Format implements Runner.
func (e Exec) Format(ctx context.Context, fs FS) error {
	out, code, err := e.run(ctx, fs.Env, FormatArgs(fs)...)
	return failed("format", out, code, err)
}

// FormatArgs are juicefs format's arguments: --no-update, so it never changes a file system that
// is already there; --trash-days 0, so a deleted volume's data goes at once (JuiceFS's default
// keeps removed files, and their objects in the bucket, a day in a trash no silo can see).
func FormatArgs(fs FS) []string {
	return []string{"format", "--no-update", "--trash-days", "0", "--storage", fs.Storage, "--bucket", fs.Bucket, fs.Metadata, fs.Name}
}

// Mount implements Runner.
func (e Exec) Mount(ctx context.Context, fs FS, dir string, o MountOptions) error {
	out, code, err := e.run(ctx, fs.Env, MountArgs(fs, dir, o)...)
	return failed("mount "+dir, out, code, err)
}

// MountArgs are juicefs mount's arguments.
func MountArgs(fs FS, dir string, o MountOptions) []string {
	args := []string{"mount", "-d"}
	if o.Subdir != "" {
		// A volume's mount is a silo's: JuiceFS's own files at its root (.accesslog, .config,
		// .stats, .control) get a .jfs prefix, so they don't shadow the silo's files of those
		// names (a lookup at the root answers with them first), and are left out of listings.
		args = append(args, "--subdir", o.Subdir, "--prefix-internal", "--hide-internal")
	}
	if o.CacheDir != "" {
		args = append(args, "--cache-dir", o.CacheDir)
	}
	if o.CacheSizeMiB > 0 {
		args = append(args, "--cache-size", strconv.Itoa(o.CacheSizeMiB))
	}
	if o.Log != "" {
		args = append(args, "--log", o.Log)
	}
	return append(args, fs.Metadata, dir)
}

// SetQuota implements Runner.
func (e Exec) SetQuota(ctx context.Context, fs FS, dir string, capacityGiB int64) error {
	out, code, err := e.run(ctx, fs.Env, QuotaArgs(fs, dir, capacityGiB)...)
	return failed("quota set "+dir, out, code, err)
}

// QuotaArgs are juicefs quota set's arguments (JuiceFS 1.1 and later; capacity in GiB).
func QuotaArgs(fs FS, dir string, capacityGiB int64) []string {
	return []string{"quota", "set", fs.Metadata, "--path", dir, "--capacity", strconv.FormatInt(capacityGiB, 10)}
}

// Umount implements Runner. It unmounts with umount(2), as root, not with juicefs umount: that
// runs fusermount, and Ubuntu's fuse3 confines fusermount with an AppArmor profile that only lets
// it unmount under home folders, /mnt, /media, /run/user and /tmp. A volume's mount point is under
// /var/lib/kevaso, so fusermount is refused there and the volume never lets go. The node side
// runs as root with CAP_SYS_ADMIN, so it doesn't need fusermount. The JuiceFS client exits once
// its mount is gone. A busy mount fails (EBUSY) and is tried again; it is never lazily detached.
func (e Exec) Umount(ctx context.Context, dir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	unmount := e.Unmount
	if unmount == nil {
		unmount = func(dir string) error { return syscall.Unmount(dir, 0) }
	}
	if err := unmount(dir); err != nil {
		return fmt.Errorf("juicefs umount %s: %w", dir, err)
	}
	return nil
}

// Mounted implements Runner.
func (e Exec) Mounted(dir string) (bool, error) {
	p := e.MountInfo
	if p == "" {
		p = "/proc/self/mountinfo"
	}
	f, err := os.Open(p)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	return MountedIn(f, dir)
}

// MountedIn reports whether a mount table (the mountinfo format) has a JuiceFS mount at dir.
func MountedIn(r io.Reader, dir string) (bool, error) {
	dir = filepath.Clean(dir)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		// ID PARENT MAJ:MIN ROOT MOUNTPOINT OPTIONS [OPTIONAL…] - FSTYPE SOURCE SUPER
		f := strings.Fields(sc.Text())
		if len(f) < 7 {
			continue
		}
		sep := -1
		for i := 6; i < len(f); i++ {
			if f[i] == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+1 >= len(f) {
			continue
		}
		if unescape(f[4]) == dir && strings.HasPrefix(f[sep+1], "fuse.juicefs") {
			return true, nil
		}
	}
	return false, sc.Err()
}

// unescape undoes mountinfo's octal escapes (\040 is a space).
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

var _ Runner = Exec{}
