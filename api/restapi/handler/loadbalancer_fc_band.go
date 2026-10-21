/*
 * Copyright (c) 2026 NetLOX Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
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

// fcBandsModel renders the priority bands for a rule's read-back. Every
// band is present with every value explicit, zero included, so a reader
// can tell an idle band from one that was not reported.
func fcBandsModel(b cmn.FcBandsArg) *models.LoadbalanceEntryServiceArgumentsFcEffectiveBands {
	return &models.LoadbalanceEntryServiceArgumentsFcEffectiveBands{
		CriticalReservePct:   int32(b.CriticalReservePct),
		CriticalReserveUnits: int32(b.CriticalReserveUnits),
		Critical:             fcBandModel(b.Critical),
		Standard:             fcBandModel(b.Standard),
		Sheddable:            fcBandModel(b.Sheddable),
	}
}

func fcBandModel(b cmn.FcBandArg) *models.FcBandEffective {
	return &models.FcBandEffective{
		Inflight:    int32(b.Inflight),
		Queued:      int32(b.Queued),
		WaitMs:      int32(b.WaitMs),
		QueuePct:    int32(b.QueuePct),
		MinSharePct: int32(b.MinSharePct),
		FloorUnits:  int32(b.FloorUnits),
		Weight:      int32(b.Weight),
		WeightPct:   int32(b.WeightPct),
	}
}
