/*
 * Copyright (c) 2026 NetLOX Inc
 * SPDX-License-Identifier: Apache-2.0
 */

package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/loxilb-io/loxilb-inference-gateway/mcp/internal/mcp/guard"
)

// The PR-C2 IPsec reads: viewer tools, GET only; the pre-shared key of a
// peer configuration and any certificate material never reach the result.

var ipsecReadTools = []string{
	"ipsec_config_get", "ipsec_tunnel_get", "ipsec_tunnel_peerconfig_get",
	"ipsec_cert_list", "ipsec_cert_get", "ipsec_ca_cert_list", "ipsec_ca_cert_get",
}

const ipsecFakePEM = "-----BEGIN CERTIFICATE-----\nMIIfake\n-----END CERTIFICATE-----\n"

func ipsecRoutes(t *testing.T) map[string]string {
	t.Helper()
	future := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	peer, _ := json.Marshal(map[string]any{
		"tunnelName":   "tun1",
		"ipsecConf":    "# strongSwan peer configuration for tunnel tun1\n\nconn tun1\n    keyexchange=ikev2\n    left=31.31.31.253\n    leftauth=psk\n",
		"ipsecSecrets": `peer-b peer-a : PSK "s3cr3t-psk"`,
		"notes":        "install on the remote peer",
	})
	// The members a certificate read must never forward: not something the
	// API sends today.
	cert, _ := json.Marshal(map[string]any{
		"name": "gw-cert", "subject": "CN=gw.example", "issuer": "CN=lab-ca", "serial": "1092",
		"san": []string{"gw.example"}, "keyUsage": []string{"digitalSignature"},
		"notBefore": "0001-01-01T00:00:00.000Z", "notAfter": "0001-01-01T00:00:00.000Z", "installedAt": "1970-01-01T00:00:00.000Z",
		"certificate": ipsecFakePEM, "privateKey": "-----BEGIN PRIVATE KEY-----\nMIIkey\n-----END PRIVATE KEY-----\n",
	})
	ca, _ := json.Marshal(map[string]any{
		"name": "lab-ca", "subject": "CN=lab-ca", "issuer": "CN=lab-ca", "serial": "1",
		"notBefore": past, "notAfter": future, "certificate": ipsecFakePEM,
	})
	return map[string]string{
		"/netlox/v1/config/ipsec":                         `{"fastPathEnabled":true,"hwOffloadEnabled":false,"antiReplayEnabled":true,"mtu":1400,"supportedAlgorithms":["aes256gcm16"]}`,
		"/netlox/v1/config/ipsec/tunnels/tun1":            `{"name":"tun1","localIp":"31.31.31.254","remoteIp":"31.31.31.253","authMode":"psk","psk":"leaked-if-the-gateway-forgot","state":"down","dpd":{"action":"clear"}}`,
		"/netlox/v1/config/ipsec/tunnels/tun1/peerconfig": string(peer),
		"/netlox/v1/config/ipsec/certificates/all":        `{"ipsecCertificateAttr":[` + string(cert) + `]}`,
		"/netlox/v1/config/ipsec/certificates/gw-cert":    string(cert),
		"/netlox/v1/config/ipsec/ca-certificates/all":     `{"ipsecCACertificateAttr":[` + string(ca) + `]}`,
		"/netlox/v1/config/ipsec/ca-certificates/lab-ca":  string(ca),
		"/netlox/v1/config/ipsec/stats":                   `{"tunnelsTotal":1}`,
		"/netlox/v1/config/ipsec/sas/all":                 `{"ipsecSaAttr":[]}`,
		"/netlox/v1/config/ipsec/tunnels/all":             `{"ipsecTunnelAttr":[{"name":"tun1","state":"down"}]}`,
	}
}

