package juicefs

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"
)

// The node side unmounts with umount(2), never through the juicefs command (juicefs umount runs
// fusermount, which Ubuntu's AppArmor profile refuses under /var/lib/kevaso).
func TestUmountIsUmount2NotFusermount(t *testing.T) {
	const dir = "/var/lib/kevaso/volumes/u1/data"
	var got []string
	e := Exec{
		Command: func(_ context.Context, name string, args, _ []string) ([]byte, int, error) {
			t.Errorf("Umount ran a command: %s %s", name, strings.Join(args, " "))
			return nil, 1, nil
		},
		Unmount: func(d string) error { got = append(got, d); return nil },
	}
	if err := e.Umount(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != dir {
		t.Fatalf("unmounted %v, want exactly %s", got, dir)
	}

	// A busy mount is an error (tried again later), and it names the mount point and the cause.
	e.Unmount = func(string) error { return syscall.EBUSY }
	err := e.Umount(context.Background(), dir)
	if !errors.Is(err, syscall.EBUSY) || !strings.Contains(err.Error(), dir) {
		t.Fatalf("a busy mount: %v", err)
	}

	// Past its deadline, nothing is unmounted.
	got = nil
	e.Unmount = func(d string) error { got = append(got, d); return nil }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.Umount(ctx, dir); err == nil || len(got) != 0 {
		t.Fatalf("a cancelled unmount: %v, unmounted %v", err, got)
	}
}
