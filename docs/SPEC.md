# sysext-alpine — Technical Specification

Reimplementation of `systemd-sysext`/`systemd-confext` behavior as a standalone
static Go binary for Alpine Linux (musl, OpenRC, no systemd).

Sources: systemd 260 man page (`docs/reference/systemd-sysext.8.txt`),
systemd `src/sysext/sysext.c` (v260), UAPI Discoverable Partitions Specification.

Compatibility goal: **same paths, same on-disk/runtime layout, same overlay
semantics** as systemd-sysext, so images and tooling interoperate.

## 1. Concepts

| | sysext | confext |
|---|---|---|
| Merges into | `/usr` and `/opt` | `/etc` |
| Release file in image | `/usr/lib/extension-release.d/extension-release.NAME` | `/etc/extension-release.d/extension-release.NAME` |
| Level field | `SYSEXT_LEVEL=` | `CONFEXT_LEVEL=` |
| Search dirs (priority order) | `/etc/extensions`, `/run/extensions`, `/var/lib/extensions` | `/run/confexts`, `/var/lib/confexts`, `/usr/lib/confexts`, `/usr/local/lib/confexts` |
| Mount flags on overlay | `ro,nodev` | `ro,nodev,nosuid,noexec` (`--noexec=false` drops noexec) |

- Extensions are purely additive by design (overlayfs allows override; permitted but discouraged).
- Files outside the target hierarchies in an image are ignored.
- An empty **directory** named like an extension (without `.raw`) in a
  higher-priority search dir **masks** a same-named extension in lower-priority dirs.
- In each search dir: subdirectories = directory-based images; `*.raw` files = disk images.
  Symlinks followed.

## 2. extension-release matching

`NAME` in `extension-release.NAME` must match the image name (image filename
minus `.raw`, or directory name). Escape hatch: a file with xattr
`user.extension-release.strict` set to false-y is accepted regardless of name
(systemd picks the single file in the dir then).

File format = os-release(5) format (KEY=VALUE, shell-style quoting, `#` comments).

Match algorithm against host `/etc/os-release` (fallback `/usr/lib/os-release`):

1. `ID=` of extension must equal host `ID`, **unless** extension `ID=_any`.
2. If extension `ID != _any`:
   - If extension defines `SYSEXT_LEVEL=` (confext: `CONFEXT_LEVEL=`): it must
     equal host's same field.
   - Else: extension `VERSION_ID=` must equal host `VERSION_ID=`.
3. If extension defines `ARCHITECTURE=` and value is not `_any`, it must match
   the kernel architecture (uname-based, systemd `ConditionArchitecture=`
   identifiers: `x86-64`, `arm64`, `riscv64`, `x86`, `arm`, ...).
4. `--force` skips all checks.
5. `EXTENSION_RELOAD_MANAGER=1` → systemd reloads the service manager; on
   Alpine: no-op (log only).

Extra fields `SYSEXT_SCOPE=`/`CONFEXT_SCOPE=` (`initrd`, `system`, `portable`):
informational; systemd-sysext checks scope contains `system` when running on a
booted system. Default when absent: applies everywhere.

## 3. Image formats

1. **Plain directory** (or btrfs subvolume) — used directly as lowerdir source.
2. **Raw disk image without partition table** — bare filesystem (squashfs,
   erofs, ext4). Detect by superblock magic. Loop-mount read-only.
3. **Raw GPT disk image** (DDI) — partition discovery per UAPI Discoverable
   Partitions Spec. Loop device with partition scan. Relevant type GUIDs:
   - root x86-64: `4f68bce3-e8cd-4db1-96e7-fbcaf984b709`
   - root arm64:  `b921b045-1df0-41c3-af44-4c6f280d3fae`
   - usr  x86-64: `8484680c-9521-48c6-9c11-b0720656f69e`
   - usr  arm64:  `b0e01050-ee5f-4390-949a-9101b17104e9`
   Use root partition if present, else usr partition (mounted at `/usr` of the
   image tree). Verity/signature partitions: **out of MVP scope** (ignore).
4. Image policy / verity / signatures: out of scope for MVP.

Filesystem magics: squashfs `hsqs` @ 0; erofs `0xE0F5E1E2` @ 1024; ext4
`0xEF53` @ 1080; GPT: `EFI PART` @ LBA1 (offset 512, also check 4096 sector).

## 4. Runtime workspace & overlay construction (compat with systemd)

Workspace: `/run/systemd/sysext/` (confext: `/run/systemd/confext/`), mode 0700:

