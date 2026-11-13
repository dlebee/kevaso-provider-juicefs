package juicefs

import (
	"os"
	"slices"
	"testing"

	"kevaso.com/core/sdk/kplugin"
)

// The package's manifest (what kvoadm plugin install grants) is valid and asks for what each side
// does: both mount FUSE as root; only the node side's mounts are the host's (kevasod, the container
// runtimes and virtiofsd use them); the controller side keeps its own namespace and writes only its
// state folder, where it mounts each class's root.
func TestManifest(t *testing.T) {
	b, err := os.ReadFile("../" + kplugin.ManifestFile)
	if err != nil {
		t.Fatal(err)
	}
	m, err := kplugin.ParseManifest(b)
	if err != nil || m.Name != Name {
		t.Fatalf("%+v %v", m, err)
	}
	for side, n := range map[string]kplugin.Needs{"controller": m.Controller, "node": m.Node} {
		if !n.Root || !n.Mounts() || !slices.Equal(n.Devices, []string{"/dev/fuse"}) || len(n.Capabilities) != 1 {
			t.Errorf("%s side: %+v", side, n)
		}
	}
	if !m.Node.HostMounts || m.Controller.HostMounts {
		t.Errorf("host mounts: node %v, controller %v", m.Node.HostMounts, m.Controller.HostMounts)
	}
	if !slices.Equal(m.Controller.WritePaths, []string{DefaultStateDir}) {
		t.Errorf("controller writes %v", m.Controller.WritePaths)
	}
}
