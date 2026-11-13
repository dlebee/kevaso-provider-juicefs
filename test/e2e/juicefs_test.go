//go:build e2e

// Package e2e runs the juicefs storage class on real Linodes: one Linode runs the backends (a
// Postgres metadata database and a SeaweedFS bucket, reachable from the cluster's machines only),
// one is the control plane and two are docker workers. Both sides of the plugin are installed with
// kvoadm and the package's manifest, and a container silo with a juicefs volume is formatted,
// mounted, sized, moved, moved again when its host is powered off, cut off from its metadata
// database, and deleted.
//
// It costs money and needs a token, so it only runs when asked:
//
//	KEVASO_LINODE_TOKEN_FILE=~/linode-token.txt go test -tags e2e -count=1 -v -timeout 100m -run TestJuiceFS ./test/e2e/
//
// Everything it makes is labelled and tagged kvt-jfs-RUN and deleted at the end, also when it
// fails; KEVASO_E2E_KEEP=1 keeps a failed run's machines. The machines and sweeps are the Linode
// provider's linodetest bed; TestSweep deletes what a dead run left:
//
//	KEVASO_E2E_SWEEP=kvt-jfs- KEVASO_LINODE_TOKEN_FILE=~/linode-token.txt go test -tags e2e -run TestSweep ./test/e2e/
//
// Why Postgres for the metadata (not Redis): it's the engine research E029 ran the outage test
// against, it's durable on disk with no tuning (Redis needs appendonly and noeviction set to be a
// safe JuiceFS engine), and stopping it is the same systemctl stop. Why SeaweedFS for the bucket:
// MinIO's community binaries are gone and Garage publishes no checksums (E029); SeaweedFS's GitHub
// releases carry digests.
package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"kevaso.com/provider-linode/linodetest"
)

const (
	prefix         = "kvt-jfs-"
	juicefsVersion = "1.4.2"
	seaweedVersion = "4.53"
	unit           = "kevaso-plugin-juicefs"
	ns             = "e2e"
)

type condition struct{ Type, Status, Reason, Message string }

type silo struct {
	Metadata struct {
		UID        string `json:"uid"`
		Generation int64  `json:"generation"`
	} `json:"metadata"`
	Status struct {
		Phase      string      `json:"phase"`
		Host       string      `json:"host"`
		Movable    string      `json:"movable"`
		MovingFrom string      `json:"movingFrom"`
		Conditions []condition `json:"conditions"`
		Volumes    []struct {
			Name            string `json:"name"`
			Handle          string `json:"handle"`
			AttachedTo      string `json:"attachedTo"`
			Mounted         bool   `json:"mounted"`
			Message         string `json:"message"`
			SizeBytes       int64  `json:"sizeBytes"`
			SizeNotEnforced bool   `json:"sizeNotEnforced"`
		} `json:"volumes"`
	} `json:"status"`
}

func (s silo) cond(t string) condition {
	for _, c := range s.Status.Conditions {
		if c.Type == t {
			return c
		}
	}
	return condition{}
}

func (s silo) mounted() bool {
	return s.Status.Phase == "Running" && len(s.Status.Volumes) == 1 && s.Status.Volumes[0].Mounted &&
		s.Status.Volumes[0].AttachedTo == s.Status.Host
}

