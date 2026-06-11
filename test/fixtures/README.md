# Real-image test fixtures (local only — never committed)

Everything in this directory except this README is gitignored: fixtures may
contain private keys and large images.

Drop a systemd-built signed extension DDI here to activate the
interoperability test in `test/e2e/inner-signed.sh`:

- `signed-<name>.raw` — GPT DDI with root + root-verity + root-verity-sig
  partitions, e.g. produced by:

  ```sh
  systemd-repart -S -s SOURCE_DIR signed-example.raw \
      --private-key=db.key --certificate=db.pem
  ```

- the signer certificate as `*.crt` or `*.pem` (PEM; `db.pem` preferred when
  several are present).

Behavior in the suite:
- valid certificate → the fixture must merge under `--image-policy=root=signed`
- expired certificate → it must be rejected under `root=signed` and degrade
  to plain dm-verity (with a warning) under `root=signed+verity`

The merge runs with `--force` since CI signing fixtures often carry no
`extension-release` payload.
