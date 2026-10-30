/*
 * Copyright (c) 2026 NetLOX Inc
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

package tools

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// IPsec reads: the global configuration, one tunnel, the remote-peer
// configuration a tunnel generates, and certificate metadata. Each tool
// performs one GET; none sends a mutation. The pre-shared key a peer
// configuration carries is masked by the bridge, and certificates are
// decoded into named metadata fields only, so no PEM or key reaches the
// result whatever the gateway sends.

// ---- ipsec_config_get ----

type ipsecConfigGetOut struct {
	Target string `json:"target"`
	Config any    `json:"config" jsonschema:"fast path, hardware offload, anti-replay, SA lifetime warning, sequence overflow action, MTU and supported algorithms, as the gateway reports them"`
}

func (d *Deps) ipsecConfigGet() sdk.ToolHandlerFor[targetIn, ipsecConfigGetOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in targetIn) (*sdk.CallToolResult, ipsecConfigGetOut, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, ipsecConfigGetOut{}, err
		}
		var raw any
		if err := c.Get(ctx, "/config/ipsec", &raw); err != nil {
			d.audit("ipsec_config_get", c.Name(), false, err.Error())
			return nil, ipsecConfigGetOut{}, err
		}
		d.audit("ipsec_config_get", c.Name(), true, "")
		return nil, ipsecConfigGetOut{Target: c.Name(), Config: maskIPsecSecrets(sanitizeAny(raw, 0))}, nil
	}
}

// ---- ipsec_tunnel_get ----

type ipsecTunnelIn struct {
	Target string `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	Name   string `json:"name" jsonschema:"tunnel name, as ipsec_status_get lists it"`
}

type ipsecTunnelGetOut struct {
	Target string `json:"target"`
	Tunnel any    `json:"tunnel" jsonschema:"endpoints, identities, authentication mode, IKE/ESP proposals, selector, DPD, state and counters; a credential-named member is masked"`
}

func (d *Deps) ipsecTunnelGet() sdk.ToolHandlerFor[ipsecTunnelIn, ipsecTunnelGetOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in ipsecTunnelIn) (*sdk.CallToolResult, ipsecTunnelGetOut, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, ipsecTunnelGetOut{}, err
		}
		if err := validatePathSegment(in.Name); err != nil {
			return nil, ipsecTunnelGetOut{}, fmt.Errorf("name: %w", err)
		}
		var raw any
		if err := c.Get(ctx, "/config/ipsec/tunnels/"+url.PathEscape(in.Name), &raw); err != nil {
			d.audit("ipsec_tunnel_get", c.Name(), false, err.Error())
			return nil, ipsecTunnelGetOut{}, err
		}
		d.audit("ipsec_tunnel_get", c.Name(), true, "")
		return nil, ipsecTunnelGetOut{Target: c.Name(), Tunnel: maskIPsecSecrets(sanitizeAny(raw, 0))}, nil
	}
}

// ipsecSecretMember reports whether a member name of an IPsec object can
// carry a credential. The tunnel read the gateway serves has none today;
// the check keeps a member added later out of the transcript.
func ipsecSecretMember(name string) bool {
	n := strings.ToLower(name)
	for _, s := range []string{"psk", "secret", "password", "passphrase", "privatekey", "private_key"} {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

// maskIPsecSecrets replaces the value of every credential-named member, at
// any depth, with the redaction marker.
func maskIPsecSecrets(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if ipsecSecretMember(k) {
				t[k] = redacted
				continue
			}
			t[k] = maskIPsecSecrets(val)
		}
	case []any:
		for i := range t {
			t[i] = maskIPsecSecrets(t[i])
		}
	}
	return v
}

// ---- ipsec_tunnel_peerconfig_get ----

// ipsecPeerConfLines bounds the conn block lines returned; the generator
// writes about thirty.
const ipsecPeerConfLines = 100

type ipsecPeerConfigGetOut struct {
	Target      string   `json:"target"`
	TunnelName  string   `json:"tunnel_name"`
	IPsecConf   []string `json:"ipsec_conf" jsonschema:"the strongSwan ipsec.conf conn block for the remote peer, one line per entry"`
	AuthEntries []string `json:"auth_entries" jsonschema:"the ipsec.secrets entry for the remote peer with its value masked (PSK mode only); empty for certificate tunnels"`
	Notes       string   `json:"notes,omitempty"`
	Note        string   `json:"note"`
}

// ipsecSecretValue matches the value of one ipsec.secrets entry:
// "[<selectors> ]: <TYPE> <value>". The separator is the first colon that
// starts the line or follows whitespace, so a colon inside an IPv6
// selector or inside the value never moves it.
var ipsecSecretValue = regexp.MustCompile(`^((?:.*?\s)?:\s+[A-Za-z0-9]+\s+)(\S.*)$`)

// maskAuthEntries keeps the selectors and the secret type of each
// ipsec.secrets line and masks its value. A line of any other shape is
// replaced whole: an entry the bridge cannot read is not passed on.
func maskAuthEntries(s string) []string {
	out := []string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if m := ipsecSecretValue.FindStringSubmatch(line); m != nil {
			out = append(out, clean(m[1]+redacted))
		} else {
			out = append(out, redacted)
		}
		if len(out) >= maxListItems {
			break
		}
	}
	return out
}

func (d *Deps) ipsecTunnelPeerconfigGet() sdk.ToolHandlerFor[ipsecTunnelIn, ipsecPeerConfigGetOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in ipsecTunnelIn) (*sdk.CallToolResult, ipsecPeerConfigGetOut, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, ipsecPeerConfigGetOut{}, err
		}
		if err := validatePathSegment(in.Name); err != nil {
			return nil, ipsecPeerConfigGetOut{}, fmt.Errorf("name: %w", err)
		}
		// Decoded into named fields only: a member the API may add later
		// never reaches the result by accident.
		var raw struct {
			TunnelName string `json:"tunnelName"`
			Conf       string `json:"ipsecConf"`
			Auth       string `json:"ipsecSecrets"`
			Notes      string `json:"notes"`
		}
		if err := c.Get(ctx, fmt.Sprintf("/config/ipsec/tunnels/%s/peerconfig", url.PathEscape(in.Name)), &raw); err != nil {
			d.audit("ipsec_tunnel_peerconfig_get", c.Name(), false, err.Error())
			return nil, ipsecPeerConfigGetOut{}, err
		}
		d.audit("ipsec_tunnel_peerconfig_get", c.Name(), true, "")
		out := ipsecPeerConfigGetOut{
			Target:      c.Name(),
			TunnelName:  clean(raw.TunnelName),
			IPsecConf:   []string{},
			AuthEntries: maskAuthEntries(raw.Auth),
			Notes:       cleanLong(raw.Notes),
			Note:        "the pre-shared key is never returned: the auth entry shows its selectors and type only; take the key from where it was provisioned",
		}
		for _, line := range strings.Split(raw.Conf, "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			out.IPsecConf = append(out.IPsecConf, clean(line))
			if len(out.IPsecConf) >= ipsecPeerConfLines {
				break
			}
		}
		return nil, out, nil
	}
}

// ---- ipsec_cert_list / ipsec_cert_get / ipsec_ca_cert_list / ipsec_ca_cert_get ----

// ipsecCertRaw names the metadata members of IPsecCertificate and
// IPsecCACertificate. Nothing else is decoded, so certificate or key
// material never reaches the result.
type ipsecCertRaw struct {
	Name        string   `json:"name"`
	Subject     string   `json:"subject"`
	Issuer      string   `json:"issuer"`
	Serial      string   `json:"serial"`
	NotBefore   string   `json:"notBefore"`
	NotAfter    string   `json:"notAfter"`
	San         []string `json:"san"`
	KeyUsage    []string `json:"keyUsage"`
	InstalledAt string   `json:"installedAt"`
	Description string   `json:"description"`
}

type ipsecCertMeta struct {
	Name        string   `json:"name"`
	Subject     string   `json:"subject"`
	Issuer      string   `json:"issuer"`
	Serial      string   `json:"serial"`
	NotBefore   string   `json:"not_before,omitempty"`
	NotAfter    string   `json:"not_after,omitempty"`
	Expired     *bool    `json:"expired,omitempty"`
	San         []string `json:"san,omitempty"`
	KeyUsage    []string `json:"key_usage,omitempty"`
	InstalledAt string   `json:"installed_at,omitempty"`
	Description string   `json:"description,omitempty"`
}

// ipsecCertNote says why validity can be absent: the gateway's handlers
// fill neither validity date nor installedAt, and the zero value they
// serialize is not a date.
const ipsecCertNote = "metadata only, no PEM; not_before, not_after and installed_at appear only when the gateway reports a real date (current handlers leave them unset)"

// ipsecDate returns an RFC 3339 date, or "" for an absent, unparsable or
// zero value (year 1 or the Unix epoch, which the gateway sends unset).
func ipsecDate(s string) (string, time.Time) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil || t.Year() <= 1 || t.Unix() == 0 {
		return "", time.Time{}
	}
	return t.UTC().Format(time.RFC3339), t
}

func ipsecCertFrom(r ipsecCertRaw, now time.Time) ipsecCertMeta {
	m := ipsecCertMeta{
		Name:        clean(r.Name),
		Subject:     clean(r.Subject),
		Issuer:      clean(r.Issuer),
		Serial:      clean(r.Serial),
		San:         cleanStrings(r.San),
		KeyUsage:    cleanStrings(r.KeyUsage),
		Description: clean(r.Description),
	}
	m.NotBefore, _ = ipsecDate(r.NotBefore)
	var na time.Time
	if m.NotAfter, na = ipsecDate(r.NotAfter); m.NotAfter != "" {
		exp := now.After(na)
		m.Expired = &exp
	}
	m.InstalledAt, _ = ipsecDate(r.InstalledAt)
	return m
}

type ipsecCertListOut struct {
	Target       string          `json:"target"`
	Count        int             `json:"count"`
	Certificates []ipsecCertMeta `json:"certificates"`
	Note         string          `json:"note"`
}

type ipsecCertGetIn struct {
	Target string `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	Name   string `json:"name" jsonschema:"certificate name, as the list tool reports it"`
}

type ipsecCertGetOut struct {
	Target      string        `json:"target"`
	Certificate ipsecCertMeta `json:"certificate"`
	Note        string        `json:"note"`
}

// certListOut converts the entries of one list read.
func certListOut(target string, entries []ipsecCertRaw) ipsecCertListOut {
	now := time.Now()
	out := ipsecCertListOut{Target: target, Count: len(entries), Certificates: []ipsecCertMeta{}, Note: ipsecCertNote}
	for _, r := range entries {
		if len(out.Certificates) >= maxListItems {
			break
		}
		out.Certificates = append(out.Certificates, ipsecCertFrom(r, now))
	}
	return out
}

func (d *Deps) ipsecCertList() sdk.ToolHandlerFor[targetIn, ipsecCertListOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in targetIn) (*sdk.CallToolResult, ipsecCertListOut, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, ipsecCertListOut{}, err
		}
		var raw struct {
			Entries []ipsecCertRaw `json:"ipsecCertificateAttr"`
		}
		if err := c.Get(ctx, "/config/ipsec/certificates/all", &raw); err != nil {
			d.audit("ipsec_cert_list", c.Name(), false, err.Error())
			return nil, ipsecCertListOut{}, err
		}
		d.audit("ipsec_cert_list", c.Name(), true, "")
		return nil, certListOut(c.Name(), raw.Entries), nil
	}
}

func (d *Deps) ipsecCACertList() sdk.ToolHandlerFor[targetIn, ipsecCertListOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in targetIn) (*sdk.CallToolResult, ipsecCertListOut, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, ipsecCertListOut{}, err
		}
		var raw struct {
			Entries []ipsecCertRaw `json:"ipsecCACertificateAttr"`
		}
		if err := c.Get(ctx, "/config/ipsec/ca-certificates/all", &raw); err != nil {
			d.audit("ipsec_ca_cert_list", c.Name(), false, err.Error())
			return nil, ipsecCertListOut{}, err
		}
		d.audit("ipsec_ca_cert_list", c.Name(), true, "")
		return nil, certListOut(c.Name(), raw.Entries), nil
	}
}

func (d *Deps) ipsecCertGet() sdk.ToolHandlerFor[ipsecCertGetIn, ipsecCertGetOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in ipsecCertGetIn) (*sdk.CallToolResult, ipsecCertGetOut, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, ipsecCertGetOut{}, err
		}
		if err := validatePathSegment(in.Name); err != nil {
			return nil, ipsecCertGetOut{}, fmt.Errorf("name: %w", err)
		}
		var raw ipsecCertRaw
		if err := c.Get(ctx, "/config/ipsec/certificates/"+url.PathEscape(in.Name), &raw); err != nil {
			d.audit("ipsec_cert_get", c.Name(), false, err.Error())
			return nil, ipsecCertGetOut{}, err
		}
		d.audit("ipsec_cert_get", c.Name(), true, "")
		return nil, ipsecCertGetOut{Target: c.Name(), Certificate: ipsecCertFrom(raw, time.Now()), Note: ipsecCertNote}, nil
	}
}

func (d *Deps) ipsecCACertGet() sdk.ToolHandlerFor[ipsecCertGetIn, ipsecCertGetOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in ipsecCertGetIn) (*sdk.CallToolResult, ipsecCertGetOut, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, ipsecCertGetOut{}, err
		}
		if err := validatePathSegment(in.Name); err != nil {
			return nil, ipsecCertGetOut{}, fmt.Errorf("name: %w", err)
		}
		var raw ipsecCertRaw
		if err := c.Get(ctx, "/config/ipsec/ca-certificates/"+url.PathEscape(in.Name), &raw); err != nil {
			d.audit("ipsec_ca_cert_get", c.Name(), false, err.Error())
			return nil, ipsecCertGetOut{}, err
		}
		d.audit("ipsec_ca_cert_get", c.Name(), true, "")
		return nil, ipsecCertGetOut{Target: c.Name(), Certificate: ipsecCertFrom(raw, time.Now()), Note: ipsecCertNote}, nil
	}
}
