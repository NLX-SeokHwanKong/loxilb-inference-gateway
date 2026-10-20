# loxilb-inference-gateway Helm chart

One release for the inference-gateway stack: the loxilb data plane
(`charts/loxilb-igw`, a DaemonSet), the kube-loxilb controller
(`charts/kube-loxilb`, branch `integration/inference-gateway`) and the Gateway
API objects that express the endpoint-selection policy (`templates/inference/`:
GatewayClass, Gateway, one InferencePool + HTTPRoute per `inference.pools[]`).

There is no separate Endpoint Picker pod: kube-loxilb does not delegate to one,
and loxilb selects endpoints itself (`epselect` chwbl / gpuaware / wrr-hash,
KV-exact, prefill/decode). The pool's `loxilb.io/*` annotations are the policy.

## Install

Prerequisites: Kubernetes >= 1.30, the Gateway API CRDs (v1.5.1, standard
channel) and the Inference Extension CRDs (v1.6.0). The pre-install hook checks
them and fails with the install commands when they are missing; set
`crds.installGatewayAPI=true` to have it apply them.

```bash
helm install igw deploy/helm/loxilb-inference-gateway -n kube-system \
  --set kube-loxilb.cidrPools.defaultPool=<VIP>/32 \
  --set kube-loxilb.image.repository=<image built from integration/inference-gateway> \
  --set kube-loxilb.image.tag=<tag>
helm test igw -n kube-system --logs
```

The VIP must be an address the loxilb node can bind (fullproxy binds a socket):
in the k3d testbed it is the node IP. The published `kube-loxilb:latest` does
not yet include the inference-gateway integration; build the branch
(`cicd/common/k8s-inference/k3d_common.sh` does).

## Values

| Key | What it does |
|---|---|
| `inference.gatewayClass`, `inference.gateway` | the GatewayClass and Gateway (namespace `llm` by default) |
| `inference.pools[]` | pod selector + targetPort, `modelName`, `epselect`, `sseMode`, free `annotations`, `auth` (rendered, not yet honoured by kube-loxilb), `route` |
| `loxilb-igw.*` | image, flags (`loxilb.*`), hostPath persistence for `/etc/loxilb` and the audit dir (its parent is mounted; loxilb creates the 0700 directory itself), optional trust ConfigMap at `/run/integration-ca`, management Service |
| `kube-loxilb.*` | image, `cidrPools`, `setRoles`, `loxiURL` (external loxilb), `extraArgs`, the wait-for-loxilb initContainer |
| `crds.*` | pre-install check, `installGatewayAPI` |
| `tests.*` | `helm test` images and the loxilb URL they call |

`values-k3d-dev.yaml` is the profile `cicd/k3d-helm-inference-gwapi` installs;
it reproduces `cicd/k3d-incluster-inference-gwapi` so that scenario's
`validation.sh` is the chart's oracle.

## Layout and phases

Design: `docs/internal/helm/helm-chart-design.md`. This is phase 1 (core).
Keycloak (JWT profiles), SIEM (audit sink) and PostgreSQL (API-key store)
subcharts, with the `igw-configure` hook that applies REST-side configuration,
come next. The subcharts are vendored as directories under `charts/`; do not
run `helm dependency update` against them.
