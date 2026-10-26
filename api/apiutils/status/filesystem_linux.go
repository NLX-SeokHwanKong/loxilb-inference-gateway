/*
 * Copyright (c) 2022 NetLOX Inc
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

package status

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/loxilb-io/loxilb/api/models"
	"golang.org/x/sys/unix"
)

// Read kernel mount and usage data without spawning children: the PID-1
// SIGCHLD reaper can otherwise steal df's status from os/exec.Wait.
func filesystemInfoFromMounts(data string, stat func(string, *unix.Statfs_t) error) ([]*models.FileSystemInfoEntry, error) {
	result := make([]*models.FileSystemInfoEntry, 0)
	seen := make(map[string]bool)
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for _, line := range strings.Split(data, "\n") {
		halves := strings.SplitN(line, " - ", 2)
		if len(halves) != 2 {
			continue
		}
		mount, fs := strings.Fields(halves[0]), strings.Fields(halves[1])
		if len(mount) < 6 || len(fs) < 2 {
			continue
		}
		target, source := unescape.Replace(mount[4]), unescape.Replace(fs[1])
		var st unix.Statfs_t
		// A mount may disappear during collection; like df, omit unavailable
		// entries and pseudo filesystems without storage blocks.
		if stat(target, &st) != nil || st.Blocks == 0 || st.Bsize <= 0 {
			continue
		}
		key := mount[2] + ":" + fs[0]
		if seen[key] {
			continue
		}
		seen[key] = true
		free := min(st.Bfree, st.Blocks)
		used := st.Blocks - free
		pct := uint64(0)
		if used+st.Bavail > 0 {
			pct = uint64(math.Ceil(float64(used) * 100 / float64(used+st.Bavail)))
		}
		result = append(result, &models.FileSystemInfoEntry{FileSystem: source, Type: fs[0], MountedOn: target, Size: humanBytes(float64(st.Blocks) * float64(st.Bsize)), Used: humanBytes(float64(used) * float64(st.Bsize)), Avail: humanBytes(float64(st.Bavail) * float64(st.Bsize)), UsePercent: strconv.FormatUint(pct, 10) + "%"})
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("no file system information found")
	}
	return result, nil
}

func humanBytes(value float64) string {
	units := []string{"", "K", "M", "G", "T", "P", "E"}
	i := 0
	for value >= 1024 && i < len(units)-1 {
		value /= 1024
		i++
	}
	if i > 0 && value < 10 {
		return fmt.Sprintf("%.1f%s", math.Ceil(value*10)/10, units[i])
	}
	return fmt.Sprintf("%.0f%s", math.Ceil(value), units[i])
}
