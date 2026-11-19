# gitforge

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/jacobcalvyn/gitforge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/jacobcalvyn/gitforge/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A standalone, single-process, GitHub-compatible git forge engineered for full protocol conformance and ultra-low-latency throughput over raw TCP HTTP/1.1 and Linux `epoll`.

## Surfaces

- `GET /healthz`: Trial liveness probe
- `/<owner>/<repo>.git/...`: Git smart HTTP (`upload-pack` protocol v2 & v0, `receive-pack` v0, pack v2 parser/writer, delta apply, shallow deepen/unshallow)
- `/api/v3/...`: GitHub REST v3 (repositories, branches, commits, contents, labels, issues, comments, pull requests, diffs, 3-way merges)
- `POST /api/graphql`: GitHub GraphQL v4 (viewer, repository, `__type` introspection, `node(id:)`, cursor pagination, mutations, operation batching)
- `POST /mcp`, `DELETE /mcp`: Streamable HTTP Model Context Protocol (`2026-07-28` `server/discover` and legacy `initialize` handshake)

## Architecture

- `main.ts`: Node 22 entrypoint launching the static Go core (`forge-linux-amd64` on Linux x86_64, `forge-local` on local dev).
- `cmd/forge`: Go process lifecycle (`MAKEFUN_ENV_URL` `/register` and idle-gated `/done` polling, or standalone `--listen` mode).
- `internal/http1`: Non-blocking HTTP/1.1 parser and writer with single-threaded Linux `epoll` loop (`serve_linux.go`), `TCP_NODELAY`, pooled buffers, chunked/`100-continue` support, and single-syscall writes.
- `internal/store`: Unified in-memory repository, slab-allocated git object, issue, comment, label, and PR store with atomic lock-free HTTP response caches.
- `internal/git`, `internal/rest`, `internal/graphql`, `internal/mcp`: Protocol handlers backed by the shared store.

## Run it

```sh
CGO_ENABLED=0 go build -o forge-local ./cmd/forge
./forge-local --listen 127.0.0.1:8080 --token test-token
```

---

<sub>Entered on [make.fun](https://make.fun/?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
