# Single-CPU Docker MCP lab

Use a Linux Docker host, an immutable candidate image that includes Python 3,
and the matching source checkout. The standard-library fixture is a test
backend, not a production MCP server. No GPU, Kubernetes or external database
is required. Host curl must support HTTP/2 for frontend H2 checks.

Reserve `172.30.100.0/24` and the `uc8-manual-*` names before provisioning.
Check Docker networks and host routes; replace the entire address plan if it
overlaps. Do not reuse an existing container or delete unrelated resources.

```text
Linux Docker host, all participants restricted to the same allowed CPU
  uc8-manual-net (bridge 172.30.100.0/24)
    gateway .10:11111 management; .10:2028 HTTP; .10:2029 frontend TLS/H2
    backend-a .21:8081 HTTP/1.1 JSON
    backend-b .22:8082 HTTP/1.1 SSE
    disposable client container -> gateway -> selected backend
```

Run the commands from the repository root. Set `IGW_IMAGE` to an image ID
from a qualified build and `UC8_STATE` to a new absolute directory. CPU 0 is
an example; select the same available CPU for every participant. The image
must provide Python 3; verify that prerequisite before starting. An image ID
is local daemon identity, not a registry address. For another node, import
the supplied image archive and verify its archive/image/binary hashes first.

```sh
export IGW_IMAGE=sha256:REPLACE_WITH_QUALIFIED_IMAGE_ID
```

```sh
export UC8_STATE=/tmp/uc8-container-lab
```

```sh
docker image inspect "$IGW_IMAGE" --format '{{.Id}}'
```

```sh
docker run --rm --entrypoint python3 "$IGW_IMAGE" --version
```

```sh
mkdir -p "$UC8_STATE/cert" "$UC8_STATE/config" "$UC8_STATE/journals"
```

Supply a CA and Gateway certificate/key with SAN `IP:172.30.100.10` under
`cert`. Keep private keys out of evidence exports. TLS tests must trust that
CA; do not disable verification. The config directory contains persisted
configuration and node secret state; container removal does not remove it.

```sh
docker network create --subnet 172.30.100.0/24 --label uc8.owner=uc8-manual uc8-manual-net
```

```sh
docker run -d --name uc8-manual-gw --label uc8.owner=uc8-manual --cpuset-cpus 0 --privileged --network uc8-manual-net --ip 172.30.100.10 -v "$UC8_STATE/cert:/opt/loxilb/cert:ro" -v "$UC8_STATE/config:/etc/loxilb" "$IGW_IMAGE" -p
```

```sh
docker run -d --name uc8-manual-backend-a --label uc8.owner=uc8-manual --cpuset-cpus 0 --network uc8-manual-net --ip 172.30.100.21 -v "$(pwd)/cicd/mcp-stateless:/opt/uc8:ro" -v "$UC8_STATE/journals:/evidence" --entrypoint python3 "$IGW_IMAGE" -B /opt/uc8/fixture.py --bind 0.0.0.0 --port 8081 --backend-id backend-a --journal /evidence/backend-a.jsonl --response-mode json
```

```sh
docker run -d --name uc8-manual-backend-b --label uc8.owner=uc8-manual --cpuset-cpus 0 --network uc8-manual-net --ip 172.30.100.22 -v "$(pwd)/cicd/mcp-stateless:/opt/uc8:ro" -v "$UC8_STATE/journals:/evidence" --entrypoint python3 "$IGW_IMAGE" -B /opt/uc8/fixture.py --bind 0.0.0.0 --port 8082 --backend-id backend-b --journal /evidence/backend-b.jsonl --response-mode sse
```

Only Gateway is privileged. No host port is published. The management API
is addressed on the lab bridge, separately from MCP user traffic. Wait for
management readiness and backend listeners before applying rules. `-p`
enables Gateway Prometheus; it is not Docker's publish flag. Sockmap is off
by default and must remain off for modern MCP validation.

```sh
curl --fail-with-body http://172.30.100.10:11111/netlox/v1/config/loadbalancer/all
```

```sh
curl --fail-with-body -X POST http://172.30.100.10:11111/netlox/v1/config/loadbalancer -H 'Content-Type: application/json' --data-binary '{"serviceArguments":{"externalIP":"172.30.100.10","port":2028,"protocol":"tcp","mode":4,"sel":0,"security":0,"sockMapMode":"off","mcp_protocol_version":"2026-07-28"},"endpoints":[{"endpointIP":"172.30.100.21","targetPort":8081,"weight":1},{"endpointIP":"172.30.100.22","targetPort":8082,"weight":1}]}'
```

```sh
docker run --rm --label uc8.owner=uc8-manual --cpuset-cpus 0 --network uc8-manual-net -v "$(pwd)/cicd/mcp-stateless:/opt/uc8:ro" --entrypoint python3 "$IGW_IMAGE" -B /opt/uc8/client.py --url http://172.30.100.10:2028/mcp --requests 12
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

Before cleanup verify each container/network has `uc8.owner=uc8-manual`.
Delete the configured test rules first. Preserve journals/config when
investigating failures; never prune Docker or remove other campaigns.

```sh
docker inspect uc8-manual-gw uc8-manual-backend-a uc8-manual-backend-b --format '{{.Name}} {{.HostConfig.CpusetCpus}} {{index .Config.Labels "uc8.owner"}}'
```

```sh
docker rm -f uc8-manual-gw uc8-manual-backend-a uc8-manual-backend-b
```

```sh
docker network rm uc8-manual-net
```
