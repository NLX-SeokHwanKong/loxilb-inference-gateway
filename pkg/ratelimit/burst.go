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

package ratelimit

// BurstTokens is the most a token-quota bucket of tokensPerMin can ever hold
// with burstPct (0 = the process default): the bound ReserveTokens and
// ReserveTokensElastic refuse or shrink a claim against when the bucket is
// full. 0 for a bucket with no quota, which bounds nothing.
func BurstTokens(tokensPerMin, burstPct int) int64 {
	if tokensPerMin <= 0 {
		return 0
	}
	return burstTokensFor(int64(tokensPerMin), int64(burstPct))
}
