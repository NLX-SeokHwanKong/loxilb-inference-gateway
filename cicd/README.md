
![build workflow](https://github.com/loxilb-io/loxilb/actions/workflows/docker-image.yml/badge.svg)  
![simple workflow](https://github.com/loxilb-io/loxilb/actions/workflows/basic-sanity.yml/badge.svg)   
[![tcp-lb-sanity](https://github.com/loxilb-io/loxilb/actions/workflows/tcp-sanity.yml/badge.svg)](https://github.com/loxilb-io/loxilb/actions/workflows/tcp-sanity.yml)   
[![udp-lb-Sanity](https://github.com/loxilb-io/loxilb/actions/workflows/udp-sanity.yml/badge.svg)](https://github.com/loxilb-io/loxilb/actions/workflows/udp-sanity.yml)   
[![sctp-lb-sanity](https://github.com/loxilb-io/loxilb/actions/workflows/sctp-sanity.yml/badge.svg)](https://github.com/loxilb-io/loxilb/actions/workflows/sctp-sanity.yml)   
![extlb workflow](https://github.com/loxilb-io/loxilb/actions/workflows/advanced-lb-sanity.yml/badge.svg)   
![ipsec workflow](https://github.com/loxilb-io/loxilb/actions/workflows/ipsec-sanity.yml/badge.svg)  
![scale workflow](https://github.com/loxilb-io/loxilb/actions/workflows/scale-sanity.yml/badge.svg)   
[![k3s-flannel](https://github.com/loxilb-io/loxilb/actions/workflows/k3s-flannel.yml/badge.svg)](https://github.com/loxilb-io/loxilb/actions/workflows/k3s-flannel.yml)   
[![k3s-calico-cluster](https://github.com/loxilb-io/loxilb/actions/workflows/k3s-flannel-cluster.yml/badge.svg)](https://github.com/loxilb-io/loxilb/actions/workflows/k3s-flannel-cluster.yml)  
[![k3s-calico](https://github.com/loxilb-io/loxilb/actions/workflows/k3s-calico.yml/badge.svg)](https://github.com/loxilb-io/loxilb/actions/workflows/k3s-calico.yml)   
[![k0s-weave](https://github.com/loxilb-io/loxilb/actions/workflows/k0s-weave.yml/badge.svg)](https://github.com/loxilb-io/loxilb/actions/workflows/k0s-weave.yml)   
[![liveness-sanity](https://github.com/loxilb-io/loxilb/actions/workflows/liveness-sanity.yml/badge.svg)](https://github.com/loxilb-io/loxilb/actions/workflows/liveness-sanity.yml)   
[![K3s-Base-Sanity-CI](https://github.com/loxilb-io/loxilb/actions/workflows/k3s-base-sanity.yml/badge.svg?branch=main)](https://github.com/loxilb-io/loxilb/actions/workflows/k3s-base-sanity.yml)    

## CLI validation halves

Five scenarios end with a `validate_cli.sh` that drives the packaged
loxicmd-inference-gateway CLI as the subject under test, with the REST API
as the oracle, and fold its exit code into the scenario's verdict. Each is
gated by the `CLI_TESTS` knob of `common.sh`: `auto` (the default) skips the
half when the image's loxicmd lacks the surface, `required` fails it
instead, `skip` skips it; `run_local_cicd.sh` runs with `required`.

| scenario | preflight | what the CLI half covers |
| --- | --- | --- |
| `ai-apikey` | `cli_preflight` | API keys and tenant rate limits: create, get, set, delete |
| `ai-model-routing` | `cli_preflight` | model-keyed rules, delete by the full L7 key |
| `ai-sse-quota` | `cli_preflight` | the SSE tuning flags on a rule |
| `ai-tenant` | `bands_cli_preflight` | `get lbversion`; the scenario's rules, objective, tenant rate limits and API keys re-created through loxicmd and compared field by field; the nine band knobs (P10), the refused combinations (S8), `get lb --bands` under load (R1), a netidentity binding |
| `ai-jwtauth` | `jwtauth_cli_preflight` | a JWT auth profile with every field the scenario sets plus the priority cap: create, `get` (default, `-o wide`, `-o json`), 409 while a rule references it, delete, 404 on a second delete |

The gateway advertises only `api_key_auth` and `jwt_auth_profile` in
`/version`; a band, objective or netidentity command prints one advisory
line on stderr and sends the request, and the CLI halves assert that the
line never moves an exit code.
