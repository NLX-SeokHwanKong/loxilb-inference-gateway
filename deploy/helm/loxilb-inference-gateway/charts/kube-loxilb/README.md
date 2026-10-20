loxilb's own CRDs (LoxiURL, BGPPeerService, BGPPolicy*, Egress), copied
verbatim from kube-loxilb `manifest/crds/` at branch
`integration/inference-gateway` (commit e69e52c). Helm installs what is in
`crds/` on first install and never upgrades or deletes it; re-copy when the
branch changes them. The example custom resources that sit next to them in
kube-loxilb are not CRDs and are not copied.
