# Flotilla

> A fleet of Tailscale exit-node proxies in a single binary.
> One SOCKS5 port + one HTTP CONNECT port in front of N independent Tailnet
> nodes — round-robin, sticky, tag-routed, with health probes and Prometheus
> metrics. Built for scrapers that need clean IP rotation without juggling
> N daemons.

```
      ┌────────── flotilla ─────────┐
SOCKS ─┤ :5040  ──┐                 │
HTTP  ─┤ :5050  ──┼─ Pool.Pick ─┐   │
admin ─┤ :8080    │             │   │
      └───────────┼─────────────┼───┘
                  │             │
   ┌──────────────┴───────┐ ┌───┴──────────┐
   │ Worker tr-1          │ │ Worker eu-1  │
   │ tsnet, exit=tr-ist-1 │ │ exit=eu-ams  │
   └──────────────────────┘ └──────────────┘
```

Flotilla is a fork of [ItalyPaleAle/tailsocks][upstream] that adds:

- **Pool mode**: N workers in one process, each a fully independent
  `tsnet.Server` pinned to its own exit node.
- **Strategies**: `round_robin`, `random`, `sticky`, `least_active`, `tagged`.
- **HTTP CONNECT** dispatcher alongside SOCKS5 (use header `X-Flotilla-Node`
  or `X-Flotilla-Tags` to influence routing per-request).
- **Health probes** + per-node egress IP discovery.
- **Prometheus metrics** at `/metrics` and a JSON admin API.
- **YAML config** with sane defaults; legacy single-node CLI flags retained.

[upstream]: https://github.com/ItalyPaleAle/tailsocks

---

## Quick start (Docker Compose)

```bash
# 1) grab the compose file
curl -O https://raw.githubusercontent.com/mamidevs/flotilla/main/docker-compose.example.yaml
mv docker-compose.example.yaml docker-compose.yaml

# 2) drop a config in place (edit the node list to your exit nodes)
docker run --rm ghcr.io/mamidevs/flotilla:latest init > flotilla.yaml

# 3) provide auth — either OAuth2 creds or a static key
echo '{"client_id":"...","client_secret":"tskey-client-...","tag":"flotilla"}' > oauth2.json
# or:  echo "TS_AUTHKEY=tskey-..." > .env

# 4) up you go
docker compose up -d
curl --socks5 127.0.0.1:5040 https://api.ipify.org   # → exit-node IP 1
curl --socks5 127.0.0.1:5040 https://api.ipify.org   # → exit-node IP 2 (round-robin)
curl http://127.0.0.1:8080/health/nodes | jq
```

## Quick start (Go)

```bash
go install github.com/mamidevs/flotilla/cmd/flotilla@latest
flotilla init -o flotilla.yaml
$EDITOR flotilla.yaml
TS_AUTHKEY=tskey-... flotilla run --config flotilla.yaml
```

## Single-node (tailsocks-compatible)

If you just want the original tailsocks behavior on this machine, all the
classic flags still work:

```bash
flotilla run --exit-node home-server --socks-addr 127.0.0.1:5040
```

## Routing hints

| Method | Selector | Example |
|---|---|---|
| HTTP CONNECT pin | `X-Flotilla-Node` header | `curl -x http://127.0.0.1:5050 -H "X-Flotilla-Node: tr-1" https://api.ipify.org` |
| HTTP CONNECT filter | `X-Flotilla-Tags` header | `-H "X-Flotilla-Tags: region:tr"` |
| HTTP sticky override | `X-Flotilla-Client` header | `-H "X-Flotilla-Client: scraper-42"` |
| SOCKS5 pin | RFC1929 user field | `curl --socks5 'node=tr-1:anything@127.0.0.1:5040' …` |
| SOCKS5 filter | RFC1929 user field | `curl --socks5 'tags=region:tr:any@127.0.0.1:5040' …` |
| SOCKS5 sticky | source IP (default) | implicit per client |

## Strategies

| `dispatch.strategy` | Behavior |
|---|---|
| `round_robin` | Atomic counter mod N, deterministic. The default. |
| `random` | Uniform random pick. |
| `sticky` | `fnv1a(ClientID) % N`. Pin a client to one worker; consistent across reconnects. |
| `least_active` | Pick the worker with the fewest in-flight connections. |
| `tagged` | Pool first filters by request tags, then round-robins the survivors. |

Pin / tag hints survive every strategy. A `node=tr-1` hint always wins.

## Endpoints

| Port | Purpose |
|---|---|
| `:5040` | Dispatcher SOCKS5 (RFC1928) — picks a worker per request. |
| `:5050` | Dispatcher HTTP CONNECT + forward proxy. |
| `:5041+` | Optional per-worker direct SOCKS5 listeners (configurable). |
| `:8080/health` | 200 if at least one worker is healthy. |
| `:8080/health/nodes` | JSON of every worker's status. |
| `:8080/ip` | Per-node public egress IP map. |
| `:8080/ip?node=X` | One node. |
| `:8080/metrics` | Prometheus scrape. |

## Prometheus metrics

```
flotilla_requests_total{node, protocol, result}
flotilla_request_duration_seconds{node, protocol}
flotilla_active_connections{node}
flotilla_node_up{node}
flotilla_node_egress_ip_info{node, egress_ip}
flotilla_pool_pick_total{strategy, node, result}
```

## What's different from tailsocks?

| Feature | tailsocks | flotilla |
|---|---|---|
| Run mode | one binary, one exit | one binary, **N** exits |
| Dispatch | n/a | round_robin / random / sticky / least_active / tagged |
| HTTP CONNECT proxy | ✗ | ✓ |
| YAML config | ✗ | ✓ (CLI flags still work for single-node) |
| Per-request worker pinning | ✗ | ✓ (header or SOCKS5 user) |
| Health probes | ✗ | ✓ |
| Prometheus metrics | ✗ | ✓ |
| OAuth2 + ephemeral keys | ✓ | ✓ (inherited verbatim) |
| MagicDNS / split-DNS resolver | ✓ | ✓ (inherited verbatim) |

If you want plain "one client → one exit node," **stick with upstream
tailsocks** — Flotilla is the choice when you need a pool.

## Building

```bash
make build           # ./flotilla
make test            # go test -race ./...
make lint            # golangci-lint run
make docker          # local container image
```

## Credits

Flotilla is a fork of [`ItalyPaleAle/tailsocks`][upstream] (MIT). The
upstream code carries the heavy lifting:

- `internal/auth` — OAuth2 client-credentials → ephemeral auth key flow.
- `internal/resolver` — MagicDNS / split-DNS resolver for SOCKS5.
- `internal/worker` — `tsnet.Server` lifecycle + `SetExitNodeIP` glue.

See [`NOTICE`](./NOTICE) for the full attribution.

## License

[MIT](./LICENSE.md).
