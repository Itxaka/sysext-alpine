# Mutable modes (`--mutable=`)

By default merging renders the target hierarchies (`/usr`, `/opt` for sysext;
`/etc` for confext) read-only. The `--mutable=` option makes them writable or
imports extra content, with the semantics of systemd-sysext 262. See the
MUTABILITY section of `docs/reference/systemd-sysext.8.txt`.

## Mutable directories

Each hierarchy has a mutable directory below `/var/lib/extensions.mutable/`
(inside `--root`), named after the hierarchy with the slashes turned into
dots:

| Hierarchy | Mutable directory |
|---|---|
| `/usr` | `/var/lib/extensions.mutable/usr` |
| `/opt` | `/var/lib/extensions.mutable/opt` |
| `/etc` | `/var/lib/extensions.mutable/etc` |
| `/srv/a/b` | `/var/lib/extensions.mutable/srv.a.b` |

A mutable directory may be a **symlink**, resolved inside `--root`. To keep
the host file system itself writable while merged, point it back at the
hierarchy:

```sh
mkdir -p /var/lib/extensions.mutable
ln -s ../../../usr /var/lib/extensions.mutable/usr
```

When the mutable directory is the hierarchy itself (same path or same inode),
it serves as the upper directory and is not also used as the bottom layer.

## Modes

| Mode | Overlay | Mutable directory |
|---|---|---|
| `no` (default) | read-only | ignored |
| `auto` | writable for hierarchies whose mutable directory exists | used as upper directory when present, never created |
| `yes` | writable | created when missing, used as upper directory |
| `import` | read-only | merged in as a layer directly below the metadata, above the extensions |
| `ephemeral` | writable | ignored; writes go to a directory in the workspace tmpfs and vanish on unmerge |
| `ephemeral-import` | writable | imported like `import`, writes go to the workspace tmpfs |

Unknown modes are rejected; on the command line the boolean spellings map to
`yes` and `no`.

In every mode but `no` all hierarchies are merged, also those no extension
ships content for, and also when no extension qualifies at all ("No
extensions found, proceeding in mutable mode."). An overlay needs at least two
layers, so a hierarchy whose host directory is empty and that gets neither an
upper nor an imported directory cannot be merged; systemd fails the same way.
Importing a hierarchy into itself fails with `ELOOP`.

## Mechanics

- **Mount options**: writable overlays drop `MS_RDONLY` and use
  `redirect_dir=on,noatime,metacopy=off,index=off`
  (`MUTABLE_EXTENSIONS_MOUNT_OPTIONS`; generic flags such as `noatime` become
  mount flags), unless `$SYSTEMD_*_OVERLAYFS_MOUNT_OPTIONS` is set: its
  value, even an empty one, replaces them.
- **Mode**: the merged hierarchy takes the mode of its top layer, so the
  metadata directory (read-only) or the upper directory (writable) is given
  the mode of the host hierarchy (`0755` when it is missing). An existing
  mutable directory with a different mode is refused under `yes` and `auto`;
  one created under `yes` gets the hierarchy's mode.
- **Workdir**: a hidden sibling of the resolved upper directory,
  `.systemd-<hierarchy>-workdir` (e.g.
  `/var/lib/extensions.mutable/.systemd-usr-workdir`), created `0700`. The
  upper directory's parent must be on the same file system (`EXDEV`
  otherwise).
- **Metadata**: the persistent modes record the workdir in
  `<hierarchy>/.systemd-sysext/work_dir`, relative to `--root` and
  C-escaped. `unmerge` only accepts a normalized relative path whose basename
  is the workdir name of that hierarchy, resolves it inside `--root` and
  removes it. The ephemeral modes record nothing; their directories live in
  the workspace (`/run/systemd/sysext/mh_workspace/<hierarchy>/`) and go away
  with it.
- **Read-only metadata**: on writable overlays the `.systemd-sysext`
  (`.systemd-confext`) directory is bind-mounted read-only, so the markers
  cannot be modified or deleted through the overlay. Metadata left in an upper
  directory by an earlier, unprotected merge is removed before mounting.
- **Origin**: the mode and the mutable directories (the ephemeral modes record
  systemd's fixed `/run/systemd/sysext/<hierarchy>`) are part of the origin
  marker, so `refresh` remerges when they change.
- **Layer order**: `metadata : imported directory : extensions (newest
  first) : host`.
- **Unmerge** removes the recorded workdirs; the mutable directories and
  their contents stay, so changes made under `auto` or `yes` reappear on the
  next mutable merge.
