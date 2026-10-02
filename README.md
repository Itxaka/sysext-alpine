# sysext-alpine

A standalone reimplementation of `systemd-sysext` and `systemd-confext` for
Alpine Linux — a single static Go binary, no systemd required. It tracks
systemd 262.

## What are sysext / confext?

*System extensions* (sysext) are read-only images that extend a running
system's `/usr` and `/opt` hierarchies via overlayfs, without modifying the
base system. *Configuration extensions* (confext) do the same for `/etc`.
They are the systemd-world mechanism for shipping add-on software (debugging
tools, container runtimes, kernel-adjacent userspace) onto immutable or
image-based systems.

This tool brings the same mechanism to Alpine Linux (musl, OpenRC):

- **Same paths**: extensions are discovered in `/etc/extensions`,
  `/run/extensions`, `/var/lib/extensions` (confext: `/run/confexts`,
  `/var/lib/confexts`, `/usr/local/lib/confexts`, `/usr/lib/confexts`),
  including versioned `NAME.raw.v/` directories as written by
  systemd-sysupdate.
- **Same image formats**: plain directories, raw filesystem images
  (squashfs, erofs, ext4, btrfs, xfs, f2fs, vfat), and GPT disk images
  following the UAPI Discoverable Partitions Specification, with dm-verity
  and signature verification.
- **Same markers**: merged hierarchies carry the same `.systemd-sysext/`
  (`.systemd-confext/`) metadata files in the same formats as systemd 262,
  so `systemd-sysext` and this tool can inspect, refresh and unmerge each
  other's merges.
- **Same output**: `status` and `list` tables and `--json` output, messages
  and exit codes match systemd-sysext 262.

The binary behaves as `confext` when invoked through the `confext` (or
`systemd-confext`) symlink, with `SYSTEMD_INVOKED_AS=systemd-confext`, or
with the `--confext` flag.

## Building

Requires Go 1.26 or newer (CI and releases build with the toolchain pinned
in `go.mod`):

```sh
make build
```

This builds the static `bin/sysext` (`CGO_ENABLED=0`) and the `bin/confext`
symlink. `make build-all` cross-compiles for amd64, arm64 and riscv64.

## Usage

```sh
sysext list      # installed extension images (NAME / TYPE / PATH / TIME)
sysext merge     # merge into /usr and /opt (fails if already merged)
sysext status    # merge state per hierarchy (default verb)
sysext unmerge   # unmount the overlays, release loop and dm devices
sysext refresh   # replace the merge when images changed, unmerge if none are left

confext merge    # same, but for /etc
confext unmerge
```

`sysext --help` lists all options. The important ones:

- `--root=PATH` operates on another OS tree (the service manager is never
  touched then).
- `--force` merges images whose extension-release does not match the host.
- `--mutable=no|auto|yes|import|ephemeral|ephemeral-import` makes merged
  hierarchies writable (see `docs/MUTABLE.md`).
- `--image-policy=POLICY` restricts which disk images are accepted
  (systemd.image-policy(7), see `docs/VERITY.md`).
- `--noexec=BOOL` mounts the overlay with or without `noexec` (confext
  defaults to `noexec`).
- `--always-refresh=yes` makes `refresh` remerge even when nothing changed.
- `--no-reload` skips the service manager actions described below.
- `--json=short|pretty` for `status` and `list`.

Settings are taken from the command line first, then from the environment,
then from configuration files:

- `SYSTEMD_SYSEXT_MUTABLE_MODE` / `SYSTEMD_CONFEXT_MUTABLE_MODE` set the
  default mutability mode.
- `SYSTEMD_SYSEXT_OVERLAYFS_MOUNT_OPTIONS` /
  `SYSTEMD_CONFEXT_OVERLAYFS_MOUNT_OPTIONS` add overlayfs mount options.
- `SYSTEMD_SYSEXT_HIERARCHIES` / `SYSTEMD_CONFEXT_HIERARCHIES` replace the
  merge targets (colon-separated absolute paths; an invalid list is an
  error).
- `/etc/systemd/sysext.conf` (confext: `confext.conf`) and drop-ins provide
  `Mutable=` and `ImagePolicy=` in `[SysExt]` / `[ConfExt]`, with the search
  order of sysext.conf(5). Invalid settings are warned about and ignored.

