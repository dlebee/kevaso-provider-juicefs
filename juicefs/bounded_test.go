package juicefs

import (
	"context"
	"strings"
	"testing"
	"time"

	"kevaso.com/core/sdk/kplugin"
)

// hangRunner is the fake whose juicefs status never ends for one metadata URL (a stalled
// database), until the call's deadline kills it.
type hangRunner struct {
	*fakeRunner
	hang string
}

func (h hangRunner) Status(ctx context.Context, fs FS) (bool, error) {
	if fs.Metadata == h.hang {
		<-ctx.Done()
		return false, ctx.Err()
	}
	return h.fakeRunner.Status(ctx, fs)
}

// K-041: a juicefs command that never ends is killed at the call's deadline, and the call says
// so; meanwhile another class's calls, and a delete's bookkeeping, go on (no lock is held across a
// command).
func TestAStalledCommandHoldsUpNothingElse(t *testing.T) {
	cfg := testConfig(t)
	stalled := cfg.Classes[0]
	stalled.Name, stalled.Metadata = "stalled", "postgres://juicefs@10.63.0.2:5432/juicefs"
	cfg.Classes = append(cfg.Classes, stalled)
	p := New(cfg, hangRunner{newFakeRunner(), stalled.Metadata}, testSecrets)
	p.CallTimeout = 2 * time.Second
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := p.EnsureVolume(context.Background(), kplugin.Volume{Class: "stalled", ID: "uid-1/data", Name: "a"})
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	if _, err := p.EnsureVolume(context.Background(), kplugin.Volume{Class: "juicefs", ID: "uid-2/data", Name: "b"}); err != nil {
		t.Fatalf("the other class: %v", err)
	}
	_ = p.DeleteVolume(context.Background(), kplugin.VolumeRef{Class: "juicefs", Handle: "uid-9/none"}) // takes p.mu
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the other class waited %v behind the stalled one", took)
	}
	err := <-done
	if took := time.Since(start); err == nil || !strings.Contains(err.Error(), "gave up") || took > 4*time.Second {
		t.Fatalf("the stalled call: %v after %v", err, took)
	}
	t.Logf("stalled class: %v", err)
}