func TestIPsecReadsAreViewerToolsAndSendNoMutation(t *testing.T) {
	mock := newMockLoxilb(t)
	mock.routes = ipsecRoutes(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	viewer, _ := listNames(t, b, guard.RoleViewer)
	for _, name := range ipsecReadTools {
		if !viewer[name] {
			t.Errorf("viewer does not list %s", name)
		}
	}
	cs := session(t, b, guard.RoleViewer)
	calls := map[string]map[string]any{
		"ipsec_tunnel_get":            {"name": "tun1"},
		"ipsec_tunnel_peerconfig_get": {"name": "tun1"},
		"ipsec_cert_get":              {"name": "gw-cert"},
		"ipsec_ca_cert_get":           {"name": "lab-ca"},
	}
	for _, name := range ipsecReadTools {
		out := callViewer(t, cs, name, calls[name])
		if out["target"] != "t1" {
			t.Errorf("%s: target = %v", name, out["target"])
		}
	}
	for _, name := range []string{"ipsec_tunnel_get", "ipsec_tunnel_peerconfig_get", "ipsec_cert_get", "ipsec_ca_cert_get"} {
		callRefused(t, cs, name, map[string]any{"name": "../x"}, "name:")
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	for _, r := range mock.requests {
		if !strings.HasPrefix(r, "GET ") {
			t.Errorf("a read tool sent %s", r)
		}
	}
}

func TestIPsecPeerconfigMasksThePSK(t *testing.T) {
	mock := newMockLoxilb(t)
	mock.routes = ipsecRoutes(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleViewer)
	out := callViewer(t, cs, "ipsec_tunnel_peerconfig_get", map[string]any{"name": "tun1"})
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "s3cr3t") || strings.Contains(string(raw), "ipsecSecrets") {
		t.Fatalf("peer configuration leaked the PSK: %s", raw)
	}
	auth, _ := out["auth_entries"].([]any)
	if len(auth) != 1 || auth[0] != "peer-b peer-a : PSK [REDACTED]" {
		t.Errorf("auth_entries = %v", out["auth_entries"])
	}
	conf, _ := out["ipsec_conf"].([]any)
	if len(conf) != 5 || conf[1] != "conn tun1" || out["tunnel_name"] != "tun1" {
		t.Errorf("ipsec_conf = %v", out["ipsec_conf"])
	}
}

func TestIPsecTunnelGetMasksCredentialMembers(t *testing.T) {
	mock := newMockLoxilb(t)
	mock.routes = ipsecRoutes(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleViewer)
	out := callViewer(t, cs, "ipsec_tunnel_get", map[string]any{"name": "tun1"})
	raw, _ := json.Marshal(out)
	tun, _ := out["tunnel"].(map[string]any)
	if strings.Contains(string(raw), "leaked") || tun["psk"] != "[REDACTED]" || tun["authMode"] != "psk" || tun["remoteIp"] != "31.31.31.253" {
		t.Errorf("ipsec_tunnel_get: %s", raw)
	}
}

func TestIPsecCertReadsReturnMetadataOnly(t *testing.T) {
	mock := newMockLoxilb(t)
	mock.routes = ipsecRoutes(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleViewer)
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"ipsec_cert_list", nil}, {"ipsec_cert_get", map[string]any{"name": "gw-cert"}},
		{"ipsec_ca_cert_list", nil}, {"ipsec_ca_cert_get", map[string]any{"name": "lab-ca"}},
	} {
		out := callViewer(t, cs, c.tool, c.args)
		raw, _ := json.Marshal(out)
		for _, bad := range []string{"BEGIN", "MIIfake", "MIIkey", "privateKey", `"certificate":"`} {
			if strings.Contains(string(raw), bad) {
				t.Fatalf("%s leaked %q: %s", c.tool, bad, raw)
			}
		}
	}
	// The zero dates the gateway serializes for unset fields are not dates.
	out := callViewer(t, cs, "ipsec_cert_get", map[string]any{"name": "gw-cert"})
	crt, _ := out["certificate"].(map[string]any)
	if _, ok := crt["not_after"]; ok || crt["installed_at"] != nil || crt["expired"] != nil || crt["subject"] != "CN=gw.example" {
		t.Errorf("gw-cert metadata: %v", crt)
	}
	out = callViewer(t, cs, "ipsec_ca_cert_list", nil)
	certs, _ := out["certificates"].([]any)
	ca, _ := certs[0].(map[string]any)
	if out["count"] != float64(1) || ca["expired"] != false || ca["not_after"] == nil || ca["name"] != "lab-ca" {
		t.Errorf("CA list: %v", out)
	}
}

func TestIPsecStatusCarriesTheConfiguration(t *testing.T) {
	mock := newMockLoxilb(t)
	mock.routes = ipsecRoutes(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleViewer)
	out := callViewer(t, cs, "ipsec_status_get", nil)
	cfg, _ := out["config"].(map[string]any)
	if cfg["fastPathEnabled"] != true || cfg["mtu"] != float64(1400) || out["tunnels"] == nil || out["errors"] != nil {
		t.Errorf("ipsec_status_get: %v", out)
	}
}
