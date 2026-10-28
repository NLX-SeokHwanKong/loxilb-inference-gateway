//go:build linux

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

package loxinet

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// checkCgroup2Mount reports whether path is a mounted cgroup2 filesystem
// carrying cgroup.procs, which is what the sockops/sockmap bootstrap joins.
// A plain directory at the path - what a restarted container keeps from its
// previous boot - fails the check, as does any other filesystem type.
func checkCgroup2Mount(path string) error {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return fmt.Errorf("statfs %s: %w", path, err)
	}
	if st.Type != unix.CGROUP2_SUPER_MAGIC {
		return fmt.Errorf("%s is not a cgroup2 mount (filesystem type 0x%x)", path, st.Type)
	}
	procs := filepath.Join(path, "cgroup.procs")
	if _, err := os.Stat(procs); err != nil {
		return fmt.Errorf("%s: %w", procs, err)
	}
	return nil
}
