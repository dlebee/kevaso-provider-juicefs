package juicefs

import (
	"context"
	"strings"
	"testing"

	"kevaso.com/core/sdk/kplugin"
)

// K-050: a volume without a size had no quota, so a namespace user could fill the class's shared
// bucket. Now a volume needs a size: one without is refused, saying what to give it, and nothing is
// made for it (no folder, no quota, not even the class's root mounted).
func TestAVolumeNeedsASize(t *testing.T) {
	run := newFakeRunner()
	p := New(testConfig(t), run, testSecrets)
	_, err := p.EnsureVolume(context.Background(), kplugin.Volume{Class: "juicefs", ID: "u2/scratch"})
	if err == nil || !strings.Contains(err.Error(), "needs a size") || !strings.Contains(err.Error(), "volumes[].size") {
		t.Fatalf("a volume without a size: %v", err)
	}
	if len(run.calls) != 0 {
		t.Fatalf("something was done for it: %v", run.calls)
	}
	if st, err := p.EnsureVolume(context.Background(), kplugin.Volume{Class: "juicefs", ID: "u2/scratch", SizeBytes: 1 << 30}); err != nil || !st.SizeEnforced {
		t.Fatalf("with a size: %+v %v", st, err)
	}
}
