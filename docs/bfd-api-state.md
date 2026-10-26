# BFD list and lifecycle contract

`GET /config/bfd/all` lists configured BFD sessions. It does not enable keepalive, create sessions or start HA transitions.

| State | Response |
|---|---|
| BFD subsystem initialized, no sessions / keepalive not configured | `200` with `{"Attr":[]}` |
| Sessions configured, including a peer that is DOWN | `200` with the current session array |
| Last session deleted | `200` with `{"Attr":[]}` |
| BFD subsystem not initialized | `503` |
| BGP-only process without BFD support | `403` |
| Unexpected internal failure | Error response; never a fabricated successful empty list |

The session table is authoritative. The lifecycle running hint must not hide entries, and deleting one of several sessions must keep the remaining sessions readable and deletable. A query returning an empty array is not evidence that BFD packets are being exchanged or that an HA peer is healthy.

The gateway's BFD protocol is managed in process; the session list is not a readiness probe for an independently enabled `bfdd` service. The `--ka` startup option or session configuration determines whether keepalive sessions are configured. Mutating sessions on a shared host may affect HA state, so live lifecycle qualification should use a dedicated network namespace and loopback peers.

The wire format is unchanged. Known IPv6 remote-address parsing limitations and live HA failover qualification are outside this change.
