/*
 * Copyright (c) 2026 LoxiLB Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at:
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package common

// The values of a network-identity binding's enumerated fields. The first of
// each pair is what an omitted field means.
const (
	NetIdentityEnforceAdvisory = "advisory"
	NetIdentityEnforceStrict   = "strict"

	NetIdentityCredentialNone    = "none"
	NetIdentityCredentialNetwork = "network"

	NetIdentityFailNeutral = "neutral"
	NetIdentityFailClosed  = "closed"

	NetIdentityClassStandard  = "standard"
	NetIdentityClassCritical  = "critical"
	NetIdentityClassSheddable = "sheddable"
)

// NetIdentityMatch - what a binding matches a request's source on: exactly
// one of the fields. All but CIDR name an interface the connection arrived
// on; Vlan, Vni and IPsecTunnel are other spellings of IngressIf, resolved
// to an interface name each time the bindings are compiled.
type NetIdentityMatch struct {
	// CIDR - IPv4 networks ("10.20.0.0/24") the socket peer is in
	CIDR []string `json:"cidr,omitempty"`
	// IngressIf - name of the interface the connection arrived on. The
	// name is stored, never an index: an interface that is re-created keeps
	// its binding.
	IngressIf string `json:"ingress_if,omitempty"`
	// Vlan - a VLAN ID, 1 to 4094: the VLAN's bridge interface, vlan<id>
	Vlan int `json:"vlan,omitempty"`
	// Vni - a VXLAN network identifier, 1 to 16777215: the interface
	// vxlan<vni>
	Vni int `json:"vni,omitempty"`
	// IPsecTunnel - the name of an IPsec tunnel configured on this node:
	// its VTI interface, vti<mark>. A tunnel with no mark has none.
	IPsecTunnel string `json:"ipsec_tunnel,omitempty"`
}

// NetIdentityVlanMax and NetIdentityVniMax bound the two numeric aliases.
const (
	NetIdentityVlanMax = 4094
	NetIdentityVniMax  = 1<<24 - 1
)

// OnInterface reports whether the match is on an ingress interface, by
// name or by one of its aliases.
func (m *NetIdentityMatch) OnInterface() bool {
	return m.IngressIf != "" || m.Vlan != 0 || m.Vni != 0 || m.IPsecTunnel != ""
}

// NetIdentityMod - a network-identity binding: the tenant of the requests
// that reach an AI service from a network. A CIDR match says where a
// request came from and proves nothing about who sent it; an interface
// match is as strong as the network boundary behind the interface. A binding
// names interfaces and addresses of one node, so it is that node's
// configuration.
type NetIdentityMod struct {
	// Name - the binding's identity; what records carry
	Name string `json:"name"`
	// Match - the source the binding applies to
	Match NetIdentityMatch `json:"match"`
	// Services - "VIP:port" of the AI services the binding applies to;
	// empty means every AI service
	Services []string `json:"services,omitempty"`
	// Tenant - the tenant a matching request is attributed to; the same
	// namespace as API-key and JWT tenants
	Tenant string `json:"tenant"`
	// AllowedModels - the models a matching request may name; empty allows
	// any
	AllowedModels []string `json:"allowed_models,omitempty"`
	// Enforce - advisory (default): a credential decides and the binding is
	// noted; strict: a credential of another tenant is refused
	Enforce string `json:"enforce,omitempty"`
	// Credential - none (default), or network: a request with no credential
	// is admitted as the binding's tenant. Interface matches only.
	Credential string `json:"credential,omitempty"`
	// ForwardIdentity - forward the request's tenant upstream as
	// X-Auth-Tenant (and its user as X-Auth-User). The services the binding
	// applies to then drop every X-Auth-* header a client sends, on every
	// request, whether or not its source matched a binding.
	ForwardIdentity bool `json:"forward_identity,omitempty"`
	// PriorityClass - standard (default), critical or sheddable
	PriorityClass string `json:"priority_class,omitempty"`
	// FailMode - what the binding's services do with a request while the
	// resolver cannot answer: neutral (default) decides it as if no binding
	// existed; closed refuses it
	FailMode string `json:"fail_mode,omitempty"`
}
