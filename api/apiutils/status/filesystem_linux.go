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

package status

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/loxilb-io/loxilb/api/models"
	"golang.org/x/sys/unix"
)

type filesystemMount struct{ device, root, target, source, kind string }

// These kernel interfaces have no storage usage to report. In particular,
// statfs on autofs can trigger an automount, so filter before any syscall.
func dummyFilesystem(kind string) bool {
	switch kind {
	case "autofs", "proc", "sysfs", "devpts", "cgroup", "cgroup2", "securityfs",
		"debugfs", "tracefs", "configfs", "fusectl", "mqueue", "pstore", "bpf",
		"rpc_pipefs", "binfmt_misc", "nsfs", "efivarfs", "selinuxfs", "smackfs":
		return true
	}
	return false
}

// Read kernel mount and usage data without spawning children: the process-wide
// SIGCHLD reaper can steal df's status from os/exec.Wait even outside PID 1.
func filesystemInfoFromMounts(data string, stat func(string, *unix.Statfs_t) error) ([]*models.FileSystemInfoEntry, error) {
	mounts := []filesystemMount{}
	lastPath := make(map[string]int)
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
		m := filesystemMount{device: mount[2], root: unescape.Replace(mount[3]), target: unescape.Replace(mount[4]), source: unescape.Replace(fs[1]), kind: fs[0]}
		lastPath[m.target] = len(mounts)
		mounts = append(mounts, m)
	}
	// Resolve stacked paths before grouping devices. statfs(path) sees only
	// the visible mount; retaining a covered entry would mislabel that result.
	groups := [][]filesystemMount{}
	groupIndex := make(map[string]int)
	for i, m := range mounts {
		if lastPath[m.target] != i || dummyFilesystem(m.kind) {
			continue
		}
		key := m.device + ":" + m.kind
		n, seen := groupIndex[key]
		if !seen {
			groupIndex[key] = len(groups)
			groups = append(groups, []filesystemMount{m})
			continue
		}
		// Prefer the filesystem root over a bind alias. An inaccessible preferred
		// path may fall back to another alias, but successful groups are read once.
		if m.root == "/" && groups[n][0].root != "/" {
			groups[n] = append([]filesystemMount{m}, groups[n]...)
		} else {
			groups[n] = append(groups[n], m)
		}
	}
	result := make([]*models.FileSystemInfoEntry, 0)
	for _, group := range groups {
		for _, m := range group {
			var st unix.Statfs_t
			if stat(m.target, &st) != nil {
				continue
			}
			blockSize := st.Frsize
			if blockSize <= 0 {
				blockSize = st.Bsize
			}
			if st.Blocks == 0 || blockSize <= 0 {
				break
			}
			free := min(st.Bfree, st.Blocks)
			used := st.Blocks - free
			percent := "-"
			denominator := float64(used) + float64(st.Bavail)
			if denominator > 0 {
				percent = strconv.FormatUint(uint64(math.Ceil(float64(used)*100/denominator)), 10) + "%"
			}
			result = append(result, &models.FileSystemInfoEntry{
				FileSystem: m.source, Type: m.kind, MountedOn: m.target,
				Size:  humanBytes(float64(st.Blocks) * float64(blockSize)),
				Used:  humanBytes(float64(used) * float64(blockSize)),
				Avail: humanBytes(float64(st.Bavail) * float64(blockSize)), UsePercent: percent,
			})
			break
		}
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
	// Rounding can cross either the one-decimal boundary or a unit boundary.
	if math.Ceil(value) >= 1024 && i < len(units)-1 {
		value /= 1024
		i++
	}
	if i > 0 && math.Ceil(value*10)/10 < 10 {
		return fmt.Sprintf("%.1f%s", math.Ceil(value*10)/10, units[i])
	}
	return fmt.Sprintf("%.0f%s", math.Ceil(value), units[i])
}
