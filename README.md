# Vaultwarden on Cloud Run with SQLite

This repository builds one container image containing [Vaultwarden],
[Litestream], and a small supervisor. It is intended for a personal
Vaultwarden deployment on [Google Cloud Run].

Vaultwarden uses SQLite on the Cloud Run instance-local memory volume.
SQLite, its WAL, and its shared-memory file are never placed on a Cloud
Storage FUSE mount. Litestream restores the database at startup and
replicates changes to a [Cloud Storage] bucket.

The supervisor also:

- holds a generation-checked single-writer lease in Cloud Storage;
- proxies requests to Vaultwarden;
- waits for remote replication before returning successful write responses;
  and
- drains requests, performs a final sync, and releases the lease during
  instance termination or revision replacement.

Cloud Storage FUSE is used only for non-database Vaultwarden files, including
attachments, Sends, RSA keys, and `config.json`.

This is a single-writer design. Keep Cloud Run `maxScale` set to `1`, and do
not use a long-lived traffic split between revisions. The lease handles the
temporary instance overlap that can occur during a deployment.

## Install

### Requirements

- A Google Cloud project with billing enabled.
- The [Google Cloud CLI] installed and authenticated.
- Docker with support for `linux/amd64` images.
- One Google Cloud region for both Cloud Run and Cloud Storage.

Set the values used by the commands below. Bucket names are globally unique.

```sh
export VW_PROJECT_ID=example-project
export VW_REGION=us-central1
export VW_BUCKET=example-vaultwarden-data
export VW_SERVICE_ACCOUNT=vaultwarden-cloud-run
export VW_ARTIFACT_REPOSITORY=containers
export VW_SERVICE_ACCOUNT_EMAIL="${VW_SERVICE_ACCOUNT}@${VW_PROJECT_ID}.iam.gserviceaccount.com"
export VW_IMAGE="${VW_REGION}-docker.pkg.dev/${VW_PROJECT_ID}/${VW_ARTIFACT_REPOSITORY}/vw-gcp-cloud-run:latest"

gcloud auth login
gcloud config set project "${VW_PROJECT_ID}"
```

### 1. Enable the required APIs

```sh
gcloud services enable \
  artifactregistry.googleapis.com \
  iam.googleapis.com \
  run.googleapis.com \
  secretmanager.googleapis.com \
  storage.googleapis.com
```

See [Enabling Google Cloud services] for permission requirements.

### 2. Build and push the image

Create a Docker repository in [Artifact Registry], configure Docker
authentication, and push the image.

```sh
gcloud artifacts repositories create "${VW_ARTIFACT_REPOSITORY}" \
  --repository-format=docker \
  --location="${VW_REGION}"

gcloud auth configure-docker "${VW_REGION}-docker.pkg.dev"
docker build --platform linux/amd64 --tag "${VW_IMAGE}" .
docker push "${VW_IMAGE}"
```

The Dockerfile pins the Vaultwarden and Litestream runtime images by version
and immutable `linux/amd64` digest.

### 3. Create the storage bucket

Create a bucket with uniform bucket-level access, then create the prefix used
by the FUSE volume.

```sh
gcloud storage buckets create "gs://${VW_BUCKET}" \
  --location="${VW_REGION}" \
  --uniform-bucket-level-access

gcloud storage cp /dev/null "gs://${VW_BUCKET}/vaultwarden/data/.keep"
```

The bucket stores both the Litestream replica and the files exposed through
the [Cloud Storage FUSE volume].

### 4. Create the runtime service account

```sh
gcloud iam service-accounts create "${VW_SERVICE_ACCOUNT}" \
  --display-name="Vaultwarden Cloud Run"

gcloud storage buckets add-iam-policy-binding "gs://${VW_BUCKET}" \
  --member="serviceAccount:${VW_SERVICE_ACCOUNT_EMAIL}" \
  --role=roles/storage.objectUser
```

Do not create or mount a service-account JSON key. The supervisor,
Litestream, and FUSE mount use the [Cloud Run service identity].

### 5. Configure the service manifest

Copy the supplied manifest outside the repository and edit that copy.

```sh
cp deploy/cloud-run.yaml /tmp/vaultwarden-cloud-run.yaml
```

Replace the following placeholders:

- `REPLACE_SERVICE_ACCOUNT` with the value of
  `VW_SERVICE_ACCOUNT_EMAIL`;
