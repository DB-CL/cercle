# Changelog

## 1.1.0 (November 6, 2026)

- Everything translated to English: interface, messages, logs, comments, documentation.
- Shorter README; documentation folder removed.
- Internal rename of member records ("fiche" to "record"), with a database migration applied on start. Wire format unchanged: compatible with 1.0.0 nodes.

## 1.0.0 (November 6, 2026)

First release.

- Single Docker image (amd64, arm64, armv7, about 15 MB), WireGuard embedded in user space: no privileges, no kernel module.
- One-click circle creation, single-use invitation codes, Ed25519-signed member records, gossip, final revocation.
- Read-only sharing of the folders mounted under `/share`, selected in the settings; incremental index (500,000 files in 13 s).
- Persistent download queue with resume, retries, priority and pause; rate limits and time windows in both directions.
- Terminal-style web interface, password login page, live updates.
- Hardening: password always required, container without privileges or capabilities, path confinement on both sides, capped peer responses.
