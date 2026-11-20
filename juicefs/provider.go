// Package juicefs is Kevaso's JuiceFS provider (design §6, "a disk that follows the silo
// anywhere"): storage classes whose volumes live in a JuiceFS file system (data in an
// S3-compatible bucket, metadata in a small database), so any host running the node side can mount
// them, in any region. Each class is topology global and reach mount: nothing is attached; the node
// side on the silo's host mounts the volume where kevasod says, holding the credentials itself, and
// the container gets that folder. The silo never sees a credential.
//
// One JuiceFS file system per class; each Kevaso volume is one folder in it, volumes/UID/NAME
// (the handle), which the node side mounts with --subdir. Two sides, one program:
//
//   - the controller side (Provider, on the control plane) formats the class's file system once,
//     keeps its root mounted in its state folder, and creates and deletes the volumes' folders;
//   - the node side (Node, on each worker) mounts a volume's folder at the path kevasod gives, and
//     unmounts it when the silo leaves.
package juicefs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"kevaso.com/core/sdk/kplugin"
)

// Version of this provider.
var Version = "0.1.0"

// Name is the plugin's name.
const Name = "juicefs"

// Class is one storage class: one JuiceFS file system.
type Class struct {
	Name        string `json:"name"` // e.g. juicefs, or juicefs-r2
	Description string `json:"description,omitempty"`
	// FileSystem is the JuiceFS file system's name (juicefs format … NAME; default kevaso).
	FileSystem string `json:"fileSystem,omitempty"`
	// Metadata is the metadata engine's URL without its password, e.g.
	// postgres://juicefs@10.63.0.1:5432/juicefs or redis://10.63.0.1:6379/1. Every host using the
	// class must reach it: while it's down, reads and writes hang (E029).
	Metadata string `json:"metadata"`
	Storage  string `json:"storage,omitempty"` // the object store's type (default s3)
	Bucket   string `json:"bucket"`            // its URL, e.g. https://ACCOUNT.r2.cloudflarestorage.com/acme-kevaso-volumes
	// CacheDir is the node side's local cache for this class (default /var/cache/kevaso-juicefs/NAME).
	CacheDir     string `json:"cacheDir,omitempty"`
	CacheSizeMiB int    `json:"cacheSizeMiB,omitempty"`
	// Credentials name the secrets (systemd credentials, LoadCredential=NAME:FILE) holding the
	// bucket's key pair and the metadata password; each is optional.
	Credentials Credentials `json:"credentials,omitempty"`
}

// Credentials are the names of a class's secrets, never the secrets themselves.
type Credentials struct {
	AccessKey    string `json:"accessKey,omitempty"`
	SecretKey    string `json:"secretKey,omitempty"`
	MetaPassword string `json:"metaPassword,omitempty"`
}

// Config is the provider's settings (not secret: credentials are named, and come separately).
type Config struct {
	Classes []Class `json:"classes"`
	// StateDir is the controller side's folder: each class's root is mounted in StateDir/mnt/NAME
	// (default /var/lib/kevaso-provider-juicefs).
	StateDir string `json:"stateDir,omitempty"`
	// VolumesDir is the node side's: it only mounts below it (default kevasod's, /var/lib/kevaso/volumes).
	VolumesDir string `json:"volumesDir,omitempty"`
}

// Defaults.
const (
	DefaultStateDir   = "/var/lib/kevaso-provider-juicefs"
	DefaultVolumesDir = "/var/lib/kevaso/volumes"
	DefaultCacheDir   = "/var/cache/kevaso-juicefs"
)

var className = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// Check says what's wrong with a configuration, if anything.
func (c Config) Check() error {
	if len(c.Classes) == 0 {
		return errors.New("no classes: add one under \"classes\"")
	}
	seen := map[string]bool{}
	for i, cl := range c.Classes {
		switch {
		case !className.MatchString(cl.Name):
			return fmt.Errorf("class %d: name %q (lowercase letters, digits, -)", i+1, cl.Name)
		case seen[cl.Name]:
			return fmt.Errorf("class %s twice", cl.Name)
		case cl.Metadata == "":
			return fmt.Errorf("class %s: no metadata URL", cl.Name)
		case strings.Contains(beforeHost(cl.Metadata), ":"):
			return fmt.Errorf("class %s: the metadata URL holds a password: name a credential (credentials.metaPassword) instead", cl.Name)
		case cl.Bucket == "":
			return fmt.Errorf("class %s: no bucket", cl.Name)
		}
		seen[cl.Name] = true
	}
	return nil
}

// beforeHost is a URL's user part (scheme://USER[:PASSWORD]@host…), or "".
func beforeHost(u string) string {
	rest := u
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		return rest[:i]
	}
	return ""
}

