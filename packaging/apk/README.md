# Alpine packaging (apk)

`APKBUILD` builds two packages:

- `sysext`: `/usr/bin/sysext` (static Go binary) and the `/usr/bin/confext`
  symlink
- `sysext-openrc`: `/etc/init.d/{sysext,confext}` and
  `/etc/conf.d/{sysext,confext}` (split off by abuild's `default_openrc`)

It needs Go 1.26 or newer (Alpine 3.24 ships it) and builds with the
packaged toolchain only (`GOTOOLCHAIN=local`).

## Build with abuild

On an Alpine system or container:

```sh
apk add alpine-sdk
adduser "$USER" abuild            # then log in again
abuild-keygen -a -i               # once: create and install a signing key

cd packaging/apk
abuild checksum                   # fetch the source tarball, fill in sha512sums
abuild -r                         # build and run the tests; packages end up in ~/packages/
```

`sha512sums` is left empty in the repository: the source is the GitHub tag
`v$pkgver`, which exists only once the release is published, so run
`abuild checksum` before the first build of a release.

To build from a local tree instead, put a snapshot named like the source into
`$SRCDEST` (by default `/var/cache/distfiles`) before running
`abuild checksum`:

```sh
v=$(. ./APKBUILD; echo "$pkgver")
git -C ../.. archive --prefix="sysext-alpine-$v/" -o "$SRCDEST/sysext-$v.tar.gz" HEAD
```

`options="net"` lets `go build` download the Go modules.

## Install and enable

```sh
apk add --allow-untrusted ~/packages/apk/$(arch)/sysext-*.apk

sysext --version
rc-update add sysext boot
rc-update add confext boot        # optional, read the notes below first
rc-service sysext start
```

Omit `--allow-untrusted` when the public key created by `abuild-keygen -i`
is installed in `/etc/apk/keys/`.

The services belong in the `boot` runlevel, like `systemd-sysext.service`
runs before `sysinit.target`: they start after `localmount` and `modules`
and before `bootmisc`, `sysctl`, `hostname` and `networking`, so services in
the `default` runlevel always see the merged extensions. Starting runs
`refresh` (it also works when something is merged already), `rc-service
sysext reload` refreshes after installing or removing images and stopping
unmerges (with `--no-reload` during shutdown, so that no services are
restarted then). The kernel command line options `systemd.sysext=0` and
`systemd.confext=0` turn the services into no-ops. Options go to
`SYSEXT_OPTS` and `CONFEXT_OPTS` in `/etc/conf.d/sysext` and
`/etc/conf.d/confext`.

### confext on Alpine

- `/etc/conf.d/confext` passes `--noexec=false`. systemd mounts merged
  confexts noexec, but OpenRC executes `/etc/init.d/*` (and `/etc/local.d`,
  `/etc/periodic`, network hooks) directly; with a noexec `/etc` no service
  can be started or stopped any more.
- A merged `/etc` is read-only. `rc-update`, `apk`, udhcpc writing
  `/etc/resolv.conf` and everything else writing to `/etc` fail while
  confexts are merged. Use a mutable mode in `CONFEXT_OPTS`
  (`--mutable=ephemeral`, or `--mutable=auto` with
  `/var/lib/extensions.mutable/etc`) on hosts that need that.
