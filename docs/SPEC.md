# sysext-alpine — Technical Specification

Reimplementation of `systemd-sysext`/`systemd-confext` behavior as a standalone
static Go binary for Alpine Linux (musl, OpenRC, no systemd).

Sources: systemd 262 (`src/sysext/sysext.c`, `src/shared/extension-util.c`,
`src/basic/os-util.c`, `src/shared/discover-image.c`, `src/shared/vpick.c`,
`src/basic/env-file.c`, `src/shared/conf-parser.c`), the man pages in
`docs/reference/`, UAPI Discoverable Partitions Specification.

Compatibility goal: **same paths, same on-disk/runtime layout, same overlay
semantics** as systemd-sysext, so images and tooling interoperate.

## 1. Concepts

| | sysext | confext |
|---|---|---|
| Merges into | `/usr` and `/opt` | `/etc` |
| Release file in image | `/usr/lib/extension-release.d/extension-release.NAME` | `/etc/extension-release.d/extension-release.NAME` |
| Level / scope fields | `SYSEXT_LEVEL=`, `SYSEXT_SCOPE=` | `CONFEXT_LEVEL=`, `CONFEXT_SCOPE=` |
| Search dirs (priority order) | `/etc/extensions`, `/run/extensions`, `/var/lib/extensions` | `/run/confexts`, `/var/lib/confexts`, `/usr/local/lib/confexts`, `/usr/lib/confexts` |
| Class suffix | `.sysext` | `.confext` |
| Mount flags on overlay | `ro,nodev` | `ro,nodev,nosuid,noexec` (`--noexec=false` drops noexec) |

The confext order follows systemd's `image_search_path` (the man page lists
`/usr/lib/confexts` before `/usr/local/lib/confexts`; the code wins).

- Extensions are purely additive by design (overlayfs allows override; permitted but discouraged).
- Files outside the target hierarchies in an image are ignored.

### Discovery (`image_discover()`)

- Search dirs and their entries are resolved inside `--root` (absolute
  symlink targets restart at the root, `..` never leaves it). Dangling
  entries and `.sysupdate.*` temporaries are skipped; nothing else is hidden,
  so `.foo.raw` is the image `.foo`.
- Regular files need the `.raw` suffix (`foo.raw` → `foo`), directories are
  directory images, block devices are `block` images named after the entry.
  The class suffix is optional and stripped: `foo.sysext.raw` and the
  directory `foo.sysext` are both `foo` for sysext (but `foo.confext.raw`
  stays `foo.confext`). Names must be valid image names: a file name without
  control characters, valid UTF-8, not starting with `.#`.
- Versioned directories (vpick): `NAME[.sysext][.raw].v/` contains
  `NAME_VERSION[_ARCH][+LEFT[-DONE]]SUFFIX` entries (regular files or block
  devices for `.raw`, directories otherwise). Entries for a foreign
  architecture are skipped (native, secondary — e.g. x86 on x86-64 — or no
  architecture qualify). The pick prefers entries with boot tries left, then
  the newest version (`strverscmp_improved`), then native over secondary
  architecture, then more tries left, fewer tries done, and finally the file
  name. The image is called `NAME`; a `.v` dir without a usable entry is
  skipped and shadows nothing.
- The first image of a name (search dir priority, then name) shadows all
  later ones. An empty directory named like an extension in a higher-priority
  dir therefore **masks** it: it is listed and, carrying no release data,
  ignored at merge time.
- `.mstack` directories are not supported and skipped (systemd lists them
  and refuses to merge them).
- `list` time: mtime (µs) for raw images, birth time (statx btime or the
  `user.crtime_usec` xattr, whichever is older) for directories.

## 2. extension-release lookup and matching

### Lookup (`open_extension_release()`)

Inside the image tree (symlinks resolved inside the image):

1. `extension-release.NAME` in the class's release dir, if it exists.
2. Otherwise every regular, non-symlink `extension-release.X` (valid image
   name, hidden and backup files skipped) is a candidate when `X` equals the
   image's base name — `NAME` without `.sysext.raw`/`.confext.raw`/`.raw`,
   cut at the first `_` or `+` (so `foo_1.2.raw` uses
   `extension-release.foo`) — or when its `user.extension-release.strict`
   xattr parses as boolean false (`parse_boolean`: `0`, `no`, `n`, `false`,
   `f`, `off`, any case). More than one candidate is an error (ENOTUNIQ).

A missing, ambiguous or unparsable release file means the image carries no
release data. File format: os-release(5), parsed like systemd's env-file
parser (whitespace around `=` ignored, `#`/`;` comments, quotes may span
lines and concatenate, backslash escapes and continuations; invalid UTF-8
fails the file).