func (c Config) class(name string) (Class, bool) {
	for _, cl := range c.Classes {
		if cl.Name == name {
			return cl, true
		}
	}
	return Class{}, false
}

// classes is what Info says: global, mounted by the node side.
func (c Config) classes() []kplugin.Class {
	var out []kplugin.Class
	for _, cl := range c.Classes {
		d := cl.Description
		if d == "" {
			d = "JuiceFS volumes: they follow the silo to any host running the node side"
		}
		out = append(out, kplugin.Class{Name: cl.Name, Description: d, Topology: kplugin.TopologyGlobal, Reach: kplugin.ReachMount})
	}
	return out
}

// Secrets returns a secret by name (a systemd credential).
type Secrets func(name string) (string, error)

// fs is a class's file system with its credentials, read when needed.
func fsOf(cl Class, secrets Secrets) (FS, error) {
	fs := FS{Name: cl.FileSystem, Metadata: cl.Metadata, Storage: cl.Storage, Bucket: cl.Bucket}
	if fs.Name == "" {
		fs.Name = "kevaso"
	}
	if fs.Storage == "" {
		fs.Storage = "s3"
	}
	for _, c := range []struct{ env, name string }{
		{"ACCESS_KEY", cl.Credentials.AccessKey}, {"SECRET_KEY", cl.Credentials.SecretKey}, {"META_PASSWORD", cl.Credentials.MetaPassword},
	} {
		if c.name == "" {
			continue
		}
		if secrets == nil {
			return FS{}, fmt.Errorf("class %s: credential %s: no credentials given", cl.Name, c.name)
		}
		v, err := secrets(c.name)
		if err != nil {
			return FS{}, fmt.Errorf("class %s: credential %s: %w", cl.Name, c.name, err)
		}
		fs.Env = append(fs.Env, c.env+"="+v)
	}
	return fs, nil
}

// volumeDirMode is the mode of every folder created for a volume's path. Traverse
// for anyone, so kevaso-vmm can open a file it owns inside; no read or write for
// anyone but the owner (root).
const volumeDirMode os.FileMode = 0o711

var idPart = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// mkdirAllMode creates path and any missing parent, and sets mode on each folder it
// creates. A folder that already exists is left as it is, mode included. mode is the
// mode the folder ends with: os.Mkdir applies the process umask, and the plugin unit
// runs with UMask=0077.
func mkdirAllMode(path string, mode os.FileMode) error {
	path = filepath.Clean(path)
	var missing []string
	for cur := path; ; {
		fi, err := os.Lstat(cur)
		if err == nil {
			if !fi.IsDir() {
				return fmt.Errorf("%s exists and is not a directory", cur)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		missing = append(missing, cur)
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], mode); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return err
			}
			fi, statErr := os.Lstat(missing[i])
			if statErr != nil {
				return statErr
			}
			if !fi.IsDir() {
				return fmt.Errorf("%s exists and is not a directory", missing[i])
			}
			continue // whoever created it sets its mode
		}
		if err := os.Chmod(missing[i], mode); err != nil {
			return err
		}
	}
	return nil
}

// HandleFor is a volume's folder in its class's file system, from the ID kevasod picks (the silo's
// UID and the volume's name): volumes/UID/NAME.
func HandleFor(id string) (string, error) {
	parts := strings.Split(id, "/")
	if len(parts) != 2 || !idPart.MatchString(parts[0]) || !idPart.MatchString(parts[1]) {
		return "", fmt.Errorf("volume ID %q: want UID/NAME", id)
	}
	return "volumes/" + id, nil
}

// checkHandle accepts only handles HandleFor makes.
func checkHandle(h string) error {
	id, ok := strings.CutPrefix(h, "volumes/")
	if !ok {
		return fmt.Errorf("handle %q isn't one of this plugin's: %w", h, kplugin.ErrNotFound)
	}
	if _, err := HandleFor(id); err != nil {
		return fmt.Errorf("handle %q: %w", h, kplugin.ErrNotFound)
	}
	return nil
}

// Provider is the controller side.
type Provider struct {
	Config
	Run     Runner
	Secrets Secrets
	// DeleteWait is how long DeleteVolume waits for a volume's folder to be removed before
	// answering kplugin.ErrInProgress (default 5s); a big folder takes a while.
	DeleteWait time.Duration
	// CallTimeout bounds each juicefs command a call runs (status, format, mount, quota; default
	// DefaultCallTimeout): past it the command is killed and the call fails, saying so (audit K-041).
	CallTimeout time.Duration
	Log         func(string, ...any)

	mu sync.Mutex // the maps below only: never held across a command
	// classMu: one root mount at a time per class (status, format, mount in order), so a slow
	// class never holds up another's calls, nor a delete's bookkeeping.
	classMu  map[string]*sync.Mutex
	roots    map[string]string        // class → its mounted root
	deleting map[string]chan struct{} // handle → closed when its folder is gone
	delErr   map[string]error
}

