package juicefs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kevaso.com/core/sdk/kplugin"
)

// Node is the node side, on each worker: it mounts a volume's folder of the class's file system at
// the path kevasod gives (below kevasod's volumes folder only), with the class's credentials in
// juicefs's environment, and unmounts it when the silo leaves. kevasod hands the folder to the
// container; nothing else crosses into the silo.
type Node struct {
	Config
	Run     Runner
	Secrets Secrets
	// CallTimeout bounds each juicefs command (default DefaultCallTimeout), as on the controller side.
	CallTimeout time.Duration
	Log         func(string, ...any)
}

func (n *Node) bound(ctx context.Context) (context.Context, context.CancelFunc) {
	return (&Provider{CallTimeout: n.CallTimeout}).bound(ctx)
}

// NewNode returns the node side.
func NewNode(cfg Config, run Runner, secrets Secrets) *Node {
	return &Node{Config: cfg, Run: run, Secrets: secrets}
}

func (n *Node) logf(f string, a ...any) {
	if n.Log != nil {
		n.Log(f, a...)
	}
}

// Info implements kplugin.Plugin.
func (n *Node) Info(context.Context) (kplugin.Info, error) {
	return kplugin.Info{Name: Name, Version: Version, Protocol: kplugin.Protocol, Storage: n.classes()}, nil
}

func (n *Node) volumesDir() string {
	if n.VolumesDir != "" {
		return n.VolumesDir
	}
	return DefaultVolumesDir
}

// checkPath accepts only a folder below kevasod's volumes folder: the node side mounts as root,
// so it never mounts over anything else.
func (n *Node) checkPath(p string) (string, error) {
	base := filepath.Clean(n.volumesDir())
	c := filepath.Clean(p)
	if !filepath.IsAbs(p) || c != p || !strings.HasPrefix(c, base+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q: not below %s", p, base)
	}
	return c, nil
}

func (n *Node) cacheDir(cl Class) string {
	if cl.CacheDir != "" {
		return cl.CacheDir
	}
	return filepath.Join(DefaultCacheDir, cl.Name)
}

// MountVolume implements kplugin.Node.
func (n *Node) MountVolume(ctx context.Context, m kplugin.NodeMount) error {
	cl, ok := n.class(m.Class)
	if !ok {
		return fmt.Errorf("storage class %s: %w", m.Class, kplugin.ErrUnsupported)
	}
	if err := checkHandle(m.Handle); err != nil {
		return err
	}
	dir, err := n.checkPath(m.Path)
	if err != nil {
		return err
	}
	if mounted, err := n.Run.Mounted(dir); err != nil {
		return err
	} else if mounted {
		return nil
	}
	fs, err := fsOf(cl, n.Secrets)
	if err != nil {
		return err
	}
	// The mount point, and the silo's folder above it. The VMM does not walk these.
	// Its unit hides /var/lib/kevaso behind a tmpfs and bind-mounts only this path,
	// and once juicefs is mounted the mode of that path is the volume folder's in
	// the file system (volumeDirMode), which is what kevaso-vmm traverses.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ctx, cancel := n.bound(ctx)
	defer cancel()
	if err := n.Run.Mount(ctx, fs, dir, MountOptions{Subdir: m.Handle, CacheDir: n.cacheDir(cl), CacheSizeMiB: cl.CacheSizeMiB}); err != nil {
		return timedOut(ctx, "juicefs mount", err)
	}
	n.logf("class %s: mounted %s at %s", cl.Name, m.Handle, dir)
	return nil
}

// UnmountVolume implements kplugin.Node.
func (n *Node) UnmountVolume(ctx context.Context, m kplugin.NodeMount) error {
	dir, err := n.checkPath(m.Path)
	if err != nil {
		return err
	}
	if mounted, err := n.Run.Mounted(dir); err != nil || !mounted {
		return err
	}
	ctx, cancel := n.bound(ctx)
	defer cancel()
	if err := n.Run.Umount(ctx, dir); err != nil {
		return timedOut(ctx, "juicefs umount", err)
	}
	n.logf("unmounted %s", dir)
	return nil
}

var (
	_ kplugin.Plugin = (*Node)(nil)
	_ kplugin.Node   = (*Node)(nil)
)
