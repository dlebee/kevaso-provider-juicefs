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

**The metadata database matters:** every host using the class must reach it. While it's down, reads
and writes on the volumes **hang** (no errors) and resume about 2 s after it's back (research E029).

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

## Not yet

- **Real machines.** The node side must run as root with FUSE (`/dev/fuse`) and its mounts must be
  visible to kevasod and the container runtime (the host's mount namespace): `kvoadm plugin install`'s
  hardened unit doesn't allow that yet, nor does it give the node side its `node.sock`. To verify on
  a real host: `juicefs mount --subdir` of a folder the controller made; `juicefs status` saying
  "not formatted" on a new database.
- **microVM silos** (virtiofs from the host's mount) and the disk-image mode.
- **Sizes**: a volume's size isn't enforced (a JuiceFS directory quota would).
- **Running the metadata database**: bring your own, or a silo in `kevaso-system`.
