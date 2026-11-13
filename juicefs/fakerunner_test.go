package juicefs

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// fakeRunner plays the juicefs command: file systems formatted or not, mounts by folder. It
// records every call, and the environment each one got, to check credentials stay out of argv.
type fakeRunner struct {
	mu        sync.Mutex
	formatted map[string]bool   // metadata URL → formatted
	mounts    map[string]string // dir → subdir
	calls     []string
	envs      [][]string
	fail      map[string]error // by call name: status, format, mount, umount
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{formatted: map[string]bool{}, mounts: map[string]string{}, fail: map[string]error{}}
}

func (f *fakeRunner) record(env []string, call string, args ...any) error {
	f.calls = append(f.calls, strings.TrimSpace(call+" "+fmt.Sprint(args...)))
	f.envs = append(f.envs, env)
	return f.fail[call]
}

func (f *fakeRunner) Status(_ context.Context, fs FS) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(fs.Env, "status", fs.Metadata); err != nil {
		return false, err
	}
	return f.formatted[fs.Metadata], nil
}

func (f *fakeRunner) Format(_ context.Context, fs FS) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(fs.Env, "format", fs.Metadata+" "+fs.Name); err != nil {
		return err
	}
	f.formatted[fs.Metadata] = true
	return nil
}

func (f *fakeRunner) Mount(_ context.Context, fs FS, dir string, o MountOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(fs.Env, "mount", dir+" "+o.Subdir); err != nil {
		return err
	}
	if !f.formatted[fs.Metadata] {
		return fmt.Errorf("juicefs mount: database is not formatted")
	}
	f.mounts[dir] = o.Subdir
	return nil
}

func (f *fakeRunner) Umount(_ context.Context, dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(nil, "umount", dir); err != nil {
		return err
	}
	delete(f.mounts, dir)
	return nil
}

func (f *fakeRunner) Mounted(dir string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.mounts[dir]
	return ok, nil
}

func (f *fakeRunner) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

var _ Runner = (*fakeRunner)(nil)
