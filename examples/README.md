# Example signed sysext

`signed-example.raw` is a genuine `systemd-repart --make-ddi=sysext` artifact:
a GPT DDI with root (erofs payload), root-verity (dm-verity hash tree) and
root-verity-sig (PKCS#7 signature) partitions. It doubles as documentation
and as the default interoperability fixture for `test/e2e/inner-signed.sh`.

> **The keys in `keys/` are public test keys, committed on purpose so anyone
> can verify and rebuild the example. Treat them as compromised; never sign
> anything real with them.**

Regenerate everything with `./make-signed-sysext.sh` (needs `openssl` and
`systemd-repart`, runs unprivileged).

## Try it

```sh
make build-static
sudo install -m0755 bin/sysext /usr/bin/sysext

sudo mkdir -p /var/lib/extensions /etc/verity.d
sudo cp examples/signed-example.raw /var/lib/extensions/
sudo cp examples/keys/db.pem /etc/verity.d/example.crt

sudo sysext --image-policy=root=signed merge
cat /usr/share/signed-example/hello.txt   # hello from signed-example
signed-example-tool                        # signed-example-tool
sudo sysext unmerge
```
