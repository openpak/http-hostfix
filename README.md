# http-hostfix

OpenPak's port-80 front.

| | |
| --- | --- |
| Role | Pass-through in front of the box's plain-HTTP entrypoint (Traefik `web`) |
| Language | Go 1.26, no dependencies |
| Image | `ghcr.io/openpak/http-hostfix` |
| Licence | AGPL-3.0-only |

Every Wii DWC title sends its NAS login with the `Host` header twice. Traefik's HTTP parser
answers that with `400 Bad Request`, so no Wii could sign in (openpak/prds#144). This service
drops repeated `Host` lines from each request head and otherwise streams bytes through
unchanged in both directions:

- request heads are parsed only to find repeated `Host` lines and the body framing
  (`Content-Length`, `chunked`); keep-alive, pipelining, `Expect: 100-continue`, `HEAD` and
  `204/304` responses work as before;
- `Upgrade`/`CONNECT` and anything that is not HTTP/1.x stream raw from the first byte;
- the upstream connection starts with a PROXY v2 header carrying the visitor's address, the way
  openpak-edge does, so Traefik still sees who is calling.

Port 443 does not pass through it.

## Logs

One line per request on stderr (collected into the per-service day files by openpak-logd):
`http METHOD HOST PATH -> STATUS Nms`, plus `(repeated Host dropped)` when it fixed one. Paths
are logged without their query string; bodies and other header values are never logged.

## Configuration

| Variable | Default | |
| --- | --- | --- |
| `HOSTFIX_LISTEN` | `:21090` | listen address |
| `HOSTFIX_UPSTREAM` | `127.0.0.1:31081` | Traefik's `web` entrypoint |
| `HOSTFIX_PROXY_PROTOCOL` | `v2` | anything else sends no PROXY header |

## Build and test

```sh
GOWORK=off go test -race ./...
docker build -t http-hostfix .
```

## Docs

- [Deploy](docs/deploy.md), including the cutover and the one-command rollback
- [Changelog](CHANGELOG.md)
