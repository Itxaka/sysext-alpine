# Disk images, dm-verity and signatures

`sysext-alpine` mounts raw extension images the way systemd-sysext 262 does:
the image is dissected like `dissect_image()` in systemd's
`src/shared/dissect-image.c` with the flags systemd-sysext passes, the image
policy is applied per partition designator, dm-verity is set up when the
image carries verity data, and root hash signatures are verified against
the same certificate directories. Everything is done with raw ioctls — no
libblkid, libcryptsetup, libdevmapper or udev. The code lives in
`internal/image`.

## Image formats

A raw image (`*.raw`) is one of:

- **A GPT partitioned disk image (DDI)** per the UAPI Discoverable
  Partitions Specification. The sector size is detected like systemd's
  `gpt_probe()`: a GPT header at LBA 1 for 512, 1024, 2048 or 4096 byte
  sectors (revision 1.0, header size 92–4096, `my_lba` 1); headers matching
  more than one size are refused. The loop device is configured with that
  logical block size, so the kernel sees the same partitions.
  The table is validated the way the kernel and libblkid do before any of
  it is used: protective MBR, header CRC32, `my_lba`, usable range,
  128-byte entries, at most 1024 entries, entry array CRC32. When the
  primary header is invalid the alternate header at the last LBA is used
  (like libblkid; partitions the kernel did not create from it are added
  with `BLKPG_ADD_PARTITION`). Entries outside the usable range are
  skipped. Every partition used is cross-checked against the kernel's view
  (`/sys/block/loopN/loopNpM/{start,size}`).
- **A single filesystem** without partition table.
- **An MBR partitioned image**, whose bootable Linux (0x83) partition is
  used as root.

Block devices (e.g. `/var/lib/extensions/foo` symlinked to `/dev/vdb`) are
read the same way, with the device's size and logical sector size. Like
systemd's `loop_device_make_internal()`, the device itself is used when the
sector size the image needs is the device's and either no partitions are
needed or the kernel scans the device's partitions already; otherwise a
loop device is set up on top of it. A device used as is is never detached.

Filesystems are recognized by superblock: btrfs, erofs, ext4, f2fs,
squashfs, vfat and xfs are mounted (systemd's default list;
`$SYSTEMD_DISSECT_FILE_SYSTEMS=type:type…` overrides it). ext2 and ext3 are
told apart from ext4 by feature flags like libblkid does and refused.
Images with several matching superblock signatures are refused as
ambiguous.