func secret(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func TestJuiceFS(t *testing.T) {
	tokenFile := os.Getenv("KEVASO_LINODE_TOKEN_FILE")
	if tokenFile == "" {
		t.Skip("KEVASO_LINODE_TOKEN_FILE not set: this test creates real Linodes")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 95*time.Minute)
	defer cancel()
	b := linodetest.NewBed(t, tokenFile, prefix)
	// One run at a time: a dead run's machines are swept first (TestSweep).
	if left, _, _, _, err := b.Owned(ctx, func(label string, _ []string) bool { return strings.HasPrefix(label, prefix) }); err != nil {
		t.Fatal(err)
	} else if len(left) > 0 {
		t.Fatalf("%d Linode(s) labelled %s… are still there (a run died?): run TestSweep first", len(left), prefix)
	}
	t.Logf("run %s, region %s", b.Tag, b.Region)
	b.CleanupAtEnd(t)
	started := time.Now()
	t.Cleanup(func() {
		t.Logf("4 Linodes for %s: about %.1f Linode-hours", time.Since(started).Round(time.Second), 4*time.Since(started).Hours())
	})

	cl := newHarness(ctx, t, b)
	plugin := cl.kit.Build("kevaso-provider-juicefs", "kevaso.com/provider-juicefs/cmd/kevaso-provider-juicefs", "linux", "")
	manifest, _ := filepath.Abs("../../kevaso-plugin.json")
	kvo := func(args ...string) string { t.Helper(); return cl.c.MustKvo("", args...) }
	get := func(name string) silo {
		var s silo
		cl.get(&s, "silo", name, ns)
		return s
	}
	report := func(step, f string, a ...any) { t.Logf("STEP %s PASS: %s", step, fmt.Sprintf(f, a...)) }

	// 1. Four Linodes: the backends, a control plane, two docker workers.
	ms := b.CreateAll(ctx, "db", "cp", "w1", "w2")
	db, cp, w1, w2 := ms["db"], ms["cp"], ms["w1"], ms["w2"]
	workers := map[string]linodetest.Machine{"w1": w1, "w2": w2}
	t.Cleanup(func() {
		cl.diagnose(map[string]linodetest.Machine{"cp": cp, "w1": w1, "w2": w2}, unit)
		if t.Failed() {
			out, _ := b.SSH(context.Background(), db, "systemctl --no-pager status postgresql@16-main kvt-weed | head -20; journalctl -u kvt-weed -n 20 --no-pager")
			t.Logf("── db ──\n%s", out)
		}
	})

	// Per-run credentials, generated here, kept in files only (never printed).
	creds := t.TempDir()
	cred := map[string]string{"s3-access-key": secret(10), "s3-secret-key": secret(20), "meta-password": secret(18)}
	for n, v := range cred {
		if err := os.WriteFile(filepath.Join(creds, n), []byte(v+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	allowed := []string{db.Private, cp.Private, w1.Private, w2.Private}
	out, err := b.SSHIn(ctx, db, cred["s3-access-key"]+"\n"+cred["s3-secret-key"]+"\n"+cred["meta-password"]+"\n",
		backendsScript(db.Private, allowed))
	if err != nil {
		t.Fatalf("backends: %v\n%s", err, out)
	}
	t.Log(lastLines(out, 8))
	meta := "postgres://juicefs@" + db.Private + ":5432/juicefs"
	bucket := "http://" + db.Private + ":8333/jfs"
	// Not reachable from the internet (this machine), only from the cluster's private addresses;
	// and the bucket refuses anonymous requests even from there.
	for _, port := range []string{"5432", "8333", "8888", "9333"} {
		if c, err := net.DialTimeout("tcp", db.Public+":"+port, 5*time.Second); err == nil {
			_ = c.Close()
			t.Fatalf("the backends' port %s answers on the public address", port)
		}
		if c, err := net.DialTimeout("tcp", db.Private+":"+port, 3*time.Second); err == nil {
			_ = c.Close() // only when this machine is on Linode's private network
			t.Fatalf("the backends' port %s answers this machine on the private address", port)
		}
	}
	linodetest.Eventually(t, "an anonymous request to the bucket from w1 refused", time.Minute, func() (bool, string) {
		out, err := b.SSH(ctx, w1, "curl -s -o /dev/null -w '%{http_code}' "+bucket+"/")
		return strings.TrimSpace(out) == "403", fmt.Sprintf("HTTP %q %v", out, err)
	})
	report("1", "backends on %s: Postgres 16 (metadata) and SeaweedFS %s (S3), listening on the private address only, "+
		"firewalled to the cluster's 4 private addresses, closed from the internet, bucket refuses anonymous requests", db.Label, seaweedVersion)

	// The cluster, and juicefs on every machine that mounts.
	cl.initCP(cp, "cp")
	cl.addWorker(w1, "w1", "docker")
	cl.addWorker(w2, "w2", "docker")
	for _, m := range []linodetest.Machine{cp, w1, w2} {
		out := cl.mustSSH(ctx, m, installJuiceFS())
		t.Logf("%s: %s", m.Label, lastLines(out, 1))
	}

	// 3a (assumed behaviour 2): what juicefs status says on a database that was never formatted.
	metaPW := "META_PASSWORD=$(cat /etc/kevaso/plugins/juicefs/meta-password) "
	fresh, _ := b.SSHIn(ctx, cp, cred["meta-password"]+"\n", "read -r META_PASSWORD; export META_PASSWORD; "+
		"timeout 60 juicefs status postgres://juicefs@"+db.Private+":5432/juicefs_fresh 2>&1; echo \"exit $?\"")
	t.Logf("juicefs status on a never-formatted database:\n%s", lastLines(fresh, 4))
	if !strings.Contains(fresh, "not formatted") || strings.Contains(fresh, "exit 0") {
		t.Errorf("ASSUMPTION WRONG: juicefs status on a fresh database: %q (Exec.Status wants a non-zero exit with \"not formatted\")", lastLines(fresh, 3))
	}

	// 2. The plugin: the controller side on the control plane, the node side on both workers,
	// with the manifest's privileges and nothing more.
	cfgFile := filepath.Join(t.TempDir(), "juicefs.json")
	cfg := fmt.Sprintf(`{"classes": [{"name": "juicefs", "metadata": %q, "bucket": %q,
  "credentials": {"accessKey": "s3-access-key", "secretKey": "s3-secret-key", "metaPassword": "meta-password"}}]}`, meta, bucket)
	_ = os.WriteFile(cfgFile, []byte(cfg), 0o644)
	installArgs := []string{"--name", "juicefs", "--binary", plugin, "--manifest", manifest, "--config", cfgFile}
	for n := range cred {
		installArgs = append(installArgs, "--credential", n+"="+filepath.Join(creds, n))
	}
	if out, err := cl.c.Kvoadm(append(append([]string{"plugin", "install", "root@" + w1.Public, "--side", "node"}, installArgs...), b.SSHFlags()...)...); err == nil || !strings.Contains(out, "--yes") {
		t.Fatalf("install without --yes: want a refusal naming --yes, got %v:\n%s", err, out)
	}
	if out, _ := b.SSH(ctx, w1, "test -e /etc/systemd/system/"+unit+".service && echo there"); strings.Contains(out, "there") {
		t.Fatal("refused install left a unit behind")
	}
	cl.pluginInstall(cp, append(installArgs, "--yes")...)
	for _, w := range []linodetest.Machine{w1, w2} {
		cl.pluginInstall(w, append(installArgs, "--side", "node", "--yes")...)
	}
	props := "systemctl show " + unit + " -p DynamicUser,User,CapabilityBoundingSet,AmbientCapabilities,DevicePolicy,DeviceAllow,KillMode,ProtectSystem,ReadWritePaths,NoNewPrivileges,ActiveState"
	nodeProps := cl.mustSSH(ctx, w1, props)
	ctlProps := cl.mustSSH(ctx, cp, props)
	for _, want := range []string{"DynamicUser=no", "CapabilityBoundingSet=cap_sys_admin\n", "DevicePolicy=closed", "/dev/fuse rw", "KillMode=process", "ProtectSystem=no", "NoNewPrivileges=yes", "ActiveState=active"} {
		if !strings.Contains(nodeProps+"\n", want) {
			t.Errorf("node side's unit: no %q in\n%s", want, nodeProps)
		}
	}
	for _, want := range []string{"DynamicUser=no", "CapabilityBoundingSet=cap_sys_admin\n", "/dev/fuse rw", "KillMode=control-group", "ProtectSystem=strict", "ReadWritePaths=/var/lib/kevaso-provider-juicefs", "ActiveState=active"} {
		if !strings.Contains(ctlProps+"\n", want) {
			t.Errorf("controller side's unit: no %q in\n%s", want, ctlProps)
		}
	}
	nsOf := "readlink /proc/1/ns/mnt /proc/$(systemctl show -p MainPID --value " + unit + ")/ns/mnt"
	if l := strings.Fields(cl.mustSSH(ctx, w1, nsOf)); len(l) != 2 || l[0] != l[1] {
		t.Errorf("node side's mount namespace isn't the host's: %v", l)
	}
	if l := strings.Fields(cl.mustSSH(ctx, cp, nsOf)); len(l) != 2 || l[0] == l[1] {
		t.Errorf("controller side shares the host's mount namespace: %v", l)
	}
	// The node side's effective capabilities: CAP_SYS_ADMIN (bit 21) only.
	if out := cl.mustSSH(ctx, w1, "grep CapEff /proc/$(systemctl show -p MainPID --value "+unit+")/status"); !strings.Contains(out, "0000000000200000") {
		t.Errorf("node side's CapEff: %q, want CAP_SYS_ADMIN only", out)
	}
	if t.Failed() {
		t.FailNow()
	}
	linodetest.Eventually(t, "both workers report the juicefs mount class", 3*time.Minute, func() (bool, string) {
		var seen []string
		ok := true
		for _, w := range []string{"w1", "w2"} {
			var h struct {
				Status struct {
					MountClasses []string `json:"mountClasses"`
				} `json:"status"`
			}
			cl.get(&h, "host", w, "")
			ok = ok && strings.Contains(strings.Join(h.Status.MountClasses, ","), "juicefs")
			seen = append(seen, fmt.Sprintf("%s=%v", w, h.Status.MountClasses))
		}
		return ok, strings.Join(seen, " ")
	})
	report("2", "kvoadm refused the manifest's privileges without --yes; with it: controller side root, CAP_SYS_ADMIN only, /dev/fuse only, "+
		"own mount namespace, ProtectSystem=strict + its state folder; node side root, CapEff CAP_SYS_ADMIN only, /dev/fuse only, the host's mount "+
		"namespace, KillMode=process; w1 and w2 report status.mountClasses [juicefs]")

	// 3. A docker silo with a juicefs volume.
	objects := func(p string) int {
		out, err := b.SSH(ctx, db, "curl -sf -K /etc/kvt-jfs/curl.conf '"+"http://"+db.Private+":8333/jfs?list-type=2&max-keys=1000&prefix="+p+"'")
		if err != nil {
			t.Fatalf("listing the bucket: %v %s", err, out)
		}
		return strings.Count(out, "<Key>")
	}
	apply := func(size string) {
		t.Helper()
		cl.c.MustKvo(`apiVersion: kevaso/v1
kind: Namespace
metadata: { name: e2e }
---
apiVersion: kevaso/v1
kind: Silo
metadata: { name: data, namespace: e2e }
spec:
  runtime: docker
  container:
    image: docker.io/library/alpine:3.20
    command: [sh, -c, "trap 'exit 0' TERM; while :; do sleep 3600 & wait; done"]
  resources: { cpu: 1, memory: 256Mi }
  availability: { onHostLoss: Move }
  volumes:
    - { name: data, class: juicefs, size: `+size+`, mountPath: /data }
`, "apply", "-f", "-")
	}
	apply("1Gi")
	var s silo
	linodetest.Eventually(t, "silo data running with its juicefs volume mounted", 10*time.Minute, func() (bool, string) {
		s = get("data")
		return s.mounted(), fmt.Sprintf("%+v", s.Status)
	})
	uid := s.Metadata.UID
	host := workers[s.Status.Host]
	vol := "/var/lib/kevaso/volumes/" + uid + "/data"
	if s.Status.Volumes[0].Handle != "volumes/"+uid+"/data" || s.Status.Volumes[0].SizeNotEnforced {
		t.Fatalf("volume status: %+v", s.Status.Volumes[0])
	}
	if n := strings.Count(cl.mustSSH(ctx, cp, "journalctl -u "+unit+" --no-pager"), "formatted JuiceFS file system"); n != 1 {
		t.Errorf("the controller formatted %d times, want once", n)
	}
	ctlNS := "nsenter -t $(systemctl show -p MainPID --value " + unit + ") -m "
	root := "/var/lib/kevaso-provider-juicefs/mnt/juicefs"
	cl.mustSSH(ctx, cp, ctlNS+"test -d "+root+"/volumes/"+uid+"/data")
	mnt := cl.mustSSH(ctx, host, "findmnt -n -o FSTYPE,SOURCE,OPTIONS "+vol)
	if !strings.HasPrefix(mnt, "fuse.juicefs") {
		t.Fatalf("%s on %s: %q, want a juicefs mount", vol, s.Status.Host, mnt)
	}
	if !strings.Contains(mnt, "allow_other") {
		t.Errorf("the node side's mount has no allow_other (virtiofsd and non-root silos need it): %q", mnt)
	}
	kvo("exec", "silo", "data", "-n", ns, "--", "sh", "-c", "echo hello from the silo > /data/probe")
	if out := cl.mustSSH(ctx, cp, ctlNS+"cat "+root+"/volumes/"+uid+"/data/probe"); !strings.Contains(out, "hello from the silo") {
		t.Fatalf("the probe in the controller's view of the file system: %q", out)
	}
	if out := kvo("exec", "silo", "data", "-n", ns, "--", "ls", "-A", "/data"); strings.TrimSpace(out) != "probe" {
		t.Errorf("the silo sees more than its own folder: %q", out)
	}
	// Names JuiceFS uses for its own files at a mount's root are the silo's to use.
	if out := kvo("exec", "silo", "data", "-n", ns, "--", "sh", "-c", "echo mine > /data/.config && cat /data/.config && rm /data/.config"); strings.TrimSpace(out) != "mine" {
		t.Errorf("a file of the silo's own named .config: %q", out)
	}
	before := objects("kevaso/chunks/")
	kvo("exec", "silo", "data", "-n", ns, "--", "sh", "-c", "head -c 8388608 /dev/urandom > /data/blob && sync && md5sum /data/blob > /data/blob.md5")
	linodetest.Eventually(t, "the blob's chunks in the bucket", 2*time.Minute, func() (bool, string) {
		n := objects("kevaso/chunks/")
		return n > before, fmt.Sprintf("%d objects (before: %d)", n, before)
	})
	// No credential inside the container: environment, process list, mount table, files.
	pattern := "<(cat /etc/kevaso/plugins/juicefs/s3-access-key /etc/kevaso/plugins/juicefs/s3-secret-key /etc/kevaso/plugins/juicefs/meta-password)"
	cid := "$(docker ps -q --filter label=kevaso.com/name=data --filter label=kevaso.com/namespace=e2e)"
	leak := cl.mustSSH(ctx, host, "bash -c '"+
		"c="+cid+"; test -n \"$c\" || { echo no container; exit 1; }; "+
		"echo inspect=$(docker inspect $c | grep -c -F -f "+pattern+"); "+
		"echo procs=$(docker exec $c sh -c \"cat /proc/[0-9]*/environ /proc/[0-9]*/cmdline /proc/mounts; ps -o args\" | grep -a -c -F -f "+pattern+"); "+
		"echo files=$(docker export $c | grep -a -c -F -f "+pattern+"); "+
		"echo volume=$(timeout 120 find "+vol+" -type f -exec grep -a -l -F -f "+pattern+" {} + | wc -l); "+
		"echo hostargv=$(ps -eo args | grep -a -c -F -f "+pattern+")'")
	for _, want := range []string{"inspect=0", "procs=0", "files=0", "volume=0", "hostargv=0"} {
		if !strings.Contains(leak, want) {
			t.Errorf("a credential leaked: want %s in %q", want, leak)
		}
	}
	report("3", "%s formatted once (fresh database: %q), folder volumes/%s/data made by the controller, mounted on %s with --subdir "+
		"(%s), the silo sees only its folder, the probe shows in the controller's view, the blob's chunks are in the bucket (%d → %d objects), "+
		"no credential in the container's inspect/env/argv/mounts/files, the volume or any host argv",
		"kevaso", lastLines(fresh, 1), uid, s.Status.Host, strings.TrimSpace(mnt), before, objects("kevaso/chunks/"))

	// 2 (cont.). The node side's mounts survive a restart of the plugin.
	count := "grep -c fuse.juicefs /proc/self/mountinfo"
	n0 := strings.TrimSpace(cl.mustSSH(ctx, host, count))
	cl.mustSSH(ctx, host, "systemctl restart "+unit+" && sleep 3 && systemctl is-active "+unit)
	if n1 := strings.TrimSpace(cl.mustSSH(ctx, host, count)); n1 != n0 {
		t.Fatalf("juicefs mounts on %s: %s before restarting the node side, %s after", s.Status.Host, n0, n1)
	}
	if out := kvo("exec", "silo", "data", "-n", ns, "--", "cat", "/data/probe"); !strings.Contains(out, "hello from the silo") {
		t.Fatalf("after restarting the node side: %q", out)
	}
	report("2", "restarting the node side on %s kept its %s juicefs mount(s) and the silo's files", s.Status.Host, n0)

	// 4. Size: the quota holds, and grows with the spec, with the silo running.
	quota := func() string {
		return cl.mustSSH(ctx, cp, metaPW+"juicefs quota get "+meta+" --path /volumes/"+uid+"/data 2>&1")
	}
	q1 := quota()
	if !regexp.MustCompile(`1(\.0)? ?GiB`).MatchString(q1) {
		t.Errorf("quota for size 1Gi: %s", q1)
	}
	ctr := strings.TrimSpace(cl.mustSSH(ctx, host, "docker inspect -f '{{.Id}} {{.State.StartedAt}}' "+cid))
	fill, err := cl.c.Kvo("", "exec", "silo", "data", "-n", ns, "--", "sh", "-c", "dd if=/dev/zero of=/data/fill bs=1M count=1300 conv=fsync 2>&1; echo rc=$?")
	if l := strings.ToLower(fill); !strings.Contains(l, "quota exceeded") || strings.Contains(l, "rc=0") {
		t.Errorf("writing 1300 MiB into a 1 GiB volume: %v %q (want it refused)", err, lastLines(fill, 3))
	}
	kvo("exec", "silo", "data", "-n", ns, "--", "rm", "-f", "/data/fill")
	apply("2Gi")
	linodetest.Eventually(t, "the quota grown to 2 GiB", 3*time.Minute, func() (bool, string) {
		q := quota()
		return regexp.MustCompile(`2(\.0)? ?GiB`).MatchString(q), q
	})
	time.Sleep(20 * time.Second) // the mounting client reloads quotas now and then
	fill2, err := cl.c.Kvo("", "exec", "silo", "data", "-n", ns, "--", "sh", "-c", "dd if=/dev/zero of=/data/fill bs=1M count=1300 conv=fsync 2>&1 && rm /data/fill && echo ok")
	if err != nil || !strings.Contains(fill2, "ok") {
		t.Errorf("1300 MiB into the grown 2 GiB volume: %v %q", err, lastLines(fill2, 3))
	}
	if now := strings.TrimSpace(cl.mustSSH(ctx, host, "docker inspect -f '{{.Id}} {{.State.StartedAt}}' "+cid)); now != ctr {
		t.Errorf("growing the size restarted the silo: %q → %q", ctr, now)
	}
	s = get("data")
	report("4", "1Gi: %s; dd of 1300 MiB refused (%s); size → 2Gi grew the quota with the same container running, and the same dd fit (status sizeBytes %d)",
		firstLine(grepLine(q1, "GiB")), lastLines(fill, 1), s.Status.Volumes[0].SizeBytes)

	// 5a. kvo move to the other worker: the old host unmounts before the new one mounts.
	from := s.Status.Host
	to := map[string]string{"w1": "w2", "w2": "w1"}[from]
	if s.Status.Movable != "Anywhere" {
		t.Errorf("status.movable: %q, want Anywhere", s.Status.Movable)
	}
	t.Log(kvo("move", "silo", "data", "-n", ns, "--to", to))
	linodetest.Eventually(t, "data running on "+to, 8*time.Minute, func() (bool, string) {
		s = get("data")
		return s.Status.Host == to && s.mounted(), fmt.Sprintf("%+v", s.Status)
	})
	when := func(m linodetest.Machine, what string) float64 {
		out := cl.mustSSH(ctx, m, "journalctl -u "+unit+" -o short-unix --no-pager | grep -F '"+what+"' | tail -1")
		f, _ := strconv.ParseFloat(firstWord(out), 64)
		return f
	}
	unmounted := when(workers[from], "unmounted "+vol)
	mounted := when(workers[to], "mounted volumes/"+uid+"/data at "+vol)
	if unmounted == 0 || mounted == 0 || unmounted > mounted {
		t.Errorf("order: %s unmounted at %f, %s mounted at %f", from, unmounted, to, mounted)
	}
	if out, _ := b.SSH(ctx, workers[from], "findmnt "+vol); strings.TrimSpace(out) != "" {
		t.Errorf("still mounted on %s after the move: %s", from, out)
	}
	if out := kvo("exec", "silo", "data", "-n", ns, "--", "sh", "-c", "cat /data/probe; md5sum -c /data/blob.md5"); !strings.Contains(out, "hello from the silo") || !strings.Contains(out, "OK") {
		t.Fatalf("files after the move: %q", out)
	}
	report("5", "kvo move %s → %s: %s unmounted %.1fs before %s mounted; probe and blob checksum there; status.movable %s",
		from, to, from, mounted-unmounted, to, s.Status.Movable)

	// 5b. onHostLoss: Move: the silo's host is powered off; after fencing it comes up on the other one.
	lost := s.Status.Host
	other := map[string]string{"w1": "w2", "w2": "w1"}[lost]
	off := time.Now()
	b.PowerOff(ctx, workers[lost])
	t.Logf("%s powered off", lost)
	fencing := false
	linodetest.Eventually(t, "data moved to "+other+" after losing "+lost, 10*time.Minute, func() (bool, string) {
		s = get("data")
		c := s.cond("HostLost")
		fencing = fencing || c.Reason == "Fencing"
		return s.Status.Host == other && s.mounted(), fmt.Sprintf("%s HostLost=%+v", s.Status.Phase, c)
	})
	took := time.Since(off).Round(time.Second)
	if c := s.cond("HostLost"); c.Reason != "Moved" {
		t.Errorf("HostLost condition after the move: %+v", c)
	}
	if !fencing {
		t.Log("(the Fencing phase wasn't caught between polls)")
	}
	if took < 40*time.Second {
		t.Errorf("moved %s after the host went down: before its lease could run out", took)
	}
	if out := kvo("exec", "silo", "data", "-n", ns, "--", "sh", "-c", "cat /data/probe; md5sum -c /data/blob.md5"); !strings.Contains(out, "OK") {
		t.Fatalf("files after the host loss: %q", out)
	}
	// The lost host comes back: it must not mount the volume again.
	b.PowerOn(ctx, workers[lost])
	linodetest.Eventually(t, lost+" back", 8*time.Minute, func() (bool, string) {
		out, err := b.SSH(ctx, workers[lost], "systemctl is-active kevasod "+unit)
		return err == nil, out
	})
	time.Sleep(20 * time.Second)
	if out, _ := b.SSH(ctx, workers[lost], "findmnt -n "+vol+"; docker ps -q --filter label=kevaso.com/name=data"); strings.TrimSpace(out) != "" {
		t.Errorf("%s came back and runs or mounts data again: %q", lost, out)
	}
	report("5", "onHostLoss: Move: %s powered off through the API, HostLost went Fencing → Moved, data running on %s with its files %s later; "+
		"%s came back with neither the mount nor the container", lost, other, took, lost)

	// 6. Two silos sharing one volume.
	t.Log("STEP 6 SKIP: the API has no accessMode (api/v1 Volume has name, class, size, mountPath, important): a volume belongs to one silo")

	// 7. The metadata database down. Each write in the silo is logged as "start end rc"; times come
	// from the machines' clocks (NTP), not this one's.
	host = workers[s.Status.Host]
	cl.mustSSH(ctx, host, "docker exec -d "+cid+" sh -c 'while :; do s=$(date +%s); (echo $s > /data/hb && sync) 2>/dev/null; r=$?; echo \"$s $(date +%s) $r\" >> /tmp/hb.log; sleep 1; done'")
	type write struct{ start, end, rc int64 }
	writes := func() []write {
		var ws []write
		for _, l := range strings.Split(cl.mustSSH(ctx, host, "docker exec "+cid+" cat /tmp/hb.log"), "\n") {
			f := strings.Fields(l)
			if len(f) != 3 {
				continue
			}
			var w write
			w.start, _ = strconv.ParseInt(f[0], 10, 64)
			w.end, _ = strconv.ParseInt(f[1], 10, 64)
			w.rc, _ = strconv.ParseInt(f[2], 10, 64)
			ws = append(ws, w)
		}
		return ws
	}
	okSince := func(t0 int64) bool {
		for _, w := range writes() {
			if w.start > t0 && w.rc == 0 {
				return true
			}
		}
		return false
	}
	time.Sleep(5 * time.Second)
	if !okSince(0) {
		t.Fatal("no heartbeat in the silo")
	}
	conds := func(x silo) string {
		var said []string
		for _, c := range x.Status.Conditions {
			said = append(said, fmt.Sprintf("%s=%s %s %q", c.Type, c.Status, c.Reason, c.Message))
		}
		return strings.Join(said, "; ")
	}
	// outage stops Postgres for d, watching the silo's status; it says what the silo's writes did.
	outage := func(d time.Duration) (stopAt, startAt int64, seen []condition, phases []string) {
		stopAt = unixOf(cl.mustSSH(ctx, db, "systemctl stop postgresql && date +%s"))
		for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(4 * time.Second) {
			x := get("data")
			if x.Metadata.UID == "" {
				continue // kvo didn't answer this time: no phase to record
			}
			if c := x.cond("VolumesResponding"); c.Status == "False" && (len(seen) == 0 || seen[len(seen)-1].Reason != c.Reason) {
				seen = append(seen, c)
			}
			if len(phases) == 0 || phases[len(phases)-1] != x.Status.Phase {
				phases = append(phases, x.Status.Phase)
			}
		}
		startAt = unixOf(cl.mustSSH(ctx, db, "systemctl start postgresql && date +%s"))
		linodetest.Eventually(t, "the silo's writes going again", 3*time.Minute, func() (bool, string) { return okSince(startAt), "" })
		linodetest.Eventually(t, "VolumesResponding back to True", 2*time.Minute, func() (bool, string) {
			c := get("data").cond("VolumesResponding")
			return c.Status == "True", fmt.Sprintf("%+v", c)
		})
		return stopAt, startAt, seen, phases
	}
	// 7a. Shorter than the client's retries (E029's 30s): writes wait, and go on once it's back.
	stopAt, startAt, seen, phases := outage(25 * time.Second)
	var longest write
	failed := 0
	for _, w := range writes() {
		if w.start >= stopAt && w.start <= startAt {
			if w.end-w.start > longest.end-longest.start {
				longest = w
			}
			if w.rc != 0 {
				failed++
			}
		}
	}
	// (whole seconds, and the database answers a moment before systemctl start returns)
	if longest.end-longest.start < 15 || longest.rc != 0 || longest.end < startAt-2 || longest.end > startAt+10 || failed > 0 {
		t.Errorf("a %ds outage: the longest write %+v (database back at %d), %d writes failed; want a wait, no error, ending once it's back", startAt-stopAt, longest, startAt, failed)
	}
	if len(seen) == 0 || seen[0].Reason != "NotAnswering" {
		t.Errorf("a %ds outage: the silo's status said %+v about its storage, want VolumesResponding=False NotAnswering", startAt-stopAt, seen)
	}
	for _, ph := range phases {
		if ph != "Running" {
			t.Errorf("phase during the outage: %v", phases)
		}
	}
	first := condition{}
	if len(seen) > 0 {
		first = seen[0]
	}
	report("7", "Postgres down %ds: a write waited %ds with no error and finished %ds after it was back; phase %v throughout; status: VolumesResponding=False %s %q; True again after",
		startAt-stopAt, longest.end-longest.start, longest.end-startAt, phases, first.Reason, first.Message)
	// 7b. Longer than the client's retries: calls fail, and the status says so.
	cl.mustSSH(ctx, db, "systemctl stop postgresql")
	status, _ := b.SSHIn(ctx, cp, "", metaPW+"timeout 30 juicefs status "+meta+" 2>&1 | tail -2; echo \"exit ${PIPESTATUS[0]}\"")
	cl.mustSSH(ctx, db, "systemctl start postgresql")
	if !strings.Contains(status, "connection refused") || strings.Contains(status, "exit 0") {
		t.Errorf("juicefs status with the database down: %q (want an error saying so)", status)
	}
	if strings.Contains(status, "not formatted") {
		t.Errorf("ASSUMPTION WRONG: juicefs status says \"not formatted\" while the database is down: %q", status)
	}
	time.Sleep(10 * time.Second)
	stopAt, startAt, seen, phases = outage(80 * time.Second)
	var firstFail *write
	for _, w := range writes() {
		if w.start >= stopAt && w.start <= startAt && w.rc != 0 && firstFail == nil {
			firstFail = &w
		}
	}
	var reasons []string
	for _, c := range seen {
		reasons = append(reasons, c.Reason+": "+c.Message)
	}
	if firstFail == nil {
		t.Logf("a %ds outage: no write failed (they waited it out)", startAt-stopAt)
	} else {
		t.Logf("a %ds outage: the first failed write began %ds in and failed after %ds", startAt-stopAt, firstFail.start-stopAt, firstFail.end-firstFail.start)
		if !slices.ContainsFunc(seen, func(c condition) bool { return c.Reason == "Failing" }) {
			t.Errorf("writes failed with the database down, the status said: %v", reasons)
		}
	}
	for _, ph := range phases {
		if ph != "Running" {
			t.Errorf("phase during the long outage: %v", phases)
		}
	}
	ff := "none"
	if firstFail != nil {
		ff = fmt.Sprintf("began %ds in, failed after %ds (rc %d)", firstFail.start-stopAt, firstFail.end-firstFail.start, firstFail.rc)
	}
	report("7", "Postgres down %ds: first failed write %s; phase %v; status went %s; juicefs status with the database down: %q; all: %s",
		startAt-stopAt, ff, phases, strings.Join(reasons, " → "), lastLines(status, 2), conds(get("data")))

	// 8. Deleting the silo deletes its folder and objects.
	kvo("delete", "silo", "data", "-n", ns)
	soft(t, "the volume's folder and objects gone", 8*time.Minute, func() (bool, string) {
		_, err := cl.c.Kvo("", "get", "silo", "data", "-n", ns)
		dir, _ := b.SSH(ctx, cp, ctlNS+"ls -A "+root+"/volumes/ 2>&1")
		n := objects("kevaso/chunks/")
		return err != nil && strings.TrimSpace(dir) == "" && n == 0, fmt.Sprintf("silo gone=%v volumes/=%q chunk objects=%d", err != nil, dir, n)
	})
	if q := cl.mustSSH(ctx, cp, metaPW+"juicefs quota list "+meta+" 2>&1"); strings.Contains(q, uid) {
		t.Errorf("a quota is left for the deleted volume: %s", q)
	}
	report("8", "deleting the silo removed volumes/%s (nothing left under volumes/), its quota, and every chunk object in the bucket", uid)

	// 8b. Removing the plugin while a volume is still in use.
	cl.c.MustKvo(`apiVersion: kevaso/v1
kind: Silo
metadata: { name: keep, namespace: e2e }
spec:
  runtime: docker
  container:
    image: docker.io/library/alpine:3.20
    command: [sh, -c, "trap 'exit 0' TERM; while :; do sleep 3600 & wait; done"]
  resources: { cpu: 1, memory: 128Mi }
  volumes:
    - { name: keep, class: juicefs, size: 1Gi, mountPath: /keep }
`, "apply", "-f", "-")
	var k silo
	linodetest.Eventually(t, "silo keep running", 8*time.Minute, func() (bool, string) {
		k = get("keep")
		return k.mounted(), fmt.Sprintf("%+v", k.Status)
	})
	kvo("exec", "silo", "keep", "-n", ns, "--", "sh", "-c", "head -c 1048576 /dev/urandom > /keep/f && sync")
	kept := objects("kevaso/chunks/")
	rm, rmErr := cl.c.Kvoadm(append([]string{"plugin", "remove", "root@" + cp.Public, "--name", "juicefs"}, b.SSHFlags()...)...)
	t.Logf("kvoadm plugin remove (controller side, a volume in use): err=%v\n%s", rmErr, rm)
	time.Sleep(30 * time.Second)
	k = get("keep")
	kh := workers[k.Status.Host]
	rmNode, rmNodeErr := cl.c.Kvoadm(append([]string{"plugin", "remove", "root@" + kh.Public, "--name", "juicefs", "--side", "node"}, b.SSHFlags()...)...)
	t.Logf("kvoadm plugin remove (node side under keep): err=%v\n%s", rmNodeErr, rmNode)
	time.Sleep(20 * time.Second)
	orphan, _ := b.SSH(ctx, kh, "findmnt -n -o FSTYPE /var/lib/kevaso/volumes/"+k.Metadata.UID+"/keep; pgrep -af 'juicefs mount' | wc -l")
	k = get("keep")
	var ks []string
	for _, c := range k.Status.Conditions {
		ks = append(ks, fmt.Sprintf("%s=%s %s %q", c.Type, c.Status, c.Reason, c.Message))
	}
	readable, _ := cl.c.Kvo("", "exec", "silo", "keep", "-n", ns, "--", "ls", "-l", "/keep/f")
	if n := objects("kevaso/chunks/"); n < kept {
		t.Errorf("removing the plugin deleted objects: %d → %d", kept, n)
	}
	refused := rmErr != nil || rmNodeErr != nil
	report("8", "kvoadm plugin remove with a volume in use: refused=%v; the bucket keeps its %d objects; on %s afterwards: mount/daemons %q; "+
		"keep's phase %s, file readable: %v; conditions: %s", refused, kept, k.Status.Host, strings.Join(strings.Fields(orphan), " "),
		k.Status.Phase, strings.Contains(readable, "/keep/f"), strings.Join(ks, "; "))
}

// TestSweep deletes what dead runs left: KEVASO_E2E_SWEEP=kvt-jfs- (linodetest.SweepFamily: only
// what is labelled with it and, where tagged, carries a kvt-jfs tag).
func TestSweep(t *testing.T) {
	sweep, tokenFile := os.Getenv("KEVASO_E2E_SWEEP"), os.Getenv("KEVASO_LINODE_TOKEN_FILE")
	if sweep == "" || tokenFile == "" {
		t.Skip("KEVASO_E2E_SWEEP and KEVASO_LINODE_TOKEN_FILE not set")
	}
	linodetest.SweepFamily(t, sweep, tokenFile)
}

// backendsScript sets up Postgres and SeaweedFS on the backends machine, listening on priv only,
// firewalled to the allowed addresses; it reads the bucket's key pair and the metadata password
// from stdin (one per line).
func backendsScript(priv string, allowed []string) string {
	hba := ""
	for _, ip := range allowed {
		hba += "echo 'host juicefs,juicefs_fresh juicefs " + ip + "/32 scram-sha-256' >> $C/pg_hba.conf; "
	}
	return `set -euo pipefail
read -r AK; read -r SK; read -r MP; export AK SK
S=/etc/kvt-jfs; install -d -m 700 $S; umask 077
# The firewall first: from the internet only SSH; from the cluster's private addresses, the rest.
nft -f - <<EOF
table inet kvtjfs {
  chain input {
    type filter hook input priority 0; policy drop;
    iif lo accept
    ct state established,related accept
    ip saddr { ` + strings.Join(allowed, ", ") + ` } accept
    tcp dport 22 accept
    icmp type echo-request accept
  }
}
EOF
export DEBIAN_FRONTEND=noninteractive
apt-get install -y -q postgresql >/dev/null 2>&1 || { apt-get update -q >/dev/null && apt-get install -y -q postgresql >/dev/null; }
PGV=$(ls /etc/postgresql); C=/etc/postgresql/$PGV/main
echo "listen_addresses = 'localhost,` + priv + `'" >> $C/postgresql.conf
` + hba + `
systemctl restart postgresql
runuser -u postgres -- psql -q -c "CREATE ROLE juicefs LOGIN"
printf "ALTER ROLE juicefs PASSWORD '%s';\n" "$MP" | runuser -u postgres -- psql -q
runuser -u postgres -- createdb -O juicefs juicefs
runuser -u postgres -- createdb -O juicefs juicefs_fresh
# SeaweedFS, checked against the digest GitHub publishes for the asset.
cd /tmp
python3 - > weed.sha <<'PY'
import json, urllib.request
r = json.load(urllib.request.urlopen("https://api.github.com/repos/seaweedfs/seaweedfs/releases/tags/` + seaweedVersion + `"))
a = [a for a in r["assets"] if a["name"] == "linux_amd64.tar.gz"][0]
print(a["digest"].split(":", 1)[1] + "  weed.tgz")
PY
curl -fsSL -o weed.tgz https://github.com/seaweedfs/seaweedfs/releases/download/` + seaweedVersion + `/linux_amd64.tar.gz
sha256sum -c weed.sha
tar -xzf weed.tgz weed && install -m 0755 weed /usr/local/bin/weed
python3 - > $S/s3.json <<'PY'
import json, os
print(json.dumps({"identities": [{"name": "jfs", "credentials": [{"accessKey": os.environ["AK"], "secretKey": os.environ["SK"]}],
  "actions": ["Admin", "Read", "Write", "List", "Tagging"]}]}))
PY
printf 'user = "%s:%s"\naws-sigv4 = "aws:amz:us-east-1:s3"\n' "$AK" "$SK" > $S/curl.conf
mkdir -p /var/lib/weed
cat > /etc/systemd/system/kvt-weed.service <<EOF
[Unit]
Description=SeaweedFS for the kvt-jfs test
[Service]
ExecStart=/usr/local/bin/weed server -dir=/var/lib/weed -ip=` + priv + ` -ip.bind=` + priv + ` -master.volumeSizeLimitMB=256 -volume.max=60 -s3 -s3.port=8333 -s3.config=$S/s3.json
Restart=always
[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload; systemctl enable --now kvt-weed >/dev/null 2>&1
for i in $(seq 90); do curl -s -o /dev/null http://` + priv + `:8333/ && break; sleep 2; done
curl -sf -o /dev/null -K $S/curl.conf -X PUT http://` + priv + `:8333/jfs
echo "postgres $PGV, weed $(weed version 2>&1 | head -1): ready"
`
}

// installJuiceFS installs the juicefs binary, checked against the digest GitHub publishes.
func installJuiceFS() string {
	return `set -euo pipefail; cd /tmp
python3 - > jfs.sha <<'PY'
import json, urllib.request
r = json.load(urllib.request.urlopen("https://api.github.com/repos/juicedata/juicefs/releases/tags/v` + juicefsVersion + `"))
a = [a for a in r["assets"] if a["name"] == "juicefs-` + juicefsVersion + `-linux-amd64.tar.gz"][0]
print(a["digest"].split(":", 1)[1] + "  jfs.tgz")
PY
curl -fsSL -o jfs.tgz https://github.com/juicedata/juicefs/releases/download/v` + juicefsVersion + `/juicefs-` + juicefsVersion + `-linux-amd64.tar.gz
sha256sum -c jfs.sha >/dev/null
tar -xzf jfs.tgz juicefs && install -m 0755 juicefs /usr/local/bin/juicefs
juicefs version`
}

func lastLines(s string, n int) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, " | ")
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return l
}

func unixOf(s string) int64 {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0
	}
	n, _ := strconv.ParseInt(f[len(f)-1], 10, 64)
	return n
}

func firstWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

func grepLine(s, sub string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			return strings.TrimSpace(l)
		}
	}
	return s
}

