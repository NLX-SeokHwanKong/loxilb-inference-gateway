loxilb's own CRDs (LoxiURL, BGPPeerService, BGPPolicy*, Egress, JWTAuthProfile), copied
verbatim from kube-loxilb `manifest/crds/` at branch `feat/auth-annotation`
(commit 830fa59; the six older CRDs are unchanged since `integration/inference-gateway`
e69e52c). Helm installs what is in `crds/` on first install and never upgrades or
deletes it; re-copy when the branch changes them. The example custom resources that
sit next to them in kube-loxilb are not CRDs and are not copied.

`JWTAuthProfile` (cluster-scoped, `jwtauthprofile.loxilb.io/v1`) is what kube-loxilb
registers on every gateway that advertises `jwt_auth_profile`; a rule whose
`loxilb.io/jwt-auth-profile` names a profile is held until that profile is programmed.
Without the CRD kube-loxilb turns profile management off and the rules still go out,
but every profile has to be registered on every gateway by hand.
