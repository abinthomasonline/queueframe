# Security policy

Queueframe stores encrypted broker credentials and can submit financially live
orders. Please report suspected vulnerabilities privately and do not test them
against another person's deployment or Zerodha account.

## Supported versions

Until the first tagged release, only the current `main` branch is supported.
After releases begin, security fixes will target the latest released version;
older versions may require upgrading rather than receiving a backport.

## Reporting a vulnerability

Use [GitHub's private vulnerability report form](https://github.com/abinthomasonline/queueframe/security/advisories/new).
If that form is unavailable, contact the repository owner privately through
their GitHub profile before sharing technical details. Do not open a public
issue for a suspected security vulnerability.

Include the affected version or commit, deployment mode, impact, and minimal
reproduction steps. Use synthetic values and redact all of the following:

- Kite API keys, API secrets, request tokens, and access tokens;
- Queueframe master keys, administrator credentials, invitation links, and
  session cookies;
- Zerodha client IDs, live order IDs, tags, symbols, quantities, prices, and
  account or holding information; and
- cloud credentials, private IP topology, database contents, backups, and TLS
  private keys.

If a real credential may have been exposed, revoke or rotate it immediately;
do not wait for the report to be investigated. Avoid placing, modifying, or
cancelling a live order as part of a proof of concept.

## Operational security

The deployment and security guides describe the supported trust boundary,
single-instance SQLite requirement, HTTPS modes, static-egress requirement,
secret sealing, backups, and update process. A vulnerability report is not a
substitute for following those controls.