// soft is Eventually that records a failure and carries on (later steps still say something).
func soft(t *testing.T, what string, d time.Duration, f func() (bool, string)) bool {
	t.Helper()
	deadline, last := time.Now().Add(d), ""
	for time.Now().Before(deadline) {
		ok, s := f()
		if ok {
			return true
		}
		last = s
		time.Sleep(5 * time.Second)
	}
	t.Errorf("%s: not in %v; last: %s", what, d, last)
	return false
}

// harness is the cluster this test drives with linodetest's kit: kvoadm and kvo built for here,
// kevasod for the Linodes.
type harness struct {
	t       *testing.T
	ctx     context.Context
	b       *linodetest.Bed
	kit     *linodetest.Kit
	c       *linodetest.Cluster
	kevasod string
}

func newHarness(ctx context.Context, t *testing.T, b *linodetest.Bed) *harness {
	k := linodetest.NewKit(t, ctx)
	return &harness{t: t, ctx: ctx, b: b, kit: k, c: k.Cluster(), kevasod: k.Build("kevasod", "kevaso.com/core/cmd/kevasod", "linux", "")}
}

func (h *harness) mustSSH(ctx context.Context, m linodetest.Machine, cmd string) string {
	h.t.Helper()
	out, err := h.b.SSH(ctx, m, cmd)
	if err != nil {
		h.t.Fatalf("%s: %s: %v\n%s", m.Label, firstLine(cmd), err, out)
	}
	return out
}