// New returns the controller side.
func New(cfg Config, run Runner, secrets Secrets) *Provider {
	return &Provider{Config: cfg, Run: run, Secrets: secrets}
}

func (p *Provider) logf(f string, a ...any) {
	if p.Log != nil {
		p.Log(f, a...)
	}
}

// Info implements kplugin.Plugin.
func (p *Provider) Info(context.Context) (kplugin.Info, error) {
	return kplugin.Info{Name: Name, Version: Version, Protocol: kplugin.Protocol, Storage: p.classes()}, nil
}

func (p *Provider) stateDir() string {
	if p.StateDir != "" {
		return p.StateDir
	}
	return DefaultStateDir
}

// DefaultCallTimeout is how long one juicefs command may take before it is killed.
const DefaultCallTimeout = 2 * time.Minute

// bound is the context of one juicefs command.
func (p *Provider) bound(ctx context.Context) (context.Context, context.CancelFunc) {
	d := p.CallTimeout
	if d <= 0 {
		d = DefaultCallTimeout
	}
	return context.WithTimeout(ctx, d)
}

// timedOut says what a command killed by its deadline was.
func timedOut(ctx context.Context, what string, err error) error {
	if err != nil && ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("%s: gave up, juicefs didn't finish within this call's deadline: %w", what, err)
	}
	return err
}

func (p *Provider) classLock(name string) *sync.Mutex {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.classMu == nil {
		p.classMu = map[string]*sync.Mutex{}
	}
	if p.classMu[name] == nil {
		p.classMu[name] = &sync.Mutex{}
	}
	return p.classMu[name]
}

// root returns the class's root, mounted: the file system is formatted first if it never was,
// and only then (a file system that's there is never formatted again).
func (p *Provider) root(ctx context.Context, name string) (string, error) {
	cl, ok := p.class(name)
	if !ok {
		return "", fmt.Errorf("storage class %s: %w", name, kplugin.ErrUnsupported)
	}
	ctx, cancel := p.bound(ctx)
	defer cancel()
	l := p.classLock(cl.Name)
	l.Lock()
	defer l.Unlock()
	dir := filepath.Join(p.stateDir(), "mnt", cl.Name)
	p.mu.Lock()
	known := p.roots[cl.Name] == dir
	p.mu.Unlock()
	if known {
		if m, err := p.Run.Mounted(dir); err == nil && m {
			return dir, nil
		}
	}
	fs, err := fsOf(cl, p.Secrets)
	if err != nil {
		return "", err
	}
	formatted, err := p.Run.Status(ctx, fs)
	if err != nil {
		return "", timedOut(ctx, "juicefs status", err)
	}
	if !formatted {
		if err := p.Run.Format(ctx, fs); err != nil {
			return "", timedOut(ctx, "juicefs format", err)
		}
		p.logf("class %s: formatted JuiceFS file system %s (it was new)", cl.Name, fs.Name)
	}
	if m, err := p.Run.Mounted(dir); err != nil || !m {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
		o := MountOptions{CacheDir: filepath.Join(p.stateDir(), "cache", cl.Name), Log: filepath.Join(p.stateDir(), cl.Name+".log")}
		if err := p.Run.Mount(ctx, fs, dir, o); err != nil {
			return "", timedOut(ctx, "juicefs mount", err)
		}
	}
	p.mu.Lock()
	if p.roots == nil {
		p.roots = map[string]string{}
	}
	p.roots[cl.Name] = dir
	p.mu.Unlock()
	return dir, nil
}

