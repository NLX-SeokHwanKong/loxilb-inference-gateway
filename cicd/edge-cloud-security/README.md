# Edge Cloud security: portable UC-7 container testbed

This scenario builds the operator manual's seven-container topology on a dedicated Linux bare-metal host. It requires no private SSH alias, campaign image ID, pre-existing database, GPU, model download, or host Python package modification by the scenario.

It runs isolated XDP blacklist/source rate, TC/L4 firewall, shared-listener source policy, connection limits, L7 API-key/RPS, KV routing coexistence, all-on convergence, HTTP/2/SSE, database/snapshot recovery, shared-model deletion, REST/CLI readback and aggregate configuration examples. Successful validation includes owned cleanup. The advanced IPv6/fault-injection/kernel-cookie/large-map-pressure campaign is **not** silently included in this core replay.

## Topology and prerequisites

```text
A .50 (allowed, keyed) ─┐
B .51 (denied source) ──┤   uc7-gw .10:8080     uc7-ep .21:80
C .52 (allowed) ────────┼── XDP → TC/L4 → L7 ─→ HTTP/1.1 + h2c/SSE fixture
D .53 (TC drop case) ───┘         │                  │
                                └→ uc7-pg .30:5432 └→ synthetic KV events :5557
                 Docker bridge uc7-security: 172.30.97.0/24
```

Management and authenticated metrics use :11111 inside the Gateway (`/netlox/v1/metrics`); access them with `docker exec`. There are no host-published ports. The backend is a CPU echo/stream fixture, not a GPU inference engine. The publisher uses `nsenter` into its network namespace, with a private host venv and local tokenizer fixtures.

