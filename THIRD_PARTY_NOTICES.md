# Third-party notices

The image published by this repository is an aggregate of separately licensed components. It must not be represented as a pure MIT image. This file records the direct components added or selected by this repository; the upstream runtime image can contain further operating-system and Web Vault dependencies with their own notices.

| Component | Version and immutable image | License | Corresponding source | Local license copy |
| --- | --- | --- | --- | --- |
| `vw-gcp-cloud-run` supervisor | This repository | MIT | https://github.com/cyfrit/vw-gcp-cloud-run | [`LICENSE`](LICENSE) |
| Vaultwarden server | `1.37.2`, `docker.io/vaultwarden/server:1.37.2@sha256:5d326778c22f063d093d6b0c9c766a28249561632266776f2c93132ab0ad3a80` | AGPL-3.0-only | https://github.com/dani-garcia/vaultwarden/tree/1.37.2 | [`licenses/VAULTWARDEN_AGPL-3.0.txt`](licenses/VAULTWARDEN_AGPL-3.0.txt) |
| Litestream | `v0.5.16`, `docker.io/litestream/litestream:0.5.16@sha256:837279d1279a90d670c915ddbb641b6e7b98bc42cc4dfb040eb569ee931078a1` | Apache-2.0 | https://github.com/benbjohnson/litestream/tree/v0.5.16 | [`licenses/LITESTREAM_LICENSE`](licenses/LITESTREAM_LICENSE) |

Vaultwarden is included unmodified. Its source is offered at the exact upstream tag above, and this repository's source supplies the supervisor, image recipe, deployment manifest, and automation used to build the published image. If a downstream changes Vaultwarden and offers that modified server for remote-network use, AGPLv3 section 13 requires an offer of the modified Vaultwarden corresponding source to those users.

The final image installs the three listed license documents and this notice under `/usr/share/licenses` and `/usr/share/doc/vw-gcp-cloud-run`, respectively.