`SYSTEMD_LOG_LEVEL=debug` shows why images are ignored and what is done with
OpenRC services. All messages go to stderr; stdout carries only tables and
JSON.

### Creating a squashfs sysext image

1. Build the content tree. Only files under `usr/` and `opt/` are merged;
   everything else is ignored:

   ```sh
   mkdir -p tree/usr/bin tree/usr/lib/extension-release.d
   cp mytool tree/usr/bin/
   ```

2. Add the extension-release file. Its suffix must match the image name
   (here: `mytool`; a deployed `mytool_1.2.raw` still finds
   `extension-release.mytool`):

   ```sh
   cat > tree/usr/lib/extension-release.d/extension-release.mytool <<EOF
   ID=_any
   ARCHITECTURE=_any
   EOF
   ```

   Instead of `ID=_any` you can pin the extension to the host distribution:
   `ID=alpine` plus the host's exact `VERSION_ID=` (Alpine bumps it on every
   point release, e.g. `3.24.2`). `SYSEXT_LEVEL=` is only compared when the
   host's os-release sets it too, which Alpine does not. Derivatives listing
   `alpine` in `ID_LIKE=` accept `ID=alpine` extensions. Set
   `ARCHITECTURE=x86-64` / `arm64` to pin the architecture.

3. Pack it and install:

   ```sh
   mksquashfs tree mytool.raw -noappend
   mv mytool.raw /var/lib/extensions/
   ```

4. Activate:

   ```sh
   sysext merge
   mytool --version   # now available in /usr/bin
   ```

Plain directories work too — just drop the tree at
`/var/lib/extensions/mytool/`. Images whose extension-release does not match
the host are skipped (`No suitable extensions found (1 ignored due to
incompatible image(s)).`), the others are merged. To temporarily *mask* an
extension, create an empty directory with its name in a higher-priority
search dir, e.g. `mkdir /etc/extensions/mytool`, then `sysext refresh`.

`refresh` compares what is merged with what is installed by identity (verity
root hash, or file handle, inode and timestamps) plus the mutability mode and
mount options: replacing `mytool.raw` with a new build is picked up, an
unchanged set is left alone. A failed refresh keeps the previous merge.

Mounts below a merged hierarchy (say a tmpfs on `/usr/local/cache`) stay
visible while it is merged and are put back on unmerge.

## OpenRC integration

Install the init scripts and their conf.d files from `packaging/openrc/`
(the Alpine package ships them in `sysext-openrc`):

```sh
install -m 0755 packaging/openrc/sysext.initd  /etc/init.d/sysext
install -m 0755 packaging/openrc/confext.initd /etc/init.d/confext
install -m 0644 packaging/openrc/sysext.confd  /etc/conf.d/sysext
install -m 0644 packaging/openrc/confext.confd /etc/conf.d/confext
rc-update add sysext boot
rc-update add confext boot   # optional, see below
```

Like `systemd-sysext.service`, `start` runs `refresh`, `stop` runs `unmerge`
and `rc-service sysext reload` refreshes. While the system shuts down
(`RC_GOINGDOWN`), `stop` unmerges with `--no-reload`, so no service is
restarted on the way down. The services run in the boot
runlevel after `localmount` and `modules` and before `sysctl`, `bootmisc`,
`hostname` and `networking`, so services started later see the extensions.
Options go into `SYSEXT_OPTS` / `CONFEXT_OPTS` in conf.d. Without
`CAP_SYS_ADMIN` (unprivileged containers) the scripts do nothing.

The kernel command line options `systemd.sysext=0` and `systemd.confext=0`
disable merging when the tool is started by the service manager (OpenRC sets
`RC_SVCNAME`); manual invocations are not affected. In a container the
arguments of PID 1 take the place of the kernel command line, like in
systemd.

confext caveats on Alpine:

- OpenRC executes the scripts in `/etc/init.d` (and `/etc/local.d`, network
  hooks, ...) directly, which fails on a `noexec` `/etc`. The confext init
  script therefore defaults to `--noexec=false`; a manual `confext merge`
  keeps systemd's `noexec` default and warns when `/etc/init.d` exists.