- `REPLACE_IMAGE_URL` with the value of `VW_IMAGE`;
- every `REPLACE_GCS_BUCKET` with the value of `VW_BUCKET`; and
- `REPLACE_DOMAIN` with the public hostname, without `https://`.

If a public hostname is not known yet, remove the `DOMAIN` environment entry
for the first deployment. Cloud Run assigns a `run.app` URL after the service
is created. Add that complete URL as the `DOMAIN` value and apply the manifest
again. A [custom domain] can be configured later.

Do not move `DATABASE_PATH` under `/mnt/vaultwarden-data`. That path is the
Cloud Storage FUSE mount and is not suitable for SQLite.

Store values such as `ADMIN_TOKEN` and SMTP credentials in [Secret Manager]
instead of the manifest. Other Vaultwarden settings can be added as normal
environment variables; see the [Vaultwarden configuration documentation].

### 6. Deploy

Apply the manifest with [`gcloud run services replace`].

```sh
gcloud run services replace /tmp/vaultwarden-cloud-run.yaml \
  --region="${VW_REGION}"
```

If Cloud Run assigned the hostname, retrieve it with:

```sh
export VW_PUBLIC_URL="$(gcloud run services describe vaultwarden \
  --region="${VW_REGION}" \
  --format='value(status.url)')"
```

Add this URL to the manifest as follows, then run `services replace` again:

```yaml
- name: DOMAIN
  value: https://SERVICE-HASH-REGION.run.app
```

Vaultwarden clients must be able to reach the Cloud Run endpoint without
Google Cloud IAM authentication. Grant `roles/run.invoker` to `allUsers`
unless an organization policy prevents public Cloud Run services.

```sh
gcloud run services add-iam-policy-binding vaultwarden \
  --region="${VW_REGION}" \
  --member=allUsers \
  --role=roles/run.invoker
```

See [Public access for Cloud Run]. Vaultwarden continues to perform its own
application authentication.

### 7. Verify

For a custom domain, set `VW_PUBLIC_URL` to its complete HTTPS URL. Then check
the supervisor endpoints.

```sh
curl --fail --silent --show-error "${VW_PUBLIC_URL}/_vwshim/healthz"
curl --fail --silent --show-error "${VW_PUBLIC_URL}/_vwshim/readyz"
```

After creating a test item in Vaultwarden, confirm that Litestream has written
replica objects.

```sh
gcloud storage ls --recursive "gs://${VW_BUCKET}/vaultwarden/replica"
```

Test independent recovery periodically with the [Litestream restore command].

## Runtime notes

- A successful `POST`, `PUT`, `PATCH`, or `DELETE` response is held until the
  supervisor confirms remote replication.
- A `503` can occur after Vaultwarden committed locally but before replication
  was confirmed. Clients must retain their normal retry behavior.
- The lease handles normal Cloud Run replica and revision overlap. It is not
  commit-level fencing for a process paused longer than the lease.

## License

The supervisor code in this repository is MIT-licensed. The built image is an
aggregate containing unmodified Vaultwarden under AGPL-3.0-only and
Litestream under Apache-2.0. It is not a pure MIT image. Exact versions, image
digests, source locations, and license copies are recorded in
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

[Artifact Registry]: https://cloud.google.com/artifact-registry/docs/repositories/create-repos
[Cloud Run service identity]: https://cloud.google.com/run/docs/securing/service-identity
[Cloud Storage]: https://cloud.google.com/storage/docs
[Cloud Storage FUSE volume]: https://cloud.google.com/run/docs/configuring/services/cloud-storage-volume-mounts
[custom domain]: https://cloud.google.com/run/docs/mapping-custom-domains
[Enabling Google Cloud services]: https://cloud.google.com/service-usage/docs/enable-disable
[Google Cloud CLI]: https://cloud.google.com/sdk/docs/install
[Google Cloud Run]: https://cloud.google.com/run/docs
[Litestream]: https://litestream.io/
[Litestream restore command]: https://litestream.io/reference/restore/
[Public access for Cloud Run]: https://cloud.google.com/run/docs/authenticating/public
[Secret Manager]: https://cloud.google.com/run/docs/configuring/services/secrets
[Vaultwarden]: https://github.com/dani-garcia/vaultwarden
[Vaultwarden configuration documentation]: https://github.com/dani-garcia/vaultwarden/wiki/Configuration-overview
[`gcloud run services replace`]: https://cloud.google.com/sdk/gcloud/reference/run/services/replace
