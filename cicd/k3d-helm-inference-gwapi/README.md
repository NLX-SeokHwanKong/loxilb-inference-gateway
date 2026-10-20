# k3d-helm-inference-gwapi — the Helm chart against the InferencePool oracle

[`k3d-incluster-inference-gwapi`](../k3d-incluster-inference-gwapi) with one
substitution: loxilb, kube-loxilb and the gateway objects are installed by
`helm install` of [`deploy/helm/loxilb-inference-gateway`](../../deploy/helm/loxilb-inference-gateway)
with `values-k3d-dev.yaml`, not by `kubectl apply` of the testbed manifests and
fixtures. Cluster, images, mock vLLM servers and the Gateway API / Inference
Extension CRDs still come from [`common/k8s-inference`](../common/k8s-inference).

`validation.sh` runs `helm test` (the chart's own pods: management REST flavor,
InferencePool → Service → loxilb rule) and then sources the original scenario's
`validation.sh` unchanged. The same eight assertions passing is the definition
of "the chart is equivalent to the testbed". `fixtures/` is a link to the
original fixtures because checks 5–7 apply a FailClose/FailOpen pool by hand.

```bash
./config.sh       # cluster, images, mocks, CRDs, client, helm install --wait
./validation.sh   # helm test + k3d-incluster-inference-gwapi assertions
./rmconfig.sh     # helm uninstall, then the common teardown
```

Knobs: everything `k3d_common.sh` takes (`IGW_IMAGE`, `KLB_IMAGE`, `KLB_REF`, ...),
plus `HELM_RELEASE` (default `igw`) and `HELM_NAMESPACE` (default `kube-system`).
Host requirements: those of the common testbed plus `helm`.
