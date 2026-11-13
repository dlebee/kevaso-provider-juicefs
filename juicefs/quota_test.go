package juicefs

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"kevaso.com/core/sdk/kplugin"
)

// A volume's size is a directory quota on its folder (whole GiB, rounded up), set with the class's
// credentials in juicefs's environment; a bigger size grows it (the same handle); no size, no quota.
func TestQuota(t *testing.T) {
	ctx := context.Background()
	run := newFakeRunner()
	p := New(testConfig(t), run, testSecrets)
	v := kplugin.Volume{Class: "juicefs", ID: "u1/home", SizeBytes: 5<<30 + 1}
	st, err := p.EnsureVolume(ctx, v)
	if err != nil || !st.SizeEnforced || st.SizeBytes != 6<<30 || run.quota("/volumes/u1/home") != 6 {
		t.Fatalf("ensure: %+v %v (quota %d GiB)", st, err, run.quota("/volumes/u1/home"))
	}
	for i, c := range run.calls {
		if strings.HasPrefix(c, "quota") && !slices.Contains(run.envs[i], "META_PASSWORD=s3cr3t-meta-password") {
			t.Fatalf("%s without the metadata password", c)
		}
	}
	v.SizeBytes = 20 << 30
	if st2, err := p.EnsureVolume(ctx, v); err != nil || st2.Handle != st.Handle || st2.SizeBytes != 20<<30 || run.quota("/volumes/u1/home") != 20 {
		t.Fatalf("grown: %+v %v (quota %d GiB)", st2, err, run.quota("/volumes/u1/home"))
	}
	none, err := p.EnsureVolume(ctx, kplugin.Volume{Class: "juicefs", ID: "u2/scratch"})
	if err != nil || none.SizeEnforced || run.quota("/volumes/u2/scratch") != 0 {
		t.Fatalf("no size: %+v %v", none, err)
	}
	run.fail["quota"] = errors.New("juicefs quota set: exit 1: quota is not supported by this metadata engine")
	if _, err := p.EnsureVolume(ctx, kplugin.Volume{Class: "juicefs", ID: "u3/data", SizeBytes: 1 << 30}); err == nil {
		t.Fatal("a quota that couldn't be set: the volume answered as if its size were enforced")
	}
	if got := strings.Join(QuotaArgs(FS{Metadata: testMeta}, "/volumes/u1/home", 20), " "); got != "quota set "+testMeta+" --path /volumes/u1/home --capacity 20" {
		t.Fatalf("quota args: %s", got)
	}
	if QuotaGiB(1) != 1 || QuotaGiB(1<<30) != 1 || QuotaGiB(1<<30+1) != 2 {
		t.Fatal("QuotaGiB rounds up")
	}
}
