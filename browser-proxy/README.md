# browser-proxy

TG-6 egress forward-proxy sidecar (docs/tg6-browser-protocol.md §5).
The ONLY network path out of the quarantine browser pod.

- Speaks forward-proxy HTTP: `CONNECT` tunnels + absolute-URI plain GET.
- Every dial goes through `shared/netguard`: dial-time SSRF floor with
  IP pinning (loopback/RFC1918/CGNAT/link-local/metadata/IPv6-ULA
  denied; a DNS answer set containing any blocked address denies the
  whole name — defeats rebinding).
- Port allowlist: 80, 443 only.
- No auth: same-pod sidecar; NetworkPolicy in `skquad-browser` ensures
  nothing else can reach it.
- Audit: one stdout line per decision —
  `ts=... target=... ip=... port=... allowed=t|f reason=...` (no payloads).

## Run

```bash
go run ./cmd/browser-proxy            # listens :8888 (PORT to override)
```

Chromium flag: `--proxy-server=http://127.0.0.1:8888`.

## Test

```bash
go vet ./... && go test ./... -count=1
```