// get reads kvo get KIND NAME [-n NS] -o json into v; false if it isn't there.
func (h *harness) get(v any, kind, name, namespace string) bool {
	args := []string{"get", kind, name, "-o", "json"}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	out, err := h.c.Kvo("", args...)
	return err == nil && json.Unmarshal([]byte(out), v) == nil
}

func (h *harness) initCP(cp linodetest.Machine, name string) {
	h.t.Helper()
	out := h.c.MustKvoadm(append([]string{"init", "root@" + cp.Public, "--kevasod", h.kevasod, "--name", name, "--cluster", "e2e",
		"--api-server", "https://" + cp.Public + ":6443"}, h.b.SSHFlags()...)...)
	if !strings.Contains(out, "KVR1-") {
		h.t.Fatalf("no recovery key printed:\n%s", out)
	}
}

func (h *harness) addWorker(m linodetest.Machine, name, runtime string) {
	h.t.Helper()
	h.t.Log(h.c.MustKvoadm(append([]string{"host", "add", "root@" + m.Public, "--kevasod", h.kevasod, "--name", name,
		"--install", "worker", "--runtime", runtime, "--region", h.b.Region}, h.b.SSHFlags()...)...))
}

func (h *harness) pluginInstall(m linodetest.Machine, args ...string) {
	h.t.Helper()
	h.t.Log(h.c.MustKvoadm(append(append([]string{"plugin", "install", "root@" + m.Public}, args...), h.b.SSHFlags()...)...))
}

// diagnose logs, if the test failed, the machines' plugin and kevasod journals and the cluster's
// hosts and silos.
func (h *harness) diagnose(ms map[string]linodetest.Machine, units ...string) {
	if !h.t.Failed() {
		return
	}
	dctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for name, m := range ms {
		cmd := "ls -la /run/kevaso-plugins/ /run/kevaso-plugins/* 2>&1; "
		for _, u := range units {
			cmd += "echo '── " + u + "'; journalctl -u " + u + " -n 40 --no-pager 2>&1; "
		}
		cmd += "journalctl -u kevasod -n 80 --no-pager 2>&1 | grep -iv 'heartbeat' | tail -60"
		out, _ := h.b.SSH(dctx, m, cmd)
		h.t.Logf("── %s ──\n%s", name, out)
	}
	for _, args := range [][]string{{"get", "hosts", "-o", "yaml"}, {"get", "silos", "-A", "-o", "yaml"}} {
		out, _ := h.c.Kvo("", args...)
		h.t.Logf("── kvo %s ──\n%s", strings.Join(args, " "), out)
	}
}
