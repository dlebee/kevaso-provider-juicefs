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

## Who can reach what

**Root on any worker running the node side can read, change and delete every volume of the class,
in the whole cluster**, not only the volumes of the silos on that worker. The node side holds the
class's own credentials (the bucket's key pair and the metadata password), which open the whole
JuiceFS file system; a silo gets only its volume's folder, but whoever is root on its host gets all
of them. So a class is one trust domain: put on one class only silos whose hosts you trust with
each other's data, and give tenants that must not share a host's reach classes of their own (each
its own file system, metadata database and bucket credentials).

Two more limits, not yet tested on real machines (`juicefs/realhost_linux_test.go` checks them on
a Linux host with `juicefs`; see [Test](#test)):

- **JuiceFS's control file.** A silo's mount is a sub-folder of the class's file system, with
  JuiceFS's own files (`.control` and the like) renamed `.jfs.*` and hidden from listings, but still
  there by name. If the control file takes inode numbers, root in a silo may reach a sibling
  volume's files through it.
- **Late writes after a fence.** When a host is cut off and its silo is fenced, the silo's processes
  are killed, not the host's JuiceFS client: what it had written and not yet committed may be
  committed when that host's client runs again, after the silo started elsewhere and wrote there.

Until those are settled, don't put on a JuiceFS class data that one tenant must not read of
another's, or that must not take a stale write after a move.

## Configuration

`--config FILE` (JSON, not secret: credentials are only named):

```json
{
  "classes": [{
    "name": "juicefs",
    "metadata": "postgres://juicefs@10.63.0.1:5432/juicefs",
    "storage": "s3",
    "bucket": "https://objects.example.com/volumes",
    "credentials": {
      "accessKey": { "name": "juicefs", "key": "access-key" },
      "secretKey": { "name": "juicefs", "key": "secret-key" },
      "metaPassword": { "name": "juicefs", "key": "metadata-password" }
    }
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
| `credentials` | where the bucket's key pair and the metadata password are: each a cluster secret's `name` and one of its keys (`key`, default `value`); each optional. Install passes each cluster secret's name with `--cluster-secret` |
| `stateDir`, `volumesDir` | the controller side's folder, and the folder the node side mounts under (kevasod's, `/var/lib/kevaso/volumes`) |

The credentials reach the `juicefs` command through its environment (`ACCESS_KEY`, `SECRET_KEY`,
`META_PASSWORD`), never its arguments. The host keeps each cluster secret as a folder with one file
per key, and systemd loads each key as the credential `NAME_KEY` (`juicefs_access-key`). Outside
systemd, `KEVASO_JUICEFS_CREDENTIALS_DIR` names a folder holding one file per credential, named the
same way.

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

Create one cluster secret holding the three credentials as keys, then allow exactly that name. Both sides need it, so the binding names both service accounts in the namespace `kevaso-system`: `plugin-juicefs` for the controller side and `plugin-juicefs-node` for the node side. A host fetches a value for a side only when that side's account may read the name.

```bash
kvo create clustersecret juicefs --from-file access-key=bucket-access-key.txt --from-file secret-key=bucket-secret-key.txt --from-file metadata-password=metadata-password.txt
```

Save this as `juicefs-secrets.yaml`. `resourceNames` lists exactly that name.

```yaml
apiVersion: kevaso/v1
kind: ClusterRole
metadata: { name: juicefs-secrets }
rules:
  - verbs: [get]
    resources: [clustersecrets/value]
    resourceNames: [juicefs]
---
apiVersion: kevaso/v1
kind: ClusterRoleBinding
metadata: { name: juicefs-secrets }
roleRef: { kind: ClusterRole, name: juicefs-secrets }
subjects:
  - { kind: ServiceAccount, name: plugin-juicefs, namespace: kevaso-system }
  - { kind: ServiceAccount, name: plugin-juicefs-node, namespace: kevaso-system }
```

```bash
kvo apply -f juicefs-secrets.yaml
```

```bash
kvoadm plugin install root@cp-1 \
    --name juicefs \
    --binary bin/kevaso-provider-juicefs-linux-amd64 \
    --manifest kevaso-plugin.json --yes \
    --config juicefs.json \
    --cluster-secret juicefs
```

```bash
kvoadm plugin install root@hv-1 \
    --name juicefs \
    --side node \
    --binary bin/kevaso-provider-juicefs-linux-amd64 \
    --manifest kevaso-plugin.json --yes \
    --config juicefs.json \
    --cluster-secret juicefs
```

Install creates the service account of the side it installs (`plugin-juicefs`, or `plugin-juicefs-node` with `--side node`) in the namespace `kevaso-system` and prints the account to bind. It records the name. It does not copy the files. Each host fetches the keys and keeps copies only root can read. The plugin reads each key from its credential directory as `juicefs_KEY`.

To replace one key (the others stay):

```bash
kvo set clustersecret juicefs --from-file secret-key=bucket-secret-key.txt
```

Each host writes the new copy and restarts the plugin. The same bytes again leave the plugin running.

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

On a Linux host with `juicefs` on PATH and `/dev/fuse`, as root, `juicefs/realhost_linux_test.go`
runs the real command over a SQLite file and a local folder (no database server, no bucket) and
checks what the fakes can't:

```bash
sudo KEVASO_JUICEFS_REAL=1 go test -count=1 -v -run TestRealHost ./juicefs/
```

On real machines, `test/e2e` (build tag `e2e`) runs the class end to end on Linodes: a backends
Linode (Postgres for the metadata, SeaweedFS for the bucket, firewalled to the cluster), a control
plane and two docker workers, then a silo's volume formatted, mounted, sized, moved, and after
its host is powered off moved by a person, cut off from its database, and deleted. It costs money and needs a
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