```
/run/systemd/sysext/
├── extensions/<name>/     # per-image mount point (or symlink target for dirs)
├── meta/<hierarchy>/      # synthesized metadata staging (topmost lowerdir)
├── overlay/<hierarchy>/   # where the overlayfs is assembled before move
```

Note: systemd dissects each image once and mounts the *image root* at
`extensions/<name>`; hierarchy paths used in lowerdirs are
`extensions/<name>/usr`, `extensions/<name>/opt`, `extensions/<name>/etc`.

### Lowerdir ordering (per hierarchy, e.g. `/usr`)

Overlayfs semantics: **first lowerdir = topmost = wins conflicts.**

```
lowerdir = meta/<hierarchy>            (synthesized .systemd-sysext marker dir)
         : ext[N-1]/<hierarchy>        (extensions reverse version-sorted —
         : ...                          newest strverscmp first/topmost)
         : ext[0]/<hierarchy>
         : /<hierarchy>                (host, bottom)
```

Extensions sorted with `strverscmp_improved`; the paths array is built in
reverse so the latest version is the top layer. Skip an extension's hierarchy
dir if the image doesn't contain it. If host hierarchy doesn't exist or is
empty, omit it (and `/opt` often doesn't exist — overlay then made of
extensions only; if only one lowerdir would remain plus meta, still mount
overlay for marker consistency).

### Marker metadata (inside `meta/<hierarchy>/`)

Directory `.systemd-sysext/` (confext: `.systemd-confext/`) containing:
- `extensions` — newline-delimited extension names (merge order)
- `dev` — device major:minor (decimal `dev_t` as string) of the overlay mount,
  written **after** mounting by stat'ing the mount point; used for
  already-merged detection
- `origin` — JSON: mapping/array describing source image paths
- `work_dir` — only in mutable mode (out of MVP scope)

`dev` write timing: systemd creates marker dir + `extensions`/`origin` before
mount, mounts overlay at staging, stats it, writes `dev`, then moves the mount
onto the real hierarchy (`MS_MOVE`).

### Merged-state detection (`is_our_mount_point` equivalent)

A hierarchy is "merged by us" iff:
1. it is a mount point, and
2. `<hierarchy>/.systemd-sysext/dev` exists, and
3. the `dev` value equals `stat(<hierarchy>).st_dev`.

### Mount flags

- sysext overlay: `MS_RDONLY|MS_NODEV` → opts `ro,nodev`
- confext overlay: `MS_RDONLY|MS_NODEV|MS_NOSUID|MS_NOEXEC`
- Image loop mounts: read-only.
- Overlay fs options for immutable mode: just `lowerdir=...` (no upperdir/workdir).
  (Mutable mode would add `redirect_dir=on,noatime,metacopy=off,index=off` +
  upperdir/workdir — out of MVP scope.)

## 5. Commands

- `merge` — discover + validate + mount overlays over `/usr` & `/opt`
  (confext: `/etc`). **Fails if already merged.** No extensions found → no-op
  (exit 0, message).
- `unmerge` — for each hierarchy: if merged-by-us, unmount (detach-lazy
  fallback), dismantle loop devices, clean workspace.
- `refresh` — unmerge (if merged) then merge. If no extensions installed →
  just unmerge. By default skip if merged set already matches
  (`--always-refresh=yes` forces).
- `status` (also default with no verb) — table: HIERARCHY / EXTENSIONS /
  SINCE. Reads `.systemd-sysext/extensions` from each hierarchy. "none" when
  unmerged.
- `list` — table of discovered installed images: NAME / TYPE (raw/directory) / PATH / TIME.
- `--json=short|pretty|off` for status/list.
- `--root=PATH` — operate relative to alternate root.
- `--force` — skip version compat checks.
- `--no-reload`, `--noexec=BOOL` (confext) accepted.
- Binary behaves as confext when invoked via argv[0] `confext` /
  `systemd-confext` symlink, or with `--confext` flag (our extension).

## 6. Alpine integration

- Static musl build: `CGO_ENABLED=0 go build`.
- OpenRC service `/etc/init.d/sysext` (and `confext`): `start` = merge,
  `stop` = unmerge. Runs after `localmount`.
- Marker paths intentionally keep the `systemd` name for interop with images
  and tooling that probe `/run/systemd/sysext` & `.systemd-sysext`.

## 7. Out of MVP scope (tracked, not implemented)

- Verity / signatures / image policies
- Mutable modes (`--mutable=`)
- initrd integration, `/.extra/sysext`
- `EXTENSION_RELOAD_MANAGER` action
- btrfs subvolume special-casing (plain dir handling covers it)
