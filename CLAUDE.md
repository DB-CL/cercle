# Cercle

Notes for a development agent (Claude Code or other).

- No Go toolchain needed on the machine: `deploy/go.sh` runs Go in Docker (`deploy/go.sh test -short ./...`, `deploy/go.sh vet ./...`, `deploy/go.sh run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...`).
- Before committing: empty `gofmt -l .`, `go vet`, `staticcheck`, tests; `deploy/e2e.sh` for any network or membership change.
- After a dependency change: `deploy/licenses.sh`.
- Everything is in English: code, comments, messages, interface, documentation, commits.
- Peer-provided text (names, paths) is always inserted as text in the interface, never as HTML.
- Never keep an SQLite transaction open across a network call: there is a single write connection, and a record update holding the circle lock would deadlock waiting for it.
- Wire constants (`cercle1/…` signing contexts, JSON field names of gossip tables and invitation codes) must not change: they keep nodes compatible across versions.
