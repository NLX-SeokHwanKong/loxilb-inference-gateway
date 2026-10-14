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
	tk "github.com/loxilb-io/loxilib"

	cmn "github.com/loxilb-io/loxilb/common"
)

// PatchTenantClass changes the class fields of a tenant that the patch
// names and leaves the others, and the tenant's limits, as they are.
func (s *Service) PatchTenantClass(tenantID string, patch cmn.AIClassPatch) error {
	if patch.IsZero() {
		return nil
	}
	cur, err := s.GetTenantClass(tenantID)
	if err != nil {
		return err
	}
	return s.SetTenantClass(tenantID, patch.Apply(cur))
}

// PatchAPIKeyClass changes the class fields of a key that the patch names
// and leaves the others as they are.
func (s *Service) PatchAPIKeyClass(keyID string, patch cmn.AIClassPatch) error {
	if patch.IsZero() {
		return nil
	}
	cur, err := s.GetAPIKeyClass(keyID)
	if err != nil {
		return err
	}
	return s.SetAPIKeyClass(keyID, patch.Apply(cur))
}

// ListAPIKeyClasses reads from the store the class fields of every key that
// sets one, by key id.
func (s *Service) ListAPIKeyClasses() (map[string]cmn.AIClassPolicy, error) {
	db, err := s.store()
	if err != nil {
		return nil, err
	}
	out, err := readClassRows(db, sqlSelectKeyClasses)
	if err != nil {
		tk.LogIt(tk.LogError, "[AIKey] Failed to list key classes: %v\n", err)
		return nil, errStoreUnavailable
	}
	return out, nil
}
