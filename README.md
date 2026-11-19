# gitforge

A standalone, single-process, GitHub-compatible git forge engineered for high conformance and low-latency throughput over raw TCP HTTP/1.1.

## Surfaces
- `GET /healthz`: Trial liveness probe
- `/<owner>/<repo>.git/...`: Git smart HTTP (`upload-pack` protocol v2 & v0, `receive-pack` v0, pack v2 parser/writer, delta apply, shallow deepen/unshallow)
- `/api/v3/...`: GitHub REST v3 (repositories, branches, commits, contents, labels, issues, comments, pull requests, diffs, 3-way merges)
- `POST /api/graphql`: GitHub GraphQL v4 (viewer, repository, `__type` introspection, `node(id:)`, cursor pagination, mutations, operation batching)
- `POST /mcp`, `DELETE /mcp`: Streamable HTTP Model Context Protocol (2026-07-28 `server/discover` and legacy `initialize` handshake)

## Architecture
- `main.ts`: Node 22 entrypoint launching the compiled Go core (`forge-linux-amd64` on Linux x86_64, `forge-local` on local dev).
- `cmd/forge`: Go process lifecycle (`MAKEFUN_ENV_URL` `/register` and `/done` polling, or standalone `--listen` mode).
- `internal/http1`: Raw-TCP HTTP/1.1 parser and writer with `TCP_NODELAY`, pooled buffers, chunked/100-continue support, and single-syscall writes.
- `internal/store`: Unified thread-safe in-memory repository, git object, issue, comment, label, and PR store with atomic lock-free HTTP response caches.
- `internal/git`, `internal/rest`, `internal/graphql`, `internal/mcp`: Protocol handlers backed by the shared store.

## Run locally

```sh
go build -o forge-local ./cmd/forge
./forge-local --listen 127.0.0.1:8080 --token test-token
```
