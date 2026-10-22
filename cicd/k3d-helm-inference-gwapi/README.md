# k3d-helm-inference-gwapi — the Helm chart against the InferencePool oracle

[`k3d-incluster-inference-gwapi`](../k3d-incluster-inference-gwapi) with one
substitution: loxilb, kube-loxilb and the gateway objects are installed by
`helm install` of [`deploy/helm/loxilb-inference-gateway`](../../deploy/helm/loxilb-inference-gateway)
with `values-k3d-dev.yaml`, not by `kubectl apply` of the testbed manifests and
fixtures. Cluster, images, mock vLLM servers and the Gateway API / Inference
Extension CRDs still come from [`common/k8s-inference`](../common/k8s-inference).

`validation.sh` runs `helm test` (the chart's own pods: management REST flavor,
InferencePool → Service → loxilb rule), then sources the original scenario's
`validation.sh` unchanged — the same eight assertions passing is the definition
of "the chart is equivalent to the testbed" — and then walks the release through
its lifecycle: an in-place `helm upgrade` (revision 2, the pre-upgrade hook runs
again, no pod restarts, the rule and the traffic stay), a `helm uninstall` that
has to leave nothing but the CRDs (release resources, the gateway objects, the
Service kube-loxilb derived from the pool, the helm test pods all gone), and a
`helm install` into the same cluster that brings the rule and the traffic back.
`fixtures/` is a link to the original fixtures because checks 5–7 apply a
FailClose/FailOpen pool by hand.

```bash
./config.sh       # cluster, images, mocks, CRDs, client, helm install --wait
./validation.sh   # helm test + k3d-incluster-inference-gwapi assertions + upgrade/uninstall/reinstall
./rmconfig.sh     # helm uninstall, then the common teardown
```

Knobs: everything `k3d_common.sh` takes (`IGW_IMAGE`, `KLB_IMAGE`, `KLB_REF`, ...),
plus `HELM_RELEASE` (default `igw`) and `HELM_NAMESPACE` (default `kube-system`).
Host requirements: those of the common testbed plus `helm`.
