# dm-verity support for GPT DDIs

`sysext-alpine` can authenticate the payload of GPT-partitioned extension
images (DDIs) with dm-verity, following the same conventions as
systemd-sysext / systemd-dissect: no configuration is carried outside the
image — the verity metadata is discovered from the partition table itself.

## How an image is dissected

For a raw GPT image, the partition table is parsed and partitions are
matched by **type GUID** per the UAPI Discoverable Partitions Specification
(root, root-verity, root-verity-sig, usr, usr-verity, usr-verity-sig — per
architecture; see `internal/image/gpt.go` for the full table). The root
partition is preferred; a usr-only image is mounted under `<root>/usr`.

The protection level of the selected payload is classified from the
partition list:

| Classification | Meaning |
|---|---|
| `absent` | no data partition of that type |
| `unprotected` | data partition, but no usable verity partition |
| `verity` | data partition + verity partition (both with non-zero unique GUIDs) |
| `signed` | `verity` + a verity-signature partition |

## Root-hash discovery

The verity **root hash** is not stored in a header — it is split across the
**unique** (per-partition) GUIDs, exactly like systemd does it:

- the data partition's unique GUID = the **first 128 bits** of the root hash,
- the verity partition's unique GUID = the **last 128 bits**,

both interpreted in the canonical textual UUID form (the form printed by
`sgdisk`/`sfdisk`), dashes stripped, concatenated. GPT stores GUIDs
mixed-endian on disk; the decoder (`guidString`) converts to canonical form
first, so the reconstruction operates on the textual representation. This
byte-order choice is locked in by a unit round-trip test
(`TestRootHashGUIDRoundTrip`) and asserted against `veritysetup`'s own root
hash in `test/e2e/inner-verity.sh`.

Because two GUIDs hold exactly 256 bits, only 32-byte digests work —
**sha256 only**. If either unique GUID is all-zero, no root hash can be
conveyed and the image is handled as `unprotected` (subject to policy).

The rest of the verity parameters come from the **verity superblock**
written by `veritysetup format` at offset 0 of the verity partition (magic
`verity\0\0`, version 1, hash type 1, algorithm, data/hash block sizes,
data block count, salt). The hash tree starts at hash block 1 (the
superblock occupies block 0).

## Device-mapper activation

No libdevmapper, no udev: the dm-verity device is created by issuing raw
ioctls on `/dev/mapper/control` with manually packed buffers
(`internal/image/verity.go`):

1. `DM_DEV_CREATE` — create device `sysext-<imagename>-verity`,
2. `DM_TABLE_LOAD` — single `verity` target:
   `<version> <datadev> <hashdev> <data_block_size> <hash_block_size>
   <data_blocks> 1 <algorithm> <roothash> <salt-hex>`,
3. `DM_DEV_SUSPEND` (without the suspend flag = **resume**) — activate.

The `struct dm_ioctl` layout (312 bytes) and ioctl numbers
(`0xc138fd03/04/06/09`) are pinned by unit tests against
`linux/dm-ioctl.h`. Since no udev runs on Alpine, `/dev/mapper/control` and
`/dev/mapper/<name>` are created via `mknod` when missing (the device
number is taken from the ioctl reply). The filesystem is then mounted
read-only from the dm device instead of the raw partition; unmounting
removes the dm device (`DM_DEV_REMOVE`) before detaching the loop device.

**Tamper behavior:** dm-verity verifies blocks on access. A tampered image
may still *mount* successfully (if the superblock area is intact); reads of
corrupted blocks then fail with `EIO`. Both outcomes — failed merge or
failing reads — mean the corruption cannot go unnoticed.

## Image policy

`--image-policy=` accepts a subset of `systemd.image-policy(7)`:

```
policy       := designator '=' alternatives (':' designator '=' alternatives)*
alternatives := protection ('+' protection)*
protection   := verity | signed | encrypted | unprotected | absent
```

- Only the `root` and `usr` designators are enforced; other designators
  (`esp`, `home`, ...) parse but are ignored.
- The empty policy (the default) is equivalent to
  `root=verity+signed+encrypted+unprotected+absent:usr=verity+signed+encrypted+unprotected+absent`
  — everything allowed.
- Designators not mentioned in a non-empty policy keep that allow-all
  default (systemd's `default`/`ignore`/`unused` fallback machinery is not
  implemented).

At mount time the image's actual protection (table above) is checked
against the allowed set for the used designator; protected access is
preferred: when a usable verity partition exists and the policy allows
`verity` (or `signed`), dm-verity is used. If the policy allows only
`unprotected`, the data partition is mounted directly even when verity data
is present. If the actual protection is not acceptable, mounting fails with
`image does not satisfy image policy`.

Bare-filesystem images (squashfs/erofs/ext4 without a partition table)
carry no verity metadata and classify as an `unprotected` root payload.

Examples:

```sh
sysext --image-policy=root=verity merge          # require verity for root payloads
sysext --image-policy=root=verity+unprotected merge
sysext --image-policy=root=signed merge          # currently always fails, see below
```

## Security limitations

These are deliberate scope limits — know them before relying on the
feature:

1. **Signature verification is NOT implemented.** Verity-signature
   partitions are *detected* (raising the classification to `signed`), but
   the PKCS#7 signature is never validated against any key. Consequences:
   - A policy whose only acceptable level is `signed` is rejected with
     `signature verification not implemented` rather than pretending to
     verify.
   - When the policy also allows `verity`, a signed image is treated as a
     plain verity image: the hash tree is enforced, the signature is
     ignored.
   - This means there is **no root of trust**: the root hash comes from the
     image itself (its partition GUIDs). dm-verity here protects against
     *accidental corruption* and *post-publication tampering of the data
     relative to its hash tree*, but an attacker who can replace the whole
     image file can simply re-create consistent verity metadata. Only
     signature verification against a trusted key (not implemented) would
     close that hole.
2. **Encrypted (LUKS) images are unsupported.** `encrypted` is accepted in
   policy strings but never matches; an image that would need it fails the
   policy check.
3. **sha256 only.** The GUID-based root-hash discovery cannot carry digests
   other than 256 bits; images using other algorithms are rejected when
   verity is attempted.
4. **No FEC.** Forward-error-correction data (optional in veritysetup) is
   ignored; corrupted blocks fail with `EIO` instead of being repaired.
5. **Policy subset.** Per-designator fallbacks (`default` pseudo-designator)
   and the `ignore`/`unused` semantics of systemd.image-policy(7) are not
   implemented; unlisted designators default to allow-all (see above).
6. **Tampering may surface late.** Because verification happens per block
   on read, a merge of a tampered image can succeed and only later reads
   fail with `EIO`.
