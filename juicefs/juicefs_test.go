package juicefs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"kevaso.com/core/sdk/kplugin"
	"kevaso.com/core/sdk/kplugin/kplugintest"
)

const testMeta = "postgres://juicefs@10.63.0.1:5432/juicefs"

func testConfig(t *testing.T) Config {
	return Config{
		Classes: []Class{{Name: "juicefs", Metadata: testMeta, Bucket: "https://s3.example.com/acme-volumes",
			Credentials: Credentials{AccessKey: "bucket-key", SecretKey: "bucket-secret", MetaPassword: "meta-password"}}},
		StateDir:   t.TempDir(),
		VolumesDir: t.TempDir(),
	}
}

func testSecrets(name string) (string, error) {
	if name == "missing" {
		return "", os.ErrNotExist
	}
	return "s3cr3t-" + name, nil
}

// Both sides keep the plugin protocol's promises (the same suite the fake runs in core).
func TestConformance(t *testing.T) {
	cfg := testConfig(t)
	run := newFakeRunner()
	kplugintest.Conformance(t, kplugintest.Suite{
		Plugin:   New(cfg, run, testSecrets),
		Node:     NewNode(cfg, run, testSecrets),
		MountDir: cfg.VolumesDir,
	})
}

func TestInfo(t *testing.T) {
	i, _ := New(testConfig(t), newFakeRunner(), testSecrets).Info(context.Background())
	if len(i.Storage) != 1 || i.Storage[0].Topology != kplugin.TopologyGlobal || !i.Storage[0].Mounted() || i.Storage[0].OwnHostsOnly {
		t.Fatalf("a juicefs class is global, mounted by the node side, on any host: %+v", i.Storage)
	}
}

