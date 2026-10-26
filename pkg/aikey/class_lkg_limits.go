/*
 * Copyright (c) 2025 LoxiLB Authors
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

package aikey

import (
	"fmt"
	"sort"
)

// The limit rows of the last-known-good file.
//
// A request whose tenant a network-identity binding names carries no key,
// so nothing the key store must confirm about a credential stands between it
// and its tenant's limits. During an outage those limits come from what the
// store last confirmed to this process (rlLastKnown). A process that starts
// while the store is away has confirmed nothing, and used to refuse every
// such request. The file therefore also carries the tenant, tenant|model and
// default limit rows as the store last had them, and a process that serves
// the file serves those rows too, exactly where rlLastKnown would be read,
// until the store answers once. Keys are never in the file: a keyed request
// still fails closed while the store is away.

var (
	sqlSelectAllTenantLimits = fmt.Sprintf(
		`SELECT tenant_id, rps, tokens_per_min, burst_pct FROM %s.tenant_rate_limits`, Schema)
	sqlSelectAllTenantModelLimits = fmt.Sprintf(
		`SELECT tenant_id, model, tokens_per_min FROM %s.tenant_model_rate_limits`, Schema)
	sqlSelectAllRateLimitDefaults = fmt.Sprintf(
		`SELECT scope, rule_ident, default_user_rps, default_user_tpm, default_tenant_rps,`+
			` default_tenant_tpm, vip_shared_rps, vip_shared_tpm FROM %s.rate_limit_defaults`, Schema)
)

// lkgTenantLimit is one row of tenant_rate_limits in the file.
type lkgTenantLimit struct {
	Tenant       string `json:"tenant"`
	RPS          int    `json:"rps"`
	TokensPerMin int    `json:"tokens_per_min"`
	BurstPct     int    `json:"burst_pct"`
}

// lkgTenantModelLimit is one row of tenant_model_rate_limits in the file.
type lkgTenantModelLimit struct {
	Tenant       string `json:"tenant"`
	Model        string `json:"model"`
	TokensPerMin int    `json:"tokens_per_min"`
}

// lkgDefaults is one row of rate_limit_defaults in the file.
type lkgDefaults struct {
	Scope            string `json:"scope"`
	RuleIdent        string `json:"rule_ident"`
	DefaultUserRPS   int    `json:"default_user_rps"`
	DefaultUserTPM   int    `json:"default_user_tpm"`
	DefaultTenantRPS int    `json:"default_tenant_rps"`
	DefaultTenantTPM int    `json:"default_tenant_tpm"`
	VipSharedRPS     int    `json:"vip_shared_rps"`
	VipSharedTPM     int    `json:"vip_shared_tpm"`
}

// limitRows is the whole of the three tables, as of one read, sorted so that
// equal content is equal bytes in the file.
type limitRows struct {
	Tenants      []lkgTenantLimit
	TenantModels []lkgTenantModelLimit
	Defaults     []lkgDefaults
}

func readLimitRows(db DBTX) (*limitRows, error) {
	out := &limitRows{}
	rows, err := db.Query(sqlSelectAllTenantLimits)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r lkgTenantLimit
		if err := rows.Scan(&r.Tenant, &r.RPS, &r.TokensPerMin, &r.BurstPct); err != nil {
			rows.Close()
			return nil, err
		}
		out.Tenants = append(out.Tenants, r)
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}

	if rows, err = db.Query(sqlSelectAllTenantModelLimits); err != nil {
		return nil, err
	}
	for rows.Next() {
		var r lkgTenantModelLimit
		if err := rows.Scan(&r.Tenant, &r.Model, &r.TokensPerMin); err != nil {
			rows.Close()
			return nil, err
		}
		out.TenantModels = append(out.TenantModels, r)
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}

	if rows, err = db.Query(sqlSelectAllRateLimitDefaults); err != nil {
		return nil, err
	}
	for rows.Next() {
		var r lkgDefaults
		if err := rows.Scan(&r.Scope, &r.RuleIdent, &r.DefaultUserRPS, &r.DefaultUserTPM,
			&r.DefaultTenantRPS, &r.DefaultTenantTPM, &r.VipSharedRPS, &r.VipSharedTPM); err != nil {
			rows.Close()
			return nil, err
		}
		out.Defaults = append(out.Defaults, r)
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}
	out.sort()
	return out, nil
}

type rowsCloser interface {
	Err() error
	Close() error
}

func closeRows(r rowsCloser) error {
	err := r.Err()
	r.Close()
	return err
}

func (l *limitRows) sort() {
	sort.Slice(l.Tenants, func(i, j int) bool { return l.Tenants[i].Tenant < l.Tenants[j].Tenant })
	sort.Slice(l.TenantModels, func(i, j int) bool {
		a, b := l.TenantModels[i], l.TenantModels[j]
		if a.Tenant != b.Tenant {
			return a.Tenant < b.Tenant
		}
		return a.Model < b.Model
	})
	sort.Slice(l.Defaults, func(i, j int) bool {
		a, b := l.Defaults[i], l.Defaults[j]
		if a.Scope != b.Scope {
			return a.Scope < b.Scope
		}
		return a.RuleIdent < b.RuleIdent
	})
}

// lkgLimitSet is the file's limit rows, indexed by the rate-limit cache keys
// the outage fallbacks read. The file holds the whole of each table, so a
// key that is not in it is a row the store did not have: "no limits".
type lkgLimitSet struct {
	limits   map[string]*rateLimitCacheEntry
	defaults map[string]*defaultsCacheEntry
}

func newLKGLimitSet(l *limitRows) *lkgLimitSet {
	set := &lkgLimitSet{
		limits:   make(map[string]*rateLimitCacheEntry, len(l.Tenants)+len(l.TenantModels)),
		defaults: make(map[string]*defaultsCacheEntry, len(l.Defaults)),
	}
	for _, r := range l.Tenants {
		set.limits[cacheKeyForTenant(r.Tenant)] = &rateLimitCacheEntry{rps: r.RPS,
			tokensPerMin: r.TokensPerMin, burstPct: r.BurstPct}
	}
	for _, r := range l.TenantModels {
		set.limits[cacheKeyForModel(r.Tenant, r.Model)] = &rateLimitCacheEntry{tokensPerMin: r.TokensPerMin}
	}
	for _, r := range l.Defaults {
		set.defaults[cacheKeyForDefaults(r.Scope, r.RuleIdent)] = &defaultsCacheEntry{
			userRPS: r.DefaultUserRPS, userTPM: r.DefaultUserTPM,
			tenantRPS: r.DefaultTenantRPS, tenantTPM: r.DefaultTenantTPM,
			vipRPS: r.VipSharedRPS, vipTPM: r.VipSharedTPM, exists: true,
		}
	}
	return set
}

// lkgRateLimit answers a tenant or tenant|model cache key from the file
// while the file is served; ok is false otherwise.
func (s *Service) lkgRateLimit(cacheKey string) (*rateLimitCacheEntry, bool) {
	set := s.class.lkgLimits.Load()
	if set == nil {
		return nil, false
	}
	if e, found := set.limits[cacheKey]; found {
		return e, true
	}
	return &rateLimitCacheEntry{}, true
}

// lkgDefaultsEntry is lkgRateLimit for the defaults rows.
func (s *Service) lkgDefaultsEntry(cacheKey string) (*defaultsCacheEntry, bool) {
	set := s.class.lkgLimits.Load()
	if set == nil {
		return nil, false
	}
	if e, found := set.defaults[cacheKey]; found {
		return e, true
	}
	return &defaultsCacheEntry{}, true
}