Mounts are read-only and `nodev`. ext3/ext4/xfs and f2fs are mounted with
`norecovery`, btrfs with `rescue=nologreplay` (falling back to
`norecovery` on older kernels), so a journal that needs replaying is never
written to the image (systemd's `partition_pick_mount_options()`).

## Partition selection

Partitions are classified by type GUID into designators (`root`, `usr`,
`root-verity`, `usr-verity`, `root-verity-sig`, `usr-verity-sig`, `esp`,
`xbootldr`, `swap`, `home`, `srv`, `var`, `tmp`). The root/usr type GUIDs of
**all** architectures of the specification are known
(`internal/image/gpt.go`, generated from systemd's `sd-gpt.h`).

- Partitions labelled `_empty` (unused A/B slots of systemd-sysupdate) and
  partitions with the no-auto attribute (bit 63; not for the ESP) are
  skipped. ESPs with the "no block IO protocol" attribute are skipped.
- Partitions whose designator the image policy ignores are skipped; a
  designator the policy requires to be absent refuses the image.
- When several partitions of one designator remain, the host architecture
  wins over its secondary architecture (x86-64 → x86, arm64 → arm,
  s390x → s390, ppc64 → ppc), which wins over any other architecture. A
  foreign-architecture partition is still used when it is the only one
  (the extension-release `ARCHITECTURE=` check decides afterwards). For the
  same architecture, root/usr and their verity partitions pick the newest
  partition label by version comparison; other designators take the first.
- Without root and usr partitions, a single generic Linux partition
  (`0fc63daf-8483-4772-8e79-3d69d8477de4`) is used as root; several are
  refused.
- A `/var` partition is only used when its UUID is bound to the host's
  machine ID (HMAC-SHA256 of the `/var` type UUID, like systemd).

Refused combinations, as in systemd: verity or signature partitions without
their data partition, a signature partition without verity partition, root
and usr of different architectures, verity protected root with a separate
usr partition, verity on both root and usr.

The root partition is mounted at the image mount point and the usr
partition, if any, at `<mount point>/usr` (it must exist in the root
filesystem). An image with only a usr partition gets a tmpfs as its root.
Images without any usable root or usr partition fail with `ENXIO` ("root
or usr partition requested but found neither"), a single filesystem whose
root partition the policy ignores with `ENOPKG`; systemd-sysext treats both
as fatal too. The generic root fallback is checked against the policy's GPT
flags as if it had none set, like in systemd.

## Image policy

`--image-policy=` takes the full systemd.image-policy(7) grammar and
semantics of systemd 262 (`internal/image/policy.go`): per-designator
rules, the empty designator for the default rule, the `open`/`ignore`
shortcuts, the specials `*`, `-` and `~`, filesystem flags and the GPT
`read-only-on/off` and `growfs-on/off` flags. Unlisted designators fall
back to the default rule, which is `ignore` unless given. Rules for
`root-verity`/`usr-verity` and `*-verity-sig` that are not listed are
derived from the data partition's rule (verity partitions need the data
rule to allow `verity` or `signed`, signature partitions `signed`). The
effective per-designator policy matches `systemd-analyze image-policy`,
which the unit tests compare against.

Without `--image-policy=` the class default applies: for sysext `root` and
`usr` may be `verity+signed+encrypted+encryptedwithintegrity+unprotected+absent`
and everything else is ignored; for confext the same holds for `root`
only, so usr partitions are ignored. (systemd parses an explicitly empty
policy string like `-`.)

Enforcement follows `dissect_image()`: every partition found is checked
(use, protection level, GPT flags), not just root and usr. A partition with
a verity partition qualifies for `verity`, with a signature partition also
for `signed`, and always for `unprotected`. Single filesystem images
qualify only for `unprotected`, or for `verity`/`signed` when sidecar
verity data covers them.

Divergence: filesystem flags restrict the allowed filesystem types of a
designator as the man page describes. systemd 262 instead uses a single
listed type as the mount type and ignores a list of several.

## Root hash and dm-verity

The verity root hash of the root (or usr) partition comes from, in this
order (systemd's `verity_settings_load()`,
`dissected_image_load_verity_sig_partition()` and
`dissected_image_guess_verity_roothash()`):

1. sidecar files next to the image (see below),
2. the signature partition's JSON `rootHash`, when the policy admits the
   signature partition (the data rule allows `signed`),
3. the partition UUIDs: data partition UUID = first 128 bits, verity
   partition UUID = last 128 bits.

The verity parameters come from the superblock `veritysetup format` writes
at the start of the hash device (version 1, hash type 0 or 1). The hash
algorithm must be one of sha1, sha224, sha256, sha384 or sha512 and the root
hash must have its digest size, so sha512 images work with a root hash from
a signature or sidecar. The device-mapper table is

```
<hash_type> <data maj:min> <hash maj:min> <data_block_size> <hash_block_size> <data_blocks> 1 <algorithm> <root hash> <salt>
```

A wrong root hash (e.g. UUIDs that do not encode it) makes the mount fail
with `EIO`; a tampered image may mount and fail later on read.

dm verity is used when the root hash is known for the designator and the
policy allows `verity` or `signed`; if the policy allows neither, the data
partition is mounted directly. If the policy allows `signed` but not
`verity`, a signature must verify.

The dm device is named like systemd does without verity sharing: after the
data partition node and the loop device's disk sequence number, e.g.
`loop3p1-42-verity`, unique per attachment, so concurrent activations, a
sysext and a confext of the same name, or validation and merge mounts of
one image never collide. `DM_DEV_CREATE` on an existing name fails; nothing
is ever removed to make room. The DM UUID follows libcryptsetup
(`CRYPT-VERITY-<superblock uuid>-<name>`). The `struct dm_ioctl` buffers are
encoded in native byte order (big-endian hosts such as s390x included).

Without udev the `/dev/mapper/<name>` node is created for the mount and
removed again right after it, since nothing would remove it once the kernel
drops the device.

## Device lifetime

Loop devices are configured with `LO_FLAGS_AUTOCLEAR`, read-only, with
direct I/O when the backing file allows it (`$SYSTEMD_LOOP_DIRECT_IO=0`
disables it), and with partition scanning enabled only after
`LOOP_CONFIGURE` (systemd's workaround for a kernel partition rescan race).
Allocation is serialized with a lock on `/dev/loop-control`. The `/dev/loopN`
and `/dev/loopNpM` nodes are validated against sysfs and recreated when
stale.

Once the filesystems are mounted the dm-verity device is marked for
deferred removal and the loop device fd is closed, like systemd's
`dissected_image_relinquish()`. From then on the kernel owns teardown:
unmounting the image mount points removes the dm device and detaches the
loop device. Nothing needs to be remembered for unmerge, and a crash after
mounting cannot leak devices beyond the mounts themselves.

## Signatures

The signature partition holds a JSON object, NUL-padded:

| Field | Meaning |
|---|---|
| `rootHash` | the verity root hash, hex |
| `signature` | base64 DER PKCS#7 signature over the lowercase hex root hash |

Partitions larger than 4 MiB, data after the NUL padding, a missing field,
invalid hex or base64 are errors. Other fields (`certificateFingerprint`)
are ignored, like systemd does.

A signature is checked when one exists, the policy allows `signed` and
`$SYSTEMD_DISSECT_VERITY_SIGNATURE` is not false:

1. The signature is first handed to the kernel (dm-verity
   `root_hash_sig_key_desc`, verified against the kernel's trusted
   keyrings), like libcryptsetup's `crypt_activate_by_signed_key()`. When
   the kernel refuses, the dm device is removed before anything else
   happens.
2. If the kernel refuses, it is verified in userspace like systemd's
   `validate_signature_userspace()`, unless disabled with
   `$SYSTEMD_ALLOW_USERSPACE_VERITY=0` or `systemd.allow_userspace_verity=0`
   on the kernel command line:
   - Trusted certificates are the `*.crt` files in `/etc/verity.d`,
     `/run/verity.d`, `/usr/local/lib/verity.d` and `/usr/lib/verity.d`
     (below `--root=`). Hidden and backup files are skipped. A file name in
     an earlier directory overrides the same name in later ones; an empty
     file or a symlink to `/dev/null` masks it; only regular files count.
   - The first certificate of each file is used; unreadable or unparsable
     files are skipped.
   - The signer must be one of these certificates
     (`PKCS7_NOINTERN|PKCS7_NOVERIFY`): certificates embedded in the
     signature are ignored, a CA certificate does not make the certificates
     it issued trusted, and validity periods are not checked.
   - Without any trusted certificate the signature is not parsed at all.
   - Signatures larger than 64 KiB or nesting ASN.1 deeper than 32 levels
     are malformed: a root hash signature is a few KiB (the kernel takes at
     most 32767 bytes), and the PKCS#7 parser recurses once per level.
3. If neither verifies the signature, the image is refused when the policy
   does not allow `verity`; otherwise it is mounted as plain verity (the
   hash tree is still enforced) and a warning is passed to
   `MountOpts.Warnf`. Malformed signature data is always an error.

## How failures are reported

Messages follow systemd-sysext 262:

- Without a configured image policy, a raw image that cannot be dissected
  or mounted fails the merge with `Failed to read metadata for image NAME:
  <strerror>` (e.g. `Package not installed` for an image with neither a
  partition table nor a known filesystem).
- With `--image-policy=` or `ImagePolicy=`, an image the policy rejects
  prints `PATH: Image does not match image policy.` followed by `Failed to
  merge hierarchies`; a signature that cannot be verified under a
  signed-only policy prints only `Failed to merge hierarchies`.
- The details (which partition, which check) are logged at debug level:
  `SYSTEMD_LOG_LEVEL=debug`.

## Sidecar verity data

As with systemd's `verity_settings_load()`, files next to the image add
verity data (for `foo.raw` the names drop the `.raw` suffix):

| Source | Meaning |
|---|---|
| xattr `user.verity.roothash` or `foo.roothash` | root hash for the root partition |
| xattr `user.verity.usrhash` or `foo.usrhash` | root hash for the usr partition |
| `foo.roothash.p7s` / `foo.usrhash.p7s` | PKCS#7 signature of that root hash (must not be empty, at most 4 MiB) |
| `foo.verity` | hash tree for a single filesystem image |

`$SYSTEMD_DISSECT_VERITY_SIDECAR=0` disables them. A single filesystem with
a `.verity` file and a root hash is mounted through dm-verity (the hash
file is attached to a second loop device) and classifies as `verity`, or
`signed` with a signature. A `.verity` file next to a partitioned image is
an error. For GPT images a sidecar root hash selects the data and verity
partitions whose UUIDs match its halves, and a sidecar signature is used
instead of the signature partition. Sidecars are looked up next to the
image path as discovered, which has symlinks resolved; device nodes below
`/dev` or `/sys` have none (systemd's `is_device_path()`).

## Limitations

1. **A root of trust exists only with signature verification.** For plain
   `verity` the root hash comes from the image itself; it protects against
   corruption and tampering relative to the hash tree, not against a
   replaced image. Use a policy that requires `signed` and keep the
   verity.d directories out of reach. With a policy that allows both
   `signed` and `verity` a failed verification degrades to plain verity.
2. **No LUKS.** Encrypted partitions are recognized for policy checks
   (LUKS1 and LUKS2 count as both `encrypted` and `encryptedwithintegrity`)
   but never mounted.
3. **No forward error correction.** FEC data is ignored.
4. **Trust directory symlinks** are resolved on the host, not confined to
   `--root=`.
5. **Tampering may surface late**: verification happens per block on read.