Use rootful Docker on a dedicated Linux host; root is needed for namespace capture. Ubuntu 24.04 amd64 / kernel 6.8 is the campaign reference, not a blanket qualification of other kernels/architectures. Allow at least 8 GiB RAM, 4 cores and 30 GiB free disk for a source Docker build. Install Docker Engine, git, Python 3 + venv, jq, OpenSSL, iproute2, util-linux (`nsenter`) and tcpdump. Follow [Docker's Ubuntu installation instructions](https://docs.docker.com/engine/install/ubuntu/). The build needs access to Docker registries, apt, Go/GitHub and PyPI. Containers share the host kernel; the Gateway is privileged for eBPF. Do not run this on a production host carrying unrelated critical traffic.

## Build, configure, validate, clean

From a **pinned** checkout with initialized submodules, enter this directory. Example shell session:

```bash
git clone --recurse-submodules https://github.com/loxilb-io/loxilb-inference-gateway.git
```

```bash
cd loxilb-inference-gateway
```

```bash
git checkout <full-reviewed-gateway-commit>
```

```bash
git submodule update --init --recursive
```

```bash
cd cicd/edge-cloud-security
```

```bash
sudo ./build.sh
```

```bash
sudo ./preflight.sh
```

```bash
sudo ./config.sh
```

```bash
sudo ./validation.sh
```

```bash
sudo ./rmconfig.sh
```

`build.sh` builds Gateway using the repository Dockerfile, a curl/HTTP2 client, and the scenario-specific h2c/SSE backend using the vendored reflect-echo HTTP/2 dependencies; it pulls PostgreSQL 16 and resolves every reference to an immutable local image ID. It installs pinned direct publisher dependencies into a private venv; transitive versions are recorded in `publisher-dependencies.txt`. Mutable upstream base images/package repositories mean a rebuild need not be byte-identical; preserve `images.json` or export the images to replay an exact tuple.

`preflight.sh` refuses name/subnet collisions, missing locked images, missing publisher dependencies, command-source drift after image locking and rootless Docker. It does not remove collisions. `config.sh` executes the first 36 setup commands and leaves the topology running. `validation.sh` continues the remaining 155 commands and six aggregate examples in one shell, checks 14 stage gates and 159 assertions, then removes the seven owned containers/network. It refuses a second validation of a consumed run. `rmconfig.sh` supports partial-failure cleanup; it checks all ownership labels and foreign network endpoints before deleting anything. It is idempotent after successful validation, preserves evidence/images/venv and never prunes Docker resources or volumes. PostgreSQL data uses a private run-specific bind directory (`lab/pgdata`) so stop/start preserves DB roles and schemas. Cleanup retains that sensitive directory for diagnosis; each new run gets a fresh database. Recovery also requires a fresh invalid key to return 401 without backend receipt, rather than accepting a cached valid-key 200 as proof. Three additional recovery assertions are saved in `recovery-validation.json`.

## Options and evidence

| Environment variable | Default / meaning |
|---|---|
| `EDGE_STATE` | `/var/tmp/loxilb-edge-security`; absolute private state/evidence root. Use the same value for every phase. |
| `EDGE_GATEWAY_IMAGE` | Unset: build from this checkout. Set: use an already-present, reviewed Gateway image ID/digest with XDP/TC, API-key DB, CLI and KV features. |
| `EDGE_CLIENT_IMAGE` | Unset: build `Dockerfile.client`; override must contain Bash, HTTP/2-capable curl, nc and iproute2. |
| `EDGE_BACKEND_IMAGE` | Unset: build `backend/Dockerfile` from the repository root; override must implement h2c/SSE and nonce/key receipt journal. |
| `EDGE_POSTGRES_IMAGE` | Unset: pull `postgres:16-bookworm`; explicit override must be already present. PostgreSQL 18 changes data-directory conventions; adapt the data-directory bind mount when changing major version. |

To supply options, use `sudo env EDGE_STATE=/var/tmp/my-uc7 EDGE_GATEWAY_IMAGE=sha256:... ./build.sh`; use the same `EDGE_STATE` for preflight/config/validation/cleanup. Image IDs are locked at build time; later phases do not pull tags. Keep default container names/IPs for direct comparison with the manual. Changing subnet requires changing **all** JSON source/endpoints, CLI assertions, publisher bind, addresses and checks in `commands.json`/`stage_gate.py`, not just Docker IPAM. Separate concurrent runs on one host are not supported by fixed names; use separate hosts.

Evidence: `EDGE_STATE/images.json`, `active.json`, and `runs/<run-id>/report/{score.json,execution-manifest.json}`. `commands.json` is the public, parameterized command source. Each executed `.command` and its SHA256 are recorded. Outputs, shell artifacts and `lab/` contain keys/tokens/passwords/capture/snapshots and remain mode 0700/0600. Do not publish them. `score.json` and the execution manifest summarize tests; inspect them before sharing.

Source limits and aggregate limits are different controls. `LOXILB_KV_COLDSTART_SEED_N=0` pins the test routing oracle; it does not change the product default. Global context threshold 16 includes backend contexts, is not a hard frontend connection count, and is not a tenant SLA. Tier-1.5 routing hits are not GPU cache-reuse proof. XDP `synCookies` is threshold telemetry, not cookie-handshake generation. IP-filter/firewall counters are collected asynchronously; bounded polling (30 seconds) waits for a positive valid sample while the rule remains installed, then the original traffic/counter gate still runs. Empty/error/zero responses do not become PASS. Delta calculations reject missing, negative and nonfinite samples. A controlled blacklist probe establishes an observed baseline before all-on measurement; an absent first-packet series is not silently converted to zero. Unknown/empty stages fail, and the complete score requires the exact 14 stages and 159 core checks.

## Failure handling

- Missing `ensurepip`: install the host's Python venv package, then recreate the incomplete **scenario-owned** venv; do not use pip's system-package override.
- Name/subnet collision: inspect the existing owner/routes. Choose another dedicated host or consistently modify the topology. No automatic deletion/readdressing occurs.
- The generated management password has uppercase/lowercase/digit/special characters plus fresh randomness, and rejects triple repetitions; hex-only passwords and unfiltered random triples violate the API policy.
- Setup or validation error: the run stops, helper processes are stopped with PID/start-time checks, containers remain for diagnosis. Read private `<command-id>.out`, then run `rmconfig.sh` and start a new `config.sh` run. A later rerun does not erase the failed evidence.
- API readiness, verifier or boot failure: inspect the private Gateway boot log and kernel version. Do not classify timeout alone as successful security blocking.
- Interrupted cleanup / foreign endpoint: cleanup refuses it. Remove only your explicitly identified extra lab endpoint before retrying; never `docker system prune` or broad `down -v`.

Run safety regressions without Docker: `python3 -m unittest discover -s . -p 'test_*.py'`. Runtime qualification remains separate from these unit tests.

The Security Sanity workflow runs the portable Python safety regressions on scenario changes. That CI step does not execute this full seven-container manual replay; run the phases above on your dedicated host and retain the actual execution manifest.
