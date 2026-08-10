# Publishing and releasing Queueframe

The intended repository is `abinthomasonline/queueframe`. Publish it privately
first, review the exact tracked file set and CI results, and only then decide
whether to make the source and container package public.

## Repository metadata

Recommended GitHub values:

| Field | Value |
| --- | --- |
| Owner/name | `abinthomasonline/queueframe` |
| Visibility for first review | Private |
| Description | `Self-hosted household scheduler for one-shot Zerodha Kite Connect order placement.` |
| Topics | `go`, `zerodha`, `kite-connect`, `self-hosted`, `sqlite`, `docker`, `scheduled-orders` |
| Homepage | Leave empty; never publish the URL of a live household instance as the project homepage. |

The Go module path is `github.com/abinthomasonline/queueframe`. Forks do not
need to rewrite it merely to build the program, but a long-lived fork imported
as a Go module should use its own canonical path.

## Private-first checklist

Before the first push:

1. Confirm the repository is private and the default branch will be `main`.
2. Inspect every tracked candidate. In particular, do not commit `.env`,
   `deploy/.env`, SQLite/WAL files, process locks, backups, credentials,
   invitation URLs, order exports, cloud account inventories, or instance-specific
   project/domain/IP notes.
3. Run the full local checks:

   ```sh
   test -z "$(gofmt -l cmd internal)"
   go mod verify
   go mod tidy -diff
   go vet ./...
   go test ./...
   go test -race ./...
   ./deploy/test.sh
   docker build --check .
   docker build --tag queueframe:review .
   ```

4. Push the source without a version tag. Confirm the `CI` workflow passes on
   `main` before publishing any container.
5. Review GitHub's detected secret-scanning alerts and the repository's Actions
   logs. Build output and test failures must not print deployment secrets.
6. Keep the live deployment on a reviewed commit or immutable image digest;
   publishing the repository does not authorize an automatic production update.

The workflows require no repository secret. CI has read-only source access.
The tagged release job receives only `contents: read` and `packages: write` on
the repository-provided `GITHUB_TOKEN`. Third-party actions and container base
images are pinned by immutable digest or commit.

## License decision before public visibility

There is intentionally no project `LICENSE` yet. A visible repository without
a license is not open source and does not give others permission to deploy,
modify, or redistribute it.

For Queueframe's stated goal, **Apache License 2.0** is the recommended default:
it is permissive, includes an explicit patent grant, and allows families to
self-host without a network-source obligation. Add the unmodified Apache-2.0
text and record the copyright holder before making the repository public.

If the maintainer instead wants operators of modified hosted versions to offer
their corresponding source, choose **GNU AGPLv3** deliberately. That is a
material policy difference and should not be introduced merely as a default.
The bundled fonts and icons retain their own license files under
`internal/web/static/` regardless of the project license.

## First container release

Use a SemVer tag only after private source review. `v0.1.0` is an appropriate
first pre-1.0 release when compatibility and schema evolution are not yet
promised.

Pushing the tag runs `.github/workflows/release-container.yml`, repeats the
source checks, and publishes Linux `amd64` and `arm64` images to:

```text
ghcr.io/abinthomasonline/queueframe
```

The metadata workflow publishes version tags without the leading `v`, plus an
immutable source-SHA tag. The build also attaches an SBOM and BuildKit
provenance. After it succeeds:

1. Verify that both architectures are present and note the manifest digest.
2. Confirm the package is still private during private review.
3. Test a pull and start using the digest, not a moving tag:

   ```text
   ghcr.io/abinthomasonline/queueframe:0.1.0@sha256:DIGEST
   ```

4. Put that exact value in `QUEUEFRAME_IMAGE` and follow the stopped-backup and
   update procedure in [operations](operations.md). Do not rebuild a released
   tag with different content; publish a new version.

A private GHCR consumer must authenticate to `ghcr.io` with access to read the
package. The repository's release workflow authenticates with `GITHUB_TOKEN`;
no personal token should be stored as a repository secret for publishing.

The workflow publishes a container but does not currently create a GitHub
Release, sign the image with a maintainer identity, or generate human-written
release notes. Those can be added later without weakening the existing digest,
SBOM, and provenance controls.

## Public-release gate

Complete all of the following before changing visibility:

- add the selected project license and update the container/repository license
  metadata;
- enable GitHub private vulnerability reporting and verify the instructions in
  [`SECURITY.md`](../SECURITY.md);
- enable branch protection or a ruleset for `main`, requiring the `CI` checks
  and preventing force-pushes;
- confirm the repository contains no live hostnames, IP addresses, project IDs,
  order identifiers, client IDs, screenshots, databases, or deployment secrets;
- review external links and the Zerodha/Kite trademark and non-affiliation text;
- make the repository public; and
- separately change the GHCR package to public if anonymous self-hosters should
  be able to pull it.

Repository visibility and package visibility are separate controls. Verify both
from a signed-out browser before documenting anonymous installation.