func TestConfigCheck(t *testing.T) {
	ok := Class{Name: "juicefs", Metadata: testMeta, Bucket: "https://b"}
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"fine", Config{Classes: []Class{ok}}, ""},
		{"no classes", Config{}, "no classes"},
		{"bad name", Config{Classes: []Class{{Name: "Juice FS", Metadata: testMeta, Bucket: "b"}}}, "name"},
		{"twice", Config{Classes: []Class{ok, ok}}, "twice"},
		{"no metadata", Config{Classes: []Class{{Name: "j", Bucket: "b"}}}, "no metadata"},
		{"a password in the metadata URL", Config{Classes: []Class{{Name: "j", Metadata: "postgres://juicefs:hunter2@db:5432/jfs", Bucket: "b"}}}, "holds a password"},
		{"a redis password too", Config{Classes: []Class{{Name: "j", Metadata: "redis://:hunter2@db:6379/1", Bucket: "b"}}}, "holds a password"},
		{"a port is no password", Config{Classes: []Class{{Name: "j", Metadata: "redis://db:6379/1", Bucket: "b"}}}, ""},
		{"no bucket", Config{Classes: []Class{{Name: "j", Metadata: testMeta}}}, "no bucket"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Check()
			if (tc.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestHandleFor(t *testing.T) {
	for id, want := range map[string]string{
		"0b6f2c1e-7a/home": "volumes/0b6f2c1e-7a/home",
		"u/data":           "volumes/u/data",
		"u/../etc":         "",
		"../x":             "",
		"u":                "",
		"u/a/b":            "",
		"u/.hidden":        "",
		"/data":            "",
	} {
		got, err := HandleFor(id)
		if got != want || (want == "") != (err != nil) {
			t.Errorf("%q: %q %v, want %q", id, got, err, want)
		}
	}
}

// The file system is formatted once, only when it never was; credentials go in the environment.
func TestFormatsOnlyOnce(t *testing.T) {
	ctx := context.Background()
	for _, already := range []bool{false, true} {
		run := newFakeRunner()
		run.formatted[testMeta] = already
		cfg := testConfig(t)
		p := New(cfg, run, testSecrets)
		for _, id := range []string{"u1/home", "u1/home", "u2/data"} {
			st, err := p.EnsureVolume(ctx, kplugin.Volume{Class: "juicefs", ID: id, Region: "ca-central"})
			if err != nil || st.Handle != "volumes/"+id || st.Region != "" {
				t.Fatalf("ensure %s: %+v %v", id, st, err)
			}
			if fi, err := os.Stat(filepath.Join(cfg.StateDir, "mnt", "juicefs", "volumes", id)); err != nil || !fi.IsDir() {
				t.Fatalf("the volume's folder: %v", err)
			}
		}
		want := 1
		if already {
			want = 0
		}
		if n := run.count("format"); n != want {
			t.Fatalf("already formatted %v: formatted %d times (%v)", already, n, run.calls)
		}
		if n := run.count("mount"); n != 1 {
			t.Fatalf("the root is mounted once: %d (%v)", n, run.calls)
		}
		for i, c := range run.calls {
			if strings.Contains(c, "s3cr3t") {
				t.Fatalf("a credential in the arguments: %s", c)
			}
			if strings.HasPrefix(c, "status") || strings.HasPrefix(c, "format") || strings.HasPrefix(c, "mount") {
				if !slices.Contains(run.envs[i], "META_PASSWORD=s3cr3t-meta-password") || !slices.Contains(run.envs[i], "ACCESS_KEY=s3cr3t-bucket-key") {
					t.Fatalf("%s without its credentials: %v", c, len(run.envs[i]))
				}
			}
		}
	}
}

// Deleting a silo's volumes leaves nothing of it in the file system: each deleted silo left an
// empty volumes/UID folder behind.
func TestDeleteLeavesNothing(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	p := New(cfg, newFakeRunner(), testSecrets)
	for _, id := range []string{"u1/home", "u1/data"} {
		if _, err := p.EnsureVolume(ctx, kplugin.Volume{Class: "juicefs", ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	silo := filepath.Join(cfg.StateDir, "mnt", "juicefs", "volumes", "u1")
	if err := p.DeleteVolume(ctx, kplugin.VolumeRef{Class: "juicefs", Handle: "volumes/u1/home"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(silo, "data")); err != nil {
		t.Fatalf("the silo's other volume went too: %v", err)
	}
	if err := p.DeleteVolume(ctx, kplugin.VolumeRef{Class: "juicefs", Handle: "volumes/u1/data"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(silo); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("volumes/u1 is left: %v", err)
	}
}

func TestControllerErrors(t *testing.T) {
	ctx := context.Background()
	run := newFakeRunner()
	p := New(testConfig(t), run, testSecrets)
	if _, err := p.EnsureVolume(ctx, kplugin.Volume{Class: "other", ID: "u/x"}); !errors.Is(err, kplugin.ErrUnsupported) {
		t.Fatalf("another class: %v", err)
	}
	if _, err := p.EnsureVolume(ctx, kplugin.Volume{Class: "juicefs", ID: "../x"}); err == nil {
		t.Fatal("a bad ID")
	}
	if _, err := p.AttachVolume(ctx, kplugin.Attachment{Class: "juicefs", Handle: "volumes/u/x"}); !errors.Is(err, kplugin.ErrUnsupported) {
		t.Fatalf("attach: %v", err)
	}
	if err := p.DeleteVolume(ctx, kplugin.VolumeRef{Class: "juicefs", Handle: "/etc"}); !errors.Is(err, kplugin.ErrNotFound) {
		t.Fatalf("delete a foreign handle: %v", err)
	}
	cfg := testConfig(t)
	cfg.Classes[0].Credentials.MetaPassword = "missing"
	if _, err := New(cfg, run, testSecrets).EnsureVolume(ctx, kplugin.Volume{Class: "juicefs", ID: "u/x"}); err == nil || !strings.Contains(err.Error(), "credential missing") {
		t.Fatalf("a missing credential: %v", err)
	}
}

// The node side mounts the volume's folder (--subdir) where kevasod says, only below its volumes
// folder, once; and unmounts only what is mounted.
func TestNodeMounts(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	run := newFakeRunner()
	run.formatted[testMeta] = true
	n := NewNode(cfg, run, testSecrets)
	path := filepath.Join(cfg.VolumesDir, "u1", "home")
	m := kplugin.NodeMount{Class: "juicefs", Handle: "volumes/u1/home", ID: "u1/home", Path: path}
	for i := 0; i < 2; i++ {
		if err := n.MountVolume(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if run.count("mount") != 1 || run.mounts[path] != "volumes/u1/home" {
		t.Fatalf("mounted once, its folder: %v %v", run.calls, run.mounts)
	}
	for _, bad := range []kplugin.NodeMount{
		{Class: "juicefs", Handle: "volumes/u1/home", Path: "/etc"},
		{Class: "juicefs", Handle: "volumes/u1/home", Path: cfg.VolumesDir},
		{Class: "juicefs", Handle: "volumes/u1/home", Path: cfg.VolumesDir + "/../x"},
		{Class: "juicefs", Handle: "volumes/u1/home", Path: "relative/x"},
		{Class: "juicefs", Handle: "/", Path: path + "2"},
		{Class: "other", Handle: "volumes/u1/home", Path: path + "2"},
	} {
		if err := n.MountVolume(ctx, bad); err == nil {
			t.Errorf("mounted %+v", bad)
		}
	}
	for i := 0; i < 2; i++ {
		if err := n.UnmountVolume(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if run.count("umount") != 1 || len(run.mounts) != 0 {
		t.Fatalf("unmounted once: %v", run.calls)
	}
	run.fail["mount"] = errors.New("juicefs mount: exit 1: connection refused")
	if err := n.MountVolume(ctx, m); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("a failed mount: %v", err)
	}
}

func TestArgs(t *testing.T) {
	fs := FS{Name: "kevaso", Metadata: testMeta, Storage: "s3", Bucket: "https://b/x", Env: []string{"META_PASSWORD=p"}}
	// No trash: JuiceFS 1.4 keeps removed files a day by default (format --trash-days 1), so a
	// deleted volume's objects would have stayed in the bucket.
	if got := strings.Join(FormatArgs(fs), " "); got != "format --no-update --trash-days 0 --storage s3 --bucket https://b/x "+testMeta+" kevaso" {
		t.Fatalf("format: %s", got)
	}
	got := strings.Join(MountArgs(fs, "/var/lib/kevaso/volumes/u/home", MountOptions{Subdir: "volumes/u/home", CacheDir: "/var/cache/j", CacheSizeMiB: 1024}), " ")
	// A volume's mount hides JuiceFS's own files from the silo, and keeps them off its names: on
	// the first real mount the silo's folder listed .accesslog, .config and .stats next to its
	// own files, and a file of its own named .config would have been shadowed by them.
	if got != "mount -d --subdir volumes/u/home --prefix-internal --hide-internal --cache-dir /var/cache/j --cache-size 1024 "+testMeta+" /var/lib/kevaso/volumes/u/home" {
		t.Fatalf("mount: %s", got)
	}
	// Exec hands the credentials to the command's environment, never its arguments.
	var gotArgs, gotEnv []string
	e := Exec{Command: func(_ context.Context, name string, args, env []string) ([]byte, int, error) {
		gotArgs, gotEnv = append([]string{name}, args...), env
		return []byte("2026/11/13 juicefs[1] <FATAL>: load setting: database is not formatted"), 1, nil
	}}
	formatted, err := e.Status(context.Background(), fs)
	if err != nil || formatted || gotArgs[0] != "juicefs" || !slices.Equal(gotEnv, fs.Env) || slices.Contains(gotArgs, "p") {
		t.Fatalf("status: %v %v %v %v", formatted, err, gotArgs, gotEnv)
	}
	e.Command = func(context.Context, string, []string, []string) ([]byte, int, error) {
		return []byte("line\n<FATAL>: dial tcp 10.63.0.1:5432: connect: connection refused"), 1, nil
	}
	if _, err := e.Status(context.Background(), fs); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("status, database down: %v", err)
	}
}

func TestMountedIn(t *testing.T) {
	table := `22 1 0:21 / /proc rw,nosuid shared:5 - proc proc rw
101 30 0:52 / /var/lib/kevaso/volumes/u1/home rw,relatime shared:60 - fuse.juicefs JuiceFS:kevaso rw,user_id=0
102 30 0:53 / /var/lib/kevaso/volumes/u2/my\040data rw,relatime - fuse.juicefs JuiceFS:kevaso rw
103 30 8:16 / /var/lib/kevaso/volumes/u3/data rw - ext4 /dev/sdb rw
`
	for dir, want := range map[string]bool{
		"/var/lib/kevaso/volumes/u1/home":    true,
		"/var/lib/kevaso/volumes/u1/home/":   true,
		"/var/lib/kevaso/volumes/u2/my data": true,
		"/var/lib/kevaso/volumes/u3/data":    false, // mounted, but not JuiceFS
		"/var/lib/kevaso/volumes/u4/data":    false,
	} {
		if got, err := MountedIn(strings.NewReader(table), dir); err != nil || got != want {
			t.Errorf("%s: %v %v, want %v", dir, got, err, want)
		}
	}
}
