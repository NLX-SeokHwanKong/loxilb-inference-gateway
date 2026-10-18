# cicd/ai-tenant — tenant QoS: priority bands of the capacity gate

One self-contained bed for the band queue of the capacity admission gate:
classes resolved per tenant (`critical`, `standard`, `sheddable`), the band
knobs on the rule (`fc_wake_policy`, `fc_band_weights`,
`fc_band_min_share_pct`, `fc_critical_reserve_pct`, `fc_band_wait_ms`,
`fc_band_queue_pct`, `fc_shed_queue`, `fc_tenant_wake`, `fc_llmd_headers`),
and every way a request leaves the queue.

```
tqh1 (10.71.10.1) ---- tqllb (VIP 10.71.10.254) ---- tqep1 (10.71.31.1)
```

| Port | Rule |
| --- | --- |
| 8083 | keyed, ceiling 8, queue 16, wait 30 s: the scheduling pool (S, P, X, H cases) |
| 8086 | keyless, ceiling 1, queue 4, wait 5 s, `inactiveTimeOut` 2 s: a waiter is not idle (X11) |
| 8091 | keyless, ceiling 4, queue 8: knob replace, read-back and snapshot (P10, S8, B9) |
| 8092 | plain L7 (no AI gateway), `sockMapMode request`: created and deleted by B7 |
| 8087 | keyed, h2c backends `tqep1:9090/9091`, ceiling 2: streams never queue (P6, H3) |
| 8088 | keyed P/D (`lib/pd_mock.py` prefill `:8180`, decode `:8181`), ceiling 1, queue 4 (P9, B6) |
| 8089 | keyless stateless MCP (`../mcp-stateless/fixture.py` `:8201`), ceiling 1, queue 4 (B3a) |
| 8093 | stateless MCP with `api_key_auth`, ceiling 1, queue 4 (B3b) |
| 8094 | keyed CHWBL (`sel` 8), ceiling 8, endpoint ceiling 1, queue 16 (B5) |
| 8095, 8096 | keyed, one live and one closed endpoint / only the closed one (X4) |
| 8097 | keyed h2c, one live and one closed endpoint (H3) |

The scheduling pool (8083) is re-shaped per case by a gate-only replace
(`sched_cfg`: ceiling, depth, wait, band knobs), applied in place.

Every step-3 case reads the deltas of `loxilb_ai_fc_band_total` over itself
(the collector republishes every 10 s, so a case ends with an 11 s settle)
and asserts both identities of the counter: per band, admitted + shed +
expired + cancelled equals the band's requests; and the parks (`queued`, all
bands) equal the parks that ended: resumed (the queue-wait histogram's
count) + expired + evicted + cancelled.

Tenants (PostgreSQL store, `tenantq-pg-ai-tenant`): `t-crit` critical,
`t-a` / `t-b` / `t-c` standard, `t-shed` sheddable; one key each.

Run (from this directory, on the Linux bed, `LOXILB_DOCKER_IMAGE` set to a
`tenantq-` tag):

```
./config.sh && ./validation-sched.sh; ./rmconfig.sh
```

`validation-sched.sh` declares every case of the S, X and H series and the
P and B rows the band queue owns. A case is implemented in the change that
builds its behaviour; until then it reports `PENDING(<what it waits for>)`.
The summary compares declared against executed + pending: a declared case
that neither ran nor is pending fails the run, and with `REQUIRE_ALL=1`
(the PR gate) a pending case fails it too. A case declared `elsewhere:<scenario>` is run by that
scenario (B4: `cicd/vllm-kvcache-routing-cpu/b4-band-arm.sh`, where the KV
infrastructure is); it must not run here, and the PR gate requires its result
on the final image recorded beside this scenario's. Pass and fail are read from the
admission headers and `fc_effective`, never from timing alone.

`rmconfig.sh` removes the topology and checks that nothing it started is
left: containers, namespaces and helper processes.

Not wired into a GitHub workflow (a release item).
