# BFD list and lifecycle contract

`GET /config/bfd/all` lists configured BFD sessions. It does not enable keepalive, create sessions or start HA transitions.

| State | Response |
|---|---|
| BFD subsystem initialized, no sessions / keepalive not configured | `200` with `{"Attr":[]}` |
| Sessions configured, including a peer that is DOWN | `200` with the current session array |
| Last session deleted | `200` with `{"Attr":[]}` |
| BFD subsystem not initialized | `503` |
| BGP-only process without BFD support | `400` |
| Unexpected internal failure | Error response; never a fabricated successful empty list |

The session table is authoritative. The lifecycle running hint must not hide entries, and deleting one of several sessions must keep the remaining sessions readable and deletable. A query returning an empty array is not evidence that BFD packets are being exchanged or that an HA peer is healthy.

The gateway's BFD protocol is managed in process; the session list is not a readiness probe for an independently enabled `bfdd` service. The `--ka` startup option or session configuration determines whether keepalive sessions are configured. Mutating sessions on a shared host may affect HA state, so live lifecycle qualification should use a dedicated network namespace and loopback peers.

The wire format is unchanged. Known IPv6 remote-address parsing limitations and live HA failover qualification are outside this change.

BGP-only mode returns `400` consistently for GET, POST and DELETE, following the existing unavailable-in-mode convention. It is not an RBAC denial (`403`). Uninitialized BFD returns `503` on all three operations. Snapshot capture/wipe recognize both typed BFD sentinels, including wrapped errors, and skip unavailable BFD; restore/apply of supplied BFD configuration still fails rather than silently dropping it. Other subsystem legacy error matching remains unchanged.

## Known concurrency follow-ups

This change preserves the read path's release-before-table-lock ordering. It does not resolve the pre-existing add/delete lock inversion: API mutation holds the management lock before the BFD table lock, while protocol notification can take them in the reverse order. An unbuffered session `Fin` send can also wait on a ticker blocked in notification. A peer-active delete requires separate lock/lifecycle work and timeout/deadlock tests before claiming that issue fixed.

`CISpawn` also publishes the BFD subsystem pointer without the lock used by readers. Startup publication must be synchronized in a separate change with a concurrent initialization regression. The focused/gated-session race tests here do not qualify those startup or live-peer paths as race-free.
