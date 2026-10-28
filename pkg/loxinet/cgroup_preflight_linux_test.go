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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// A plain directory - the state a restarted container keeps from its previous
// boot - is refused with a message that names the mount, not a generic open
// error.
func TestCheckCgroup2MountRefusesPlainDirectory(t *testing.T) {
	dir := t.TempDir()
	err := checkCgroup2Mount(dir)
	if err == nil {
		t.Fatalf("plain directory %s accepted as a cgroup2 mount", dir)
	}
	if !strings.Contains(err.Error(), "is not a cgroup2 mount") {
		t.Fatalf("unexpected error for a plain directory: %v", err)
	}
}

func TestCheckCgroup2MountMissingPath(t *testing.T) {
	err := checkCgroup2Mount(filepath.Join(t.TempDir(), "absent"))
	if err == nil {
		t.Fatal("missing path accepted as a cgroup2 mount")
	}
}

// The positive case needs a real cgroup2 hierarchy; the host's own is used
// when there is one, so the check is not asserted on a cgroup v1 host.
func TestCheckCgroup2MountAcceptsHostHierarchy(t *testing.T) {
	const root = "/sys/fs/cgroup"
	var st unix.Statfs_t
	if err := unix.Statfs(root, &st); err != nil || st.Type != unix.CGROUP2_SUPER_MAGIC {
		t.Skipf("%s is not a cgroup2 mount on this host", root)
	}
	if _, err := os.Stat(filepath.Join(root, "cgroup.procs")); err != nil {
		t.Skipf("%s carries no cgroup.procs: %v", root, err)
	}
	if err := checkCgroup2Mount(root); err != nil {
		t.Fatalf("host cgroup2 hierarchy refused: %v", err)
	}
}
