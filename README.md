# kevaso-provider-juicefs

The [Kevaso](https://github.com/dlebee/kevaso) provider for [JuiceFS](https://juicefs.com) (the
Apache 2.0 community edition): storage classes whose volumes **follow the silo to any host**. A
volume's data lives in an S3-compatible bucket and its metadata in a small database, so any host
running the node side can mount it, in any region, with no shared network. A container silo whose
volumes are all on such a class is movable **Anywhere**.

One program, two sides:

| Side | Runs on | Does | Holds |
|---|---|---|---|
| controller (`serve`) | the control plane | formats each class's file system once, creates and deletes volumes (folders in it) | the class's credentials |
| node (`node`) | workers | mounts a volume where kevasod asks, unmounts it when the silo leaves | the class's credentials |

Each class is reported to kevasod as topology `global`, reach `mount`
([the plugin protocol](https://github.com/dlebee/kevaso/blob/main/docs/adr/0003-provider-plugins.md)):
nothing is attached to a host. kevasod's scheduler only places a silo with a volume on the class on
hosts whose node side is running (`status.mountClasses` on the Host); the host's kevasod asks the
node side to mount the volume at `/var/lib/kevaso/volumes/SILO-UID/VOLUME` and gives that folder to
the container. **No credential reaches the silo**, only the mounted folder.

## How volumes are laid out

**One JuiceFS file system per class; one folder per Kevaso volume**, `volumes/SILO-UID/VOLUME`
(the volume's handle). The controller side keeps the class's root mounted in its state folder
(`/var/lib/kevaso-provider-juicefs/mnt/CLASS`) to create and delete those folders; the node side
mounts just the volume's folder (`juicefs mount --subdir`), so a silo never sees another's files.
One file system per class (not per volume) means one `juicefs format`, one metadata database and one
bucket prefix per class, and one cache per class on each host.

## Configuration

`--config FILE` (JSON, not secret: credentials are only named):

```json
{
  "classes": [{
    "name": "juicefs",
    "metadata": "postgres://juicefs@10.63.0.1:5432/juicefs",
    "storage": "s3",
    "bucket": "https://ACCOUNT.r2.cloudflarestorage.com/acme-kevaso-volumes",
    "credentials": { "accessKey": "r2-access-key", "secretKey": "r2-secret-key", "metaPassword": "juicefs-meta-password" }
  }]
}
```

| Field | |
|---|---|
| `name` | the storage class (silos use it as `volumes[].class`) |
| `metadata` | the metadata engine (Postgres, Redis, …), **without** a password: the config is refused if the URL holds one |
| `storage`, `bucket` | the object store's type (default `s3`) and URL |
| `fileSystem` | the JuiceFS file system's name (default `kevaso`) |
| `cacheDir`, `cacheSizeMiB` | the node side's local cache (default `/var/cache/kevaso-juicefs/CLASS`) |
| `credentials` | the names of systemd credentials (`kvoadm plugin install --credential NAME=FILE`) holding the bucket's key pair and the metadata password; each optional |
| `stateDir`, `volumesDir` | the controller side's folder, and the folder the node side mounts under (kevasod's, `/var/lib/kevaso/volumes`) |

The credentials reach the `juicefs` command through its environment (`ACCESS_KEY`, `SECRET_KEY`,
`META_PASSWORD`), never its arguments. Outside systemd, `KEVASO_JUICEFS_CREDENTIALS_DIR` names a
folder holding one file per credential.

**The metadata database matters:** every host using the class must reach it. While it's down, calls
into the volumes **wait** (no error), and go on once it's back; but the JuiceFS client gives up after
about 40 s of retries, and from then on they **fail** (an I/O error) until it's back (seen on Linodes
with JuiceFS 1.4.2 and Postgres 16: a 29 s outage was waited out, an 83 s one failed writes after
40 s). The silo isn't marked failed: its status says `VolumesResponding=False`, `NotAnswering` while
calls wait and `Failing` with the error once they fail, and `True` again when they answer.

## Sizes

A volume's `size` is a JuiceFS **directory quota** on its folder (`juicefs quota set --capacity`,
whole GiB, rounded up): writes past it fail. Growing the size in the silo's spec grows the quota at
once, on every host, with the silo running. **A volume needs a size**: one without is refused (the
silo's `status.volumes` says so, and what to give it), since without a quota nothing would stop one
volume filling the class's bucket for every other.

## Install

Both sides mount FUSE file systems as root, so they need more than a plugin's default unit. The
package says exactly what, in [`kevaso-plugin.json`](kevaso-plugin.json); `kvoadm plugin install
--manifest` shows it and writes a unit that grants that and nothing more:

| Side | Gets | Why |
|---|---|---|
| controller (`serve`) | root, `CAP_SYS_ADMIN` only, `/dev/fuse` only, its own mount namespace, writes only `/var/lib/kevaso-provider-juicefs` | it mounts each class's root privately, to make and remove volumes' folders |
| node (`node`) | root, `CAP_SYS_ADMIN` only, `/dev/fuse` only, **the host's mount namespace** | its mounts must be the host's: kevasod, the container runtimes and virtiofsd (for microVMs) use them |

The node side's unit keeps `KillMode=process`, so restarting the plugin doesn't kill the `juicefs`
daemons serving silos' volumes (it finds its mounts again in the host's mount table).

