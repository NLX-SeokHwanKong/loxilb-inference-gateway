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

package handler

import (
	"github.com/loxilb-io/loxilb/api/models"
	cmn "github.com/loxilb-io/loxilb/common"
)

// fcReservationHeldModel renders the buckets holding a token reservation
// for a rule's read-back. The list is always present, empty when nothing is
// held: a reader must be able to tell "nothing held" from "not reported".
func fcReservationHeldModel(held []cmn.FcResvHeldArg) []*models.LoadbalanceEntryServiceArgumentsFcEffectiveReservationHeldItems0 {
	out := make([]*models.LoadbalanceEntryServiceArgumentsFcEffectiveReservationHeldItems0, 0, len(held))
	for _, h := range held {
		out = append(out, &models.LoadbalanceEntryServiceArgumentsFcEffectiveReservationHeldItems0{
			Kind:   h.Kind,
			Bucket: h.Bucket,
			Tokens: int64(h.Tokens),
		})
	}
	return out
}
