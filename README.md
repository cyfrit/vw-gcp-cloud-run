# Vaultwarden on Cloud Run with durable SQLite

This image runs Vaultwarden against a local SQLite database and streams its WAL to Google Cloud Storage with Litestream. A small supervisor provides a GCS generation-based single-writer lease, restores the database before startup, and waits for remote replication before returning mutation responses.

The Cloud Storage FUSE mount is used only for Vaultwarden's non-database files: attachments, Sends, RSA keys, and `config.json`. The SQLite database, WAL, and shared-memory files always stay on the instance-local volume.

## License and upstream source

The original supervisor code in this repository is MIT-licensed; see [LICENSE](LICENSE). The published image is an aggregate, not a pure MIT artifact: it includes the unmodified [Vaultwarden](https://github.com/dani-garcia/vaultwarden) server under AGPL-3.0-only and [Litestream](https://github.com/benbjohnson/litestream) under Apache-2.0. The image labels and [third-party notices](THIRD_PARTY_NOTICES.md) record the exact upstream versions, immutable image digests, licenses, and corresponding-source locations.

If you modify Vaultwarden and let users interact with that modified version over a network, AGPLv3 section 13 requires you to offer those users its corresponding source. This repository does not modify Vaultwarden; its source is available at the exact upstream tag recorded in the notice.

## Build

The default build pins the current Vaultwarden and Litestream releases to their `linux/amd64` image manifests. [The third-party notice](THIRD_PARTY_NOTICES.md) records the exact versions and immutable digests. The scheduled updater changes the version, digest, labels, and notice together. For a manual upgrade, update that notice and pass both the version and full image reference.

```sh
docker build \
  --build-arg VAULTWARDEN_VERSION=VERSION \
  --build-arg VAULTWARDEN_IMAGE=docker.io/vaultwarden/server:VERSION@sha256:DIGEST \
  --build-arg LITESTREAM_VERSION=vVERSION \
  --build-arg LITESTREAM_IMAGE=docker.io/litestream/litestream:VERSION@sha256:DIGEST \
  -t REGION-docker.pkg.dev/PROJECT/REPOSITORY/vaultwarden:VERSION .
```

## Google Cloud prerequisites

Create a bucket in the same region as Cloud Run and a dedicated service account. Grant that service account `roles/storage.objectUser` on the bucket. Do not create or mount a service-account JSON key; Litestream and the supervisor use the Cloud Run service identity.

The GCS FUSE volume mounts only `vaultwarden/data`. Seed that prefix once before the first deployment so that `only-dir` can resolve it:

```sh
gcloud storage cp /dev/null gs://BUCKET/vaultwarden/data/.keep
```

The copied object is only a directory marker and can be removed after Vaultwarden has created persistent files.

## Deploy

Replace all `REPLACE_*` values in `deploy/cloud-run.yaml`, then apply it:

```sh
gcloud run services replace deploy/cloud-run.yaml --region REGION
```

The deployment intentionally configures: second-generation execution, instance-based CPU allocation, zero minimum instances, and a maximum of one instance. The GCS lease remains the correctness mechanism because Cloud Run can temporarily exceed the maximum during scaling or overlap revisions during deployment.

Set Vaultwarden secrets and optional settings through Cloud Run environment variables or Secret Manager. Do not put secrets into the checked-in YAML.

## Runtime behavior

On startup, the supervisor:

1. Acquires `gs://BUCKET/vaultwarden/control/lease.json` with a GCS generation precondition.
2. Starts Litestream, which restores the latest database when the local file is absent.
3. Starts Vaultwarden only after Litestream's control socket is ready.
4. Reports ready only after Vaultwarden answers its health endpoint.

For `POST`, `PUT`, `PATCH`, and `DELETE`, the proxy waits for `litestream sync -wait` semantics before forwarding Vaultwarden's response. If GCS replication cannot be confirmed, the client receives `503` instead of the upstream success response. A failed request can still have committed locally, so clients must retain their normal retry/idempotency behavior.

When a newer Cloud Run revision appears, it requests a handover through GCS. The current revision drains, syncs, releases the lease, and exits; the new revision then restores the latest remote transaction and becomes ready. Do not use a long-lived traffic split between revisions.

The lease prevents ordinary Cloud Run replica overlap and rolling-deployment split brain. It is not commit-level fencing against an arbitrarily long paused process that later resumes; that stronger guarantee would require integrating a fencing token into SQLite's commit path.

## Recovery check

To verify recovery without touching the live database, restore to a temporary local file:

```sh
litestream restore \
  -integrity-check quick \
  -o /tmp/vaultwarden-restore.sqlite3 \
  gs://BUCKET/vaultwarden/replica
```

Also test the real failure path periodically: complete a Vaultwarden write, confirm the client received success, terminate the Cloud Run instance, and verify the replacement instance contains the write.