- A merged `/etc` is read-only: `rc-update`, `apk` and udhcpc's
  `/etc/resolv.conf` updates fail while confexts are merged. Use
  `--mutable=ephemeral` or `--mutable=auto` in `CONFEXT_OPTS` on hosts that
  write to `/etc` at runtime.

### Service manager actions

After a merge or refresh, and around an unmerge, the extension-release files
of the merged extensions are read from the merged tree:

| extension-release field | systemd | sysext-alpine |
|---|---|---|
| `EXTENSION_RELOAD_MANAGER=yes` | daemon-reload | `rc-update -u` |
| `EXTENSION_RESTART_UNITS="foo.service"` | restart (start if stopped) | `rc-service foo restart` |
| `EXTENSION_RELOAD_OR_RESTART_UNITS="foo.service"` | reload, else restart | `rc-service foo reload` when the script defines `reload()`, else `restart` |

Listing any unit implies the reload, and a unit in both lists is only
restarted. `foo@bar.service` maps to OpenRC's multiplexed service
`foo.bar`; other unit types are ignored with a warning. Services whose init
script is shipped by an extension are stopped before it is unmerged, because
OpenRC cannot stop a service whose script is gone. Nothing is done with
`--no-reload`, with `--root`, or when the system was not booted by OpenRC.
Failures are warnings.

## Verity and signed images

GPT images carrying root/usr **verity** partitions are activated through
dm-verity (raw device-mapper ioctls, no udev needed), with the root hash
taken from a `.roothash` sidecar, the signature partition or the partition
UUIDs, like systemd. **Signed** images (verity-signature partitions, e.g.
built with `systemd-repart --private-key=... --certificate=...`) are
verified against the certificates in `/etc/verity.d`, `/run/verity.d`,
`/usr/local/lib/verity.d` and `/usr/lib/verity.d` (`*.crt`): the signer must
be one of them. Policy enforcement uses the full systemd.image-policy(7)
grammar. Loop and dm devices are released by the kernel once the image is
unmounted.

See `docs/VERITY.md` for details and limitations, and `examples/` for a
reproducible signed-sysext example (`make example`) with public test keys.

## Differences from systemd-sysext

- Marker and workspace paths keep the `systemd` name
  (`/run/systemd/sysext/`, `.systemd-sysext/`) for interoperability.
- Service manager interactions go to OpenRC as described above; `--help`
  says so for `--no-reload`.
- `--confext` is an extra option selecting confext mode.
- `--version` prints `sysext-alpine VERSION (systemd-sysext 262)`.

## Not implemented (deliberate)

- initrd integration (`systemd-sysext-initrd.service`, the sysroot services,
  `/.extra/sysext`): there is no systemd-stub flow on Alpine
- LUKS-encrypted partitions (the `encrypted` image-policy term never matches)
- the Varlink interface and the systemd-sysupdate refresh notification
- `.mstack` directories (skipped; systemd lists them but refuses to merge)

## Testing

```sh
make test        # unit tests
make lint        # golangci-lint (pinned version)
make vuln        # govulncheck
make fuzz        # fuzz the parsers of untrusted image data (FUZZTIME=30s)
make cover       # unit tests + coverage report
make e2e         # privileged end-to-end suites in containers (docker/podman)
make e2e-shared  # every suite with private and with shared mount propagation
make e2e-diff    # differential suite against systemd-sysext 262
make e2e-cover   # all suites with a coverage-instrumented binary
```

`SUITES="cli compat"` selects suites, `E2E_STRICT=1` turns unexpected skips
into failures (CI does), and `test/e2e/run.sh` documents the other knobs.
The root-only integration tests run with
`unshare -rm go test ./internal/overlay/ ./internal/discover/`.

The suites (`test/e2e/inner*.sh`) run in privileged containers — Alpine
(pinned in `test/e2e/run.sh`, overridable with `IMAGE=`), and archlinux for
the differential suite, which runs the same scenarios through the real
systemd-sysext 262 and this tool and compares output, exit codes and
marker files. Together they cover every image format, verity and
signatures, all mutable modes, the compatibility matrix, refresh change
detection, submounts and shared propagation, `--root`, configuration and
environment, CLI output, OpenRC service actions and the init scripts. Every
suite ends by checking that no mounts, loop or dm devices are left behind.