// EnsureVolume implements kplugin.Volumes: the volume's folder, made once.
func (p *Provider) EnsureVolume(ctx context.Context, v kplugin.Volume) (kplugin.VolumeStatus, error) {
	h, err := HandleFor(v.ID)
	if err != nil {
		return kplugin.VolumeStatus{}, err
	}
	if _, ok := p.class(v.Class); ok && v.SizeBytes <= 0 {
		// Without a size there is no quota, and nothing stops one volume filling the class's
		// bucket for everyone (K-050).
		return kplugin.VolumeStatus{}, fmt.Errorf("volume %s needs a size: give it one in the silo's volumes[].size (e.g. 10Gi); a volume on class %s is held to it by a quota", h, v.Class)
	}
	root, err := p.root(ctx, v.Class)
	if err != nil {
		return kplugin.VolumeStatus{}, err
	}
	p.mu.Lock()
	_, busy := p.deleting[h]
	p.mu.Unlock()
	if busy {
		return kplugin.VolumeStatus{}, fmt.Errorf("volume %s is being deleted: %w", h, kplugin.ErrInProgress)
	}
	// Each new folder of the volume's path, and not a folder that was already there.
	// The cloud-hypervisor VMM runs as kevaso-vmm and opens disk.root/root.raw by that
	// name. The file is 0600, owned by kevaso-vmm; the folder is root's, and kevaso-vmm
	// is neither the owner nor the group, so it needs the other-execute bit to walk in
	// and nothing more (it does not list the folder, and root writes the image before
	// the VMM starts). 0711 is that. The plugin unit's UMask=0077 would turn a plain
	// mkdir of 0711 into 0700, which is the Permission denied on root.raw, so the mode
	// is set again after the folder is created.
	if err := mkdirAllMode(filepath.Join(root, filepath.FromSlash(h)), volumeDirMode); err != nil {
		return kplugin.VolumeStatus{}, err
	}
	// Global: no region. The size is a directory quota on the volume's folder, in whole GiB
	// (rounded up); asked again with a bigger size, it grows at once, on every host.
	cl, _ := p.class(v.Class)
	fs, err := fsOf(cl, p.Secrets)
	if err != nil {
		return kplugin.VolumeStatus{}, err
	}
	gib := QuotaGiB(v.SizeBytes)
	qctx, cancel := p.bound(ctx)
	defer cancel()
	if err := p.Run.SetQuota(qctx, fs, "/"+h, gib); err != nil {
		return kplugin.VolumeStatus{}, timedOut(qctx, "juicefs quota set", err)
	}
	return kplugin.VolumeStatus{Handle: h, SizeBytes: gib << 30, SizeEnforced: true}, nil
}

// QuotaGiB is a size as juicefs quota takes it: whole GiB, rounded up.
func QuotaGiB(size int64) int64 { return (size + 1<<30 - 1) >> 30 }

// AttachVolume implements kplugin.Volumes: never, for a mount class.
func (p *Provider) AttachVolume(context.Context, kplugin.Attachment) (kplugin.AttachmentStatus, error) {
	return kplugin.AttachmentStatus{}, fmt.Errorf("juicefs volumes are mounted by the node side: %w", kplugin.ErrUnsupported)
}

// DetachVolume implements kplugin.Volumes: never, for a mount class.
func (p *Provider) DetachVolume(context.Context, kplugin.Attachment) error {
	return fmt.Errorf("juicefs volumes are mounted by the node side: %w", kplugin.ErrUnsupported)
}

// DeleteVolume implements kplugin.Volumes: removes the volume's folder, in the background (a big
// one takes a while), answering ErrInProgress until it's gone.
func (p *Provider) DeleteVolume(ctx context.Context, ref kplugin.VolumeRef) error {
	if err := checkHandle(ref.Handle); err != nil {
		return err
	}
	root, err := p.root(ctx, ref.Class)
	if err != nil {
		return err
	}
	dir := filepath.Join(root, filepath.FromSlash(ref.Handle))
	p.mu.Lock()
	done, busy := p.deleting[ref.Handle]
	if !busy {
		if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
			err, had := p.delErr[ref.Handle]
			delete(p.delErr, ref.Handle)
			p.mu.Unlock()
			if had && err != nil {
				return err
			}
			return kplugin.ErrNotFound
		}
		if p.deleting == nil {
			p.deleting, p.delErr = map[string]chan struct{}{}, map[string]error{}
		}
		done = make(chan struct{})
		p.deleting[ref.Handle] = done
		go func() {
			err := os.RemoveAll(dir)
			if err == nil {
				// The silo's folder goes with its last volume (it holds others while they're there).
				_ = os.Remove(filepath.Dir(dir))
			}
			p.mu.Lock()
			delete(p.deleting, ref.Handle)
			if err != nil {
				p.delErr[ref.Handle] = err
			}
			p.mu.Unlock()
			close(done)
			p.logf("class %s: deleted %s", ref.Class, ref.Handle)
		}()
	}
	p.mu.Unlock()
	wait := p.DeleteWait
	if wait <= 0 {
		wait = 5 * time.Second
	}
	select {
	case <-done:
	case <-time.After(wait):
		return fmt.Errorf("deleting %s: %w", ref.Handle, kplugin.ErrInProgress)
	case <-ctx.Done():
		return ctx.Err()
	}
	p.mu.Lock()
	err = p.delErr[ref.Handle]
	delete(p.delErr, ref.Handle)
	p.mu.Unlock()
	return err
}

var (
	_ kplugin.Plugin  = (*Provider)(nil)
	_ kplugin.Volumes = (*Provider)(nil)
)
