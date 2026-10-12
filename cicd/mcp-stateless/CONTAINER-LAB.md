# Single-CPU Docker MCP lab

Use a Linux Docker host, an immutable candidate image that includes Python 3,
and the matching source checkout. The standard-library fixture is a test
backend, not a production MCP server. No GPU, Kubernetes or external database
is required. Host curl must support HTTP/2 for frontend H2 checks.

Run from the repository root on a Linux Docker host with Python 3, OpenSSL,
iproute2 and Docker. The matching immutable candidate image must include Python 3.

```text
Linux Docker host: Gateway, two backend containers, client restricted to CPU 0
  uc8-mcp-lab-net (bridge 172.30.100.0/24)
    gateway .10:11111 management; .10:2028 HTTP; .10:2029 frontend TLS/H2
    backend-a .21:8081 HTTP/1.1 JSON
    backend-b .22:8082 HTTP/1.1 SSE
```

```sh
./cicd/mcp-stateless/config.sh --image sha256:REPLACE_WITH_QUALIFIED_IMAGE_ID --state-dir /tmp/uc8-container-lab --cpu 0
```

```sh
./cicd/mcp-stateless/validation.sh --state-dir /tmp/uc8-container-lab
```

`config.sh` provisions only the topology. It generates a two-day lab CA and
Gateway certificate, starts three containers, and waits for management/backend
readiness. It never configures Gateway rules. Apply the rule and its arguments
manually using the operator manual or the example below.

| Provisioning option | Default and meaning |
|---|---|
| `--image` / `UC8_IMAGE` | Required qualified local image; resolved and recorded as an immutable image ID. |
| `--state-dir` / `UC8_STATE` | `/tmp/uc8-mcp-lab`; active manifest and fresh run directories for cert/config/journals. |
| `--prefix` | `uc8-mcp-lab`; names and a unique owner label. Existing names are refused. |
| `--subnet` | `172.30.100.0/24`; offsets 10/21/22; Docker/host route overlap is refused. Update manual addresses together. |
| `--cpu` | `0`; one available CPU for all three containers and validation client. |
| `--with-mcp-rule` | Validation only: checks an existing operator-created port 2028 rule and new backend receipts; never creates rules. |

Only Gateway is privileged. Source/cert mounts are read-only, host ports are
not published, and Gateway `-p` enables Prometheus. Sockmap remains off.
The fixture binds `0.0.0.0` inside its containers; its ID, port, response mode
and journal path are fixed by the topology manifest.

```sh
curl --fail-with-body -X POST http://172.30.100.10:11111/netlox/v1/config/loadbalancer -H 'Content-Type: application/json' --data-binary '{"serviceArguments":{"externalIP":"172.30.100.10","port":2028,"protocol":"tcp","mode":4,"sel":0,"security":0,"sockMapMode":"off","mcp_protocol_version":"2026-07-28"},"endpoints":[{"endpointIP":"172.30.100.21","targetPort":8081,"weight":1},{"endpointIP":"172.30.100.22","targetPort":8082,"weight":1}]}'
```

```sh
./cicd/mcp-stateless/validation.sh --state-dir /tmp/uc8-container-lab --with-mcp-rule
```

| Argument | Meaning and omission |
|---|---|
| `externalIP`, `port`, `protocol` | Required VIP, user port and `tcp`; not management port. |
| `mode`, `sel` | Modern mode requires FullProxy `4`, RR `0` or WRR `2`. |
| `security` | `0` HTTP, `1` frontend TLS; management TLS is separate. |
| `mcp_protocol_version` | Explicit modern opt-in. Omission/empty on create is legacy, null is refused; omitted replace preserves existing mode. |
| `sockMapMode` | Modern permits off only; every request must be validated. |
| `backend_protocol`, `egress` | Modern accepts omitted/http1 and omitted/false only. Frontend H2 bridges to H1. |
| `endpointIP`, `targetPort` | Backend address and listen port; independently configurable from the VIP. |
| `weight` | Relative WRR weight. Existing zero defaults to one; zero is not a drain switch. |
| fixture bind/port | Default loopback/8080; use container-local 0.0.0.0 and the specified ports. |
| backend ID/journal | ID correlates receipts; journal is required and append-only. Use fresh paths. |
| response mode | JSON by default; SSE is a finite streamed test response, not subscription qualification. |

Read back configuration and effective state, then correlate responses with
new `executed` journal receipts. Negative requests require zero total
`received`/`executed` journal delta. API HTTP 200 alone is not delivery proof.
Use the verifiers in [README](README.md) for L7, H2, FC, failure/cancellation
and SDK slices; those are separate from this provisioning smoke test.

For the full isolated campaign, `run_gateway.py --cpu 0` pins the runner,
its backend/client child processes, and Gateway container to that CPU.
The host can have more CPUs; this proves execution under one-core affinity,
not dedicated physical hardware performance. The campaign reports affinity
and refuses a CPU outside the runner's allowed affinity set.

Delete test rules manually, then remove only this manifest's owned resources:

```sh
./cicd/mcp-stateless/rmconfig.sh --state-dir /tmp/uc8-container-lab
```

Cleanup checks owner labels and refuses mismatches. It retains certificates,
config, manifests and journals for diagnosis; keep private keys/config secrets
out of evidence exports. Failed provisioning also cleans only recorded owned
resources. A subsequent configuration creates a fresh run directory. No Docker
prune or broad volume deletion is used. CPU affinity on a larger host is not
a dedicated one-vCPU hardware performance qualification.
