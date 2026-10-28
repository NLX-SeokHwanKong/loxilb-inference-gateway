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

package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Reads the change gate performs before a mutation previews: the current
// state a tool shows beside its proposal and re-reads after it applied.
// Each is a plain GET of an operation the manifest already maps to a read
// tool; they live here so the tool code carries no second REST path.

// SecurityRate returns the security rate-limit configuration and counters
// (GET /config/securityrate/all answers a one-element list).
func (c *Client) SecurityRate(ctx context.Context) (map[string]any, error) {
	var out struct {
		Attr []map[string]any `json:"securityrateAttr"`
	}
	if err := c.Get(ctx, "/config/securityrate/all", &out); err != nil {
		return nil, err
	}
	if len(out.Attr) == 0 {
		return map[string]any{}, nil
	}
	return out.Attr[0], nil
}

// GPUStatus returns GET /config/gpu/status.
func (c *Client) GPUStatus(ctx context.Context) (map[string]any, error) {
	return c.getObject(ctx, "/config/gpu/status")
}

// LlamaFirewallStatus returns GET /config/llamafirewall/status.
func (c *Client) LlamaFirewallStatus(ctx context.Context) (map[string]any, error) {
	return c.getObject(ctx, "/config/llamafirewall/status")
}

// PIIStatus returns GET /config/pii/status.
func (c *Client) PIIStatus(ctx context.Context) (map[string]any, error) {
	return c.getObject(ctx, "/config/pii/status")
}

// OperParams returns GET /config/params (the log level).
func (c *Client) OperParams(ctx context.Context) (map[string]any, error) {
	return c.getObject(ctx, "/config/params")
}

// BGPNeighbors returns the configured BGP neighbours (GET /config/bgp/neigh/all).
func (c *Client) BGPNeighbors(ctx context.Context) ([]map[string]any, error) {
	var out struct {
		Attr []map[string]any `json:"bgpNeiAttr"`
	}
	if err := c.Get(ctx, "/config/bgp/neigh/all", &out); err != nil {
		return nil, err
	}
	return out.Attr, nil
}

// LBRuleByKey fetches one rule by its VIP/port/protocol composite key
// (GET /config/loadbalancer/externalipaddress/{ip}/port/{port}/protocol/{proto}).
// found is false when the gateway answers 404. The gateway keys rules on
// host, path and model as well, so the rule returned for a shared VIP:port
// may not be the one the caller means; compare those fields too.
func (c *Client) LBRuleByKey(ctx context.Context, ip string, port int, proto string) (rule map[string]any, found bool, err error) {
	path := fmt.Sprintf("/config/loadbalancer/externalipaddress/%s/port/%d/protocol/%s",
		url.PathEscape(ip), port, url.PathEscape(strings.ToLower(proto)))
	var out map[string]any
	if err := c.doAllow(ctx, http.MethodGet, path, nil, &out, map[int]bool{http.StatusNotFound: true}); err != nil {
		return nil, false, err
	}
	if _, has := out["serviceArguments"]; !has {
		return nil, false, nil
	}
	return out, true, nil
}

func (c *Client) getObject(ctx context.Context, path string) (map[string]any, error) {
	var out map[string]any
	if err := c.Get(ctx, path, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}