A sysext directory image must not contain `/usr/lib/os-release` (it would
replace the host's OS identity); such an image fails the merge. For disk
images the check is part of mounting: an image whose tree is neither an
extension of either class (without that file) nor, when reading metadata,
an OS tree is refused.

### Host data

Host os-release: `$SYSTEMD_OS_RELEASE` if set (no fallback), else
`/etc/os-release`, then `/usr/lib/os-release`, resolved inside `--root`. An
empty host `ID=` fails the merge. Host scope: `initrd` when
`/etc/initrd-release` exists in the root, `system` otherwise. Architecture:
`uname(2)` (honors `setarch`), mapped to systemd identifiers (`x86-64`,
`arm64`, `x86`, `arm`, `riscv64`, ...); GPT partition selection uses the
native build architecture instead.

### Matching (`extension_release_validate()`)

An image is *ignored* (not merged, no error) unless all of these hold, in
order:

1. It carries release data.
2. Scope: `SYSEXT_SCOPE=`/`CONFEXT_SCOPE=` (whitespace-separated) contains
   the host scope; absent means `system portable`, present-but-empty matches
   nothing.
3. `ARCHITECTURE=` is empty, `_any` or the host architecture.
4. `ID=` is non-empty; `_any` matches immediately.
5. `ID=` equals the host `ID` or one of the host `ID_LIKE` words.
6. If the host has neither `VERSION_ID` nor the class level: match (rolling
   release).
7. If host and extension both set the class level, they must be equal;
   otherwise, if the host has `VERSION_ID`, the extension's `VERSION_ID` must
   be equal. Empty values count as unset.

`--force` skips matching but not the metadata checks. Alpine's `VERSION_ID`
includes the patch level (`3.24.2`), so `VERSION_ID`-pinned extensions stop
matching on every point release; `ID=_any` or a `SYSEXT_LEVEL` agreed with
the host avoids that.

Release fields consumed by the service manager step (§6):
`EXTENSION_RELOAD_MANAGER=` (boolean), `EXTENSION_RESTART_UNITS=` and
`EXTENSION_RELOAD_OR_RESTART_UNITS=` (whitespace-separated unit names).

## 3. Image formats

1. **Directory** (or btrfs subvolume): bind-mounted into the workspace.
2. **Raw image** (`*.raw`, or a block device): a bare filesystem, an MBR
   image or a GPT disk image (DDI) per the UAPI Discoverable Partitions
   Specification, dissected like systemd's `dissect_image()`: partitions
   picked per architecture (native, then secondary), root and/or usr,
   dm-verity with the root hash from a sidecar, the signature partition or
   the partition UUIDs, PKCS#7 signature verification and
   systemd.image-policy(7) enforcement. Details: docs/VERITY.md.

A disk image whose tree is not an extension (no matching extension-release)
is ignored; one carrying `/usr/lib/os-release` instead is refused.

## 4. Runtime workspace & overlay construction

### Workspace

`/run/systemd/sysext/` (confext: `/run/systemd/confext/`): a private tmpfs
(source `sysext`/`confext`, mode 0700) mounted while something is merged.
Merges into a `--root` keep their workspace on the host as well, at
`/run/systemd/<class>.<first 16 hex digits of sha256(resolved root)>`, never
inside the root. Operations are serialized with `flock` on
`<workspace>.lock` (systemd itself does not lock).

```
<workspace>/
├── extensions/<name>/      image trees: raw images mounted, directories
│                           bind-mounted (systemd's layout)
├── meta/<hierarchy>/       top layer holding the metadata directory
├── overlay/<hierarchy>/    staging mount point of the overlay
├── mh_workspace/<hierarchy>/  ephemeral upper/work dirs (ephemeral modes)
└── next.*/                 refresh of a live merge builds here first
```

Hierarchy paths are nested, not escaped (`meta/usr`, `meta/foo/bar`). Each
refresh builds in a new `next.*` directory; one left behind because moving
it into place failed may still back the merged overlays and stays until the
workspace goes away on unmerge.
Teardown only unmounts (via `/proc/self/mountinfo`, deepest first) and never
deletes through symlinks.

### Layers (per hierarchy, e.g. `/usr`)

Overlayfs semantics: **first lowerdir = topmost = wins conflicts.**

```
lowerdir = meta/<hierarchy>               (metadata directory)
         : imported mutable dir           (import modes only)
         : extensions/<newest>/<hierarchy> (strverscmp_improved, newest first)
         : ...
         : extensions/<oldest>/<hierarchy>
         : <hierarchy>                     (host, unless it is the upperdir)
```

Extensions without content for a hierarchy are left out; a hierarchy no
extension contributes to is not merged (unless a mutable mode is active).
Hierarchies resolve inside `--root` (`chase`); missing ones are created as
mount points when an extension contributes to them. The merged root keeps
the mode of the host hierarchy. Mounts below the hierarchy are cloned into
the merged tree and restored on unmerge, like systemd's `move_submounts()`.
Staging works when `/` and `/run` have shared propagation.

### Metadata directory

`.systemd-sysext/` (confext: `.systemd-confext/`) at the top of every merged
hierarchy, byte-compatible with systemd 262:

| file | content |
|---|---|
| `extensions` (confext: `confexts`) | all merged extension names, one per line, in merge order |
| `dev` | `MAJOR:MINOR` of the overlay mount (decimal `dev_t` written by older versions is still read) |
| `backing` | `MAJOR:MINOR` of the block device holding the host hierarchy, when there is one |
| `origin` | sd_json pretty JSON: `mutable.mode`, `mutable.mutableDirs`, `mountOptions`, and per extension its path plus `verityHash`, or file handle / inode, mount id, crtime and mtime |
| `work_dir` | C-escaped workdir path relative to the root (persistent mutable modes) |

`refresh` skips when the new origin equals the recorded one (unless
`--always-refresh=yes`). In mutable modes the metadata directory is mounted
read-only so it cannot be copied up.

A hierarchy is merged by us iff it is a mount point whose `dev` marker
equals its `st_dev`; symlinked hierarchies are resolved first.

### Mount flags and options

- sysext: `ro,nodev`; confext: `ro,nodev,nosuid,noexec`. `--noexec=` sets
  or clears `noexec` for both classes.
- Overlay source `sysext`/`confext`, options `lowerdir=...`; mutable modes
  add `upperdir=,workdir=` and `redirect_dir=on,noatime,metacopy=off,index=off`
  (`noatime` becomes `MS_NOATIME`). `SYSTEMD_*_OVERLAYFS_MOUNT_OPTIONS` is
  appended and replaces the mutable defaults, also when set to the empty
  string; generic mount flags in it are turned into `MS_*` flags. See
  docs/MUTABLE.md.
- Image mounts: read-only, `nodev`, journal replay disabled.

## 5. Commands

- `merge`: fails with `Hierarchy '%s' is already merged.` when any hierarchy
  is merged. Discovers, mounts and checks every image once (metadata checks
  even with `--force`), ignores incompatible ones and merges the rest. With
  nothing left: `No extensions found.` / `No suitable extensions found (N
  ignored due to incompatible image(s)).`, exit 0 — or, in a mutable mode,
  `No extensions found, proceeding in mutable mode.`
- `refresh`: builds the new merge first and compares origins; unchanged →
  `Skipping extension refresh because no change was found, use
  --always-refresh=yes to always do a refresh.`; changed → old merge replaced
  under one lock (a failure before the swap keeps the old merge); nothing
  left → unmerge.
- `unmerge`: unmounts every overlay of the class (stacked ones too),
  restores submounts, removes the recorded workdirs, releases the
  workspace. Idempotent.
- `status` (default verb): HIERARCHY / EXTENSIONS / SINCE table, one
  extension per line, `-` for empty cells; hierarchies that do not exist
  are left out. JSON: `{"hierarchy", "extensions": [...], "since": usec|null}`.
- `list`: NAME / TYPE / PATH / TIME, sorted by name (`strcmp`); `No OS
  extensions found.` on stderr when empty (JSON: `[]`).
- merge, unmerge and refresh need the effective `CAP_SYS_ADMIN`.
- `--json=short|pretty` follows sd_json formatting; messages go to stderr,
  plain, filtered by `SYSTEMD_LOG_LEVEL` (default info). Errors use C
  `strerror()` wording. `Using extensions '…'.` is printed before the
  overlays are mounted; any failure while merging ends with `Failed to merge
  hierarchies` (not metadata failures and not "already merged"). A refresh
  whose cleanup fails after the new merge went live reports the merge, then
  `Extensions merged, but failed to clean up …`, and exits 1.
- SIGINT, SIGTERM and SIGHUP are held while mounts are being changed and
  delivered afterwards.

## 6. Alpine integration

- Static build: `CGO_ENABLED=0 go build`.
- OpenRC services `sysext` and `confext` (boot runlevel): `start` and
  `reload` run `refresh`, `stop` runs `unmerge` (with `--no-reload` while
  the system goes down, `RC_GOINGDOWN`); conf.d `SYSEXT_OPTS` /
  `CONFEXT_OPTS`; confext defaults to `--noexec=false` because OpenRC
  executes `/etc/init.d` scripts directly.
- Kill switch: `systemd.sysext=` / `systemd.confext=` on the kernel command
  line (`$SYSTEMD_PROC_CMDLINE` overrides it; `rd.` variants inside an
  initrd) disables every verb when invoked by a service manager
  (`RC_SVCNAME` or `SYSTEMD_EXEC_PID`): `Disabled by the kernel command line
  option '%s=', skipping execution.`, exit 0.
- Service manager step (after merge/refresh, around unmerge; never with
  `--no-reload`, any `--root`, or without a running OpenRC): extension-release
  files are read from the merged tree; `EXTENSION_RELOAD_MANAGER=` true or
  any listed unit → `rc-update -u`; `EXTENSION_RESTART_UNITS=` →
  `rc-service SVC restart`; `EXTENSION_RELOAD_OR_RESTART_UNITS=` → `reload`
  when the script defines `reload()`, else `restart`; restart wins over
  reload-or-restart. Unit names are validated like `unit_name_is_valid()`;
  `foo.service` → `foo`, `foo@bar.service` → `foo.bar`; other types are
  ignored with a warning. Before unmerging, started services whose init
  script comes from an extension tree are stopped.

## 7. Out of scope (deliberate)

- initrd integration, sysroot services, `/.extra/sysext`
- LUKS (`encrypted` image-policy term never matches)
- Varlink interface, systemd-sysupdate notification, polkit, SELinux labels
- `.mstack` directories
