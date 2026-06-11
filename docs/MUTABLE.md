# Mutable modes (`--mutable=`)

By default merging renders the target hierarchies (`/usr`, `/opt` for sysext;
`/etc` for confext) read-only. The `--mutable=` option enables write routing,
matching systemd-sysext (v256+) semantics. See the MUTABILITY section of
`docs/reference/systemd-sysext.8.txt`.

## Write-routing directories

Writes are routed per hierarchy to a directory under
`/var/lib/extensions.mutable/` (relative to `--root`), named after the
hierarchy's basename:

| Hierarchy | Routing directory |
|---|---|
| `/usr` | `/var/lib/extensions.mutable/usr` |
| `/opt` | `/var/lib/extensions.mutable/opt` |
| `/etc` | `/var/lib/extensions.mutable/etc` |

A routing entry may be a **symlink**; writes then go to its target. To keep
the host file system itself mutable while merged, route writes back to it:

```sh
mkdir -p /var/lib/extensions.mutable
ln -s /usr /var/lib/extensions.mutable/usr
ln -s /opt /var/lib/extensions.mutable/opt
ln -s /etc /var/lib/extensions.mutable/etc
```

(When a routing dir resolves to the host hierarchy itself, it serves as the
overlay upperdir and is *not* also added as the bottom lowerdir.)

## Modes

| Mode | Overlay | Routing dir |
|---|---|---|
| `no` (default) | read-only | ignored even if present |
| `auto` | per hierarchy: writable iff its routing dir (or symlink to a dir) exists | used as upperdir when present; never created |
| `yes` | writable for all hierarchies | created (`0755`, parents included) when missing; used as upperdir |
| `import` | read-only | contents merged in as an extra lowerdir, directly below the marker layer (so they override extensions and host) |
| `ephemeral` | writable | ignored; upper/work dirs live on a fresh tmpfs private to the merge — all changes vanish on unmerge |
| `ephemeral-import` | writable | imported as lowerdir like `import`, but writes go to the ephemeral tmpfs |

Unknown mode strings are rejected. Boolean spellings (`1/true/on` →
`yes`, `0/false/off` → `no`) are accepted on the command line.

## Mechanics

- **Mutable overlays** drop `MS_RDONLY` and mount with
  `upperdir=…,workdir=…,redirect_dir=on,metacopy=off,index=off` (plus the `MS_NOATIME` mount flag)
  (systemd's `MUTABLE_EXTENSIONS_MOUNT_OPTIONS`).
- **Workdir**: overlayfs requires the workdir on the same filesystem as the
  upperdir, so it is a hidden sibling of the *resolved* upperdir:
  `<parent>/.<escaped-hierarchy>-workdir`, e.g.
  `/var/lib/extensions.mutable/.usr-workdir` (or `/.usr-workdir` when the
  routing dir is a symlink to `/usr`). For ephemeral modes a tmpfs is mounted
  at `/run/systemd/sysext/mh_workspace/<hierarchy>` with `upper/` and `work/`
  inside it.
- **Marker**: when a hierarchy is mutable, the workdir path is recorded in the
  marker dir as `<hierarchy>/.systemd-sysext/work_dir` (like systemd), so
  `unmerge` can remove it even across process restarts.
- **Unmerge** removes the recorded workdirs and unmounts/removes the
  ephemeral tmpfs; the routing dirs themselves (and their accumulated
  contents) are left intact, so changes made under `auto`/`yes` reappear on
  the next mutable merge.
- **Lowerdir order** with import:
  `meta : routing-dir : extensions (newest first) : host`.

## Differences vs systemd

- `auto` skips mutability for a hierarchy (instead of failing) when its
  workdir cannot be created; `yes` fails, as in systemd.
- Image verity/signature interactions with mutability are out of scope (no
  verity support in this implementation).
- Everything else (routing paths, symlink semantics, mount options, marker
  `work_dir` file, hidden workdir naming) follows systemd 260's
  `sysext.c` behavior.