```bash
kvoadm plugin install root@cp-1 --name juicefs --binary bin/kevaso-provider-juicefs-linux-amd64 --manifest kevaso-plugin.json --yes --config juicefs.json --credential r2-access-key=r2.key --credential r2-secret-key=r2.secret --credential juicefs-meta-password=meta.pw
```

```bash
kvoadm plugin install root@hv-1 --name juicefs --side node --binary bin/kevaso-provider-juicefs-linux-amd64 --manifest kevaso-plugin.json --yes --config juicefs.json --credential r2-access-key=r2.key --credential r2-secret-key=r2.secret --credential juicefs-meta-password=meta.pw
```

## microVMs

A `cloud-hypervisor` silo gets a JuiceFS volume as a folder shared over **virtiofs**: the node side
mounts it on the host, one locked-down virtiofsd serves it to the VM, the guest mounts it by tag.
No credential enters the VM (research E029). Its own disk can live on a JuiceFS class too
(`disk.class: juicefs`): an image file on the volume, booted by one host at a time, so the VM moves
anywhere with its data.

## Build

```bash
make build
```

The `juicefs` binary (1.4 or later) must be on the hosts' PATH, or given with `--juicefs`.

## Test

```bash
make test
```

`juicefs` is faked; the plugin protocol's conformance suite (`kplugintest.Conformance`, from core)
runs against both sides.

On real machines, `test/e2e` (build tag `e2e`) runs the class end to end on Linodes: a backends
Linode (Postgres for the metadata, SeaweedFS for the bucket, firewalled to the cluster), a control
plane and two docker workers, then a silo's volume formatted, mounted, sized, moved, moved again
when its host is powered off, cut off from its database, and deleted. It costs money and needs a
Linode API token; its machines and sweeps are the Linode provider's (`linodetest`), so it builds in a
workspace with that provider next to this one (`bash core/hack/workspace.sh`).

```bash
KEVASO_LINODE_TOKEN_FILE=~/linode-token.txt go test -tags e2e -count=1 -v -timeout 100m -run TestJuiceFS ./test/e2e/
```

## Not yet

- **Checked on real machines with containers only** (`test/e2e`, Linodes): microVMs over virtiofs
  and a root disk on the class need KVM (core's `docs/notes/virtiofs-real-machine-checklist.md`).
- **Volumes shared by several silos** (`accessMode: shared`): the API has no such field yet.
- **Running the metadata database**: bring your own, or a silo in `kevaso-system`.
