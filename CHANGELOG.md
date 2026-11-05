# Changelog

## v0.1.1 - 2026-11-05

- `deploy/switch.sh` first makes the INPUT chain accept the target port (live and in `rules.v4`). The box's INPUT chain ends in a REJECT, so the first cutover to 21090 refused every port-80 connection for about a minute until the rollback (`switch.sh off`) restored it.

## v0.1.0 - 2026-11-05

- First release (openpak/prds#144): port-80 pass-through that drops repeated `Host` lines from
  request heads (the Wii's NAS login sends two, which Traefik refused with 400) and streams
  everything else through, with a PROXY v2 header to Traefik's `web` entrypoint. Keep-alive,
  pipelining, chunked bodies, `Expect: 100-continue`, `HEAD`, `Upgrade`/`CONNECT` and non-HTTP
  traffic pass unchanged. One log line per request, never bodies or query strings.
- Deploy: pinned quadlet and `deploy/switch.sh on|off` (atomic cutover and rollback).
