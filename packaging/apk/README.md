# Alpine packaging (apk)

APKBUILD for building `sysext` as an Alpine package. Produces two packages:

- `sysext` — `/usr/bin/sysext` (static Go binary) + `/usr/bin/confext` symlink
- `sysext-openrc` — `/etc/init.d/sysext` and `/etc/init.d/confext`
  (split automatically by abuild's `default_openrc`)

## Build locally with abuild

On an Alpine system (or container):

```sh
apk add alpine-sdk
adduser "$USER" abuild            # then re-login
abuild-keygen -a -i               # one-time: create and install signing key

cd packaging/apk
abuild checksum                   # fetch source tarball, fill in sha512sums
abuild -r                         # build + run tests; output in ~/packages/
```

Notes:

- The `source=` URL points at the GitHub release tag `v$pkgver`; the tag must
  exist for `abuild checksum` / `abuild -r` to fetch it. To build from a local
  tree instead, snapshot it yourself (`git archive --prefix=sysext-alpine-0.1.0/
  -o "$HOME/sysext-0.1.0.tar.gz" HEAD`) and place it in `$SRCDEST`.
- `options="net"` is set so `go build` can download modules during the build.

## Test install

```sh
apk add --allow-untrusted \
    ~/packages/apk/$(arch)/sysext-0.1.0-r0.apk \
    ~/packages/apk/$(arch)/sysext-openrc-0.1.0-r0.apk

sysext --version
confext status

rc-update add sysext default
rc-update add confext default     # optional
rc-service sysext start
```

(Omit `--allow-untrusted` if your abuild public key from `abuild-keygen -i`
is installed in `/etc/apk/keys/`.)
