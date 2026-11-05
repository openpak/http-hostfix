# Deploy: http-hostfix

Public TCP port 80 on the box is an iptables `REDIRECT` in `nat PREROUTING`. Before this
service it went to openpak-edge (`21081`), which relays to Traefik's `web` entrypoint
(`127.0.0.1:31081`) with PROXY v2. With it, port 80 goes to http-hostfix (`21090`), which
relays to the same Traefik entrypoint with PROXY v2 itself. openpak-edge, Traefik and port 443
are not touched.

```
before:  :80 -> 21081 openpak-edge (pasta, PROXY v2) -> 127.0.0.1:31081 Traefik web
after:   :80 -> 21090 http-hostfix (pasta, PROXY v2) -> 127.0.0.1:31081 Traefik web
```

Traefik's `web` entrypoint trusts PROXY v2 from `10.89.2.96`, the address every connection to
the host's `127.0.0.1:31081` arrives from, so no Traefik change is needed.

## Install

1. Copy `deploy/openpak-http-hostfix.container` to `~/.config/containers/systemd/`, set the
   image tag, `systemctl --user daemon-reload && systemctl --user start openpak-http-hostfix`.
   The image tag is pinned on purpose: no auto-update in front of all port-80 traffic.
2. Check it directly: `curl -s -H 'Host: conntest.openpak.org' http://127.0.0.1:21090/`.

## Cutover and rollback

`deploy/switch.sh` takes `~/openpak-deploy.lock`, makes sure the INPUT chain accepts the
target port (the chain ends in a REJECT; without this the redirected traffic is refused), moves
every port-80 rule in one
`iptables-restore` commit and makes the same one-line edit in `/etc/iptables/rules.v4` (a
backup is taken first).

```sh
deploy/switch.sh on    # port 80 -> http-hostfix
deploy/switch.sh off   # rollback: port 80 -> openpak-edge, as before
```

Before and after a switch, run the route check (every plain-HTTP router on Traefik's `web`
entrypoint, plus a duplicate-Host request, keep-alive, HEAD, 100-continue, chunked and a
1 MiB upload) and compare: statuses must match, and the duplicate-Host request goes from 400
to 200.

## Updating

Pin the new tag in the quadlet, `daemon-reload`, `systemctl --user restart
openpak-http-hostfix`. New connections wait for the few hundred milliseconds of the restart;
for a longer change, switch off first.
