# sysext-alpine

A standalone reimplementation of `systemd-sysext` and `systemd-confext` for
Alpine Linux — a single static Go binary, no systemd required.

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
  `/var/lib/confexts`, `/usr/lib/confexts`, `/usr/local/lib/confexts`).
- **Same image formats**: plain directories, raw filesystem images
  (squashfs, erofs, ext4), and GPT disk images following the UAPI
  Discoverable Partitions Specification.
- **Same markers**: merged hierarchies carry `.systemd-sysext/` metadata and
  the runtime workspace lives at `/run/systemd/sysext/`, so existing images
  and tooling that probe these paths keep working unchanged.

The binary behaves as `confext` when invoked through the `confext` (or
`systemd-confext`) symlink, or with the `--confext` flag.

## Building

Requires Go. Produces a fully static binary suitable for Alpine:

```sh
make build-static
```

This builds `bin/sysext` with `CGO_ENABLED=0` and creates the `bin/confext`
symlink.

## Usage

```sh
sysext list      # show installed extension images (NAME / TYPE / PATH / TIME)
sysext merge     # mount overlays over /usr and /opt (fails if already merged)
sysext status    # show merged state per hierarchy (default verb)
sysext unmerge   # unmount overlays, detach loop devices
sysext refresh   # unmerge (if merged) then merge again

confext merge    # same, but for /etc
confext unmerge
```

Useful flags: `--json=short|pretty`, `--root=PATH`, `--force` (skip
compatibility checks), `--noexec=false` (confext only).

### Creating a squashfs sysext image

1. Build the content tree. Only files under `usr/` and `opt/` are merged;
   everything else is ignored:

   ```sh
   mkdir -p tree/usr/bin tree/usr/lib/extension-release.d
   cp mytool tree/usr/bin/
   ```

2. Add the extension-release file. Its suffix **must** match the image name
   (here: `mytool`):

   ```sh
   cat > tree/usr/lib/extension-release.d/extension-release.mytool <<EOF
   ID=_any
   ARCHITECTURE=_any
   EOF
   ```

   Instead of `ID=_any` you can pin the extension to the host distribution
   (`ID=alpine` plus a matching `VERSION_ID=` or `SYSEXT_LEVEL=`), and
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
`/var/lib/extensions/mytool/`. To temporarily *mask* an extension, create an
empty directory with its name in a higher-priority search dir, e.g.
`mkdir /etc/extensions/mytool`, then `sysext refresh`.

## OpenRC integration

Install the init scripts from `packaging/openrc/`:

```sh
install -m 0755 packaging/openrc/sysext.initd  /etc/init.d/sysext
install -m 0755 packaging/openrc/confext.initd /etc/init.d/confext
rc-update add sysext default
rc-update add confext default   # optional
```

`start` merges, `stop` unmerges; the services run after `localmount`.

## Compatibility notes

- Marker paths intentionally keep the `systemd` name
  (`/run/systemd/sysext/`, `.systemd-sysext/`, `.systemd-confext/`) for
  interoperability with images and tooling built for systemd-sysext.
- extension-release matching follows the systemd algorithm: `ID` must match
  the host `/etc/os-release` (or be `_any`); for non-`_any` IDs either
  `SYSEXT_LEVEL`/`CONFEXT_LEVEL` or `VERSION_ID` must match; `ARCHITECTURE`
  is checked against the kernel architecture unless `_any`.
- Overlay mounts use the same flags as systemd: sysext `ro,nodev`, confext
  `ro,nodev,nosuid,noexec`.
- `EXTENSION_RELOAD_MANAGER=1` is accepted but is a no-op on Alpine (logged
  only) — there is no service manager to reload.

## MVP scope / limitations

Not implemented (tracked in `docs/SPEC.md` §7):

- Verity, signatures, and image policies (verity/signature partitions in GPT
  images are ignored)
- Mutable modes (`--mutable=`)
- initrd integration and `/.extra/sysext`
- `EXTENSION_RELOAD_MANAGER` action
- btrfs subvolume special-casing (plain directory handling covers it)

## Testing

```sh
make test   # unit tests
make e2e    # privileged end-to-end tests in an Alpine container (needs docker)
```

The e2e suite (`test/e2e/`) builds squashfs/ext4/erofs/GPT/directory test
extensions inside a privileged `alpine:3.21` container and exercises the full
merge/unmerge/refresh/masking lifecycle.
