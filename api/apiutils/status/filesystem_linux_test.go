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
	"errors"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestFilesystemKernelData(t *testing.T) {
	mounts := "1 0 8:1 / / rw - ext4 /dev/root rw\n2 1 8:1 /bind /bind rw - ext4 /dev/root rw\n3 1 0:2 / /proc rw - proc proc rw\n4 1 8:2 / /with\\040space rw - ext4 /dev/data rw\n5 1 8:3 / /gone rw - ext4 /dev/gone rw\nmalformed\n"
	rows, err := filesystemInfoFromMounts(mounts, func(path string, st *unix.Statfs_t) error {
		if path == "/gone" {
			return errors.New("unmounted")
		}
		if path != "/proc" {
			st.Bsize = 1024
			st.Blocks = 100
			st.Bfree = 40
			st.Bavail = 30
		}
		return nil
	})
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if rows[0].Size != "100K" || rows[0].Used != "60K" || rows[0].Avail != "30K" || rows[0].UsePercent != "67%" {
		t.Fatalf("unexpected usage: %+v", rows[0])
	}
	if rows[1].MountedOn != "/with space" {
		t.Fatalf("escaped mount: %q", rows[1].MountedOn)
	}
}

func TestFilesystemEmptyIsError(t *testing.T) {
	if _, err := filesystemInfoFromMounts("bad", unix.Statfs); err == nil {
		t.Fatal("empty data must not look like healthy storage")
	}
}

func TestFilesystemOvermountKeepsVisiblePath(t *testing.T) {
	mounts := "1 0 8:1 / / rw - ext4 /dev/root rw\n2 1 0:2 / /stack rw - tmpfs lower rw\n3 2 0:3 / /stack rw - tmpfs upper rw\n"
	calls := map[string]int{}
	rows, err := filesystemInfoFromMounts(mounts, func(path string, st *unix.Statfs_t) error {
		calls[path]++
		st.Blocks = 256
		st.Bfree = 128
		st.Bavail = 128
		st.Frsize = 4096
		st.Bsize = 16384
		return nil
	})
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if calls["/stack"] != 1 {
		t.Fatalf("overmount stat calls=%d", calls["/stack"])
	}
	if rows[1].FileSystem != "upper" || rows[1].MountedOn != "/stack" || rows[1].Size != "1.0M" {
		t.Fatalf("wrong visible mount: %+v", rows[1])
	}
}

func TestFilesystemFiltersBeforeStatAndPrefersRoot(t *testing.T) {
	mounts := "1 0 8:1 /sub /bind rw - ext4 /dev/root rw\n2 0 8:1 / /real rw - ext4 /dev/root rw\n3 0 0:3 / /auto rw - autofs automounter rw\n4 0 0:4 / /proc rw - proc proc rw\n5 0 8:2 / /hidden rw - ext4 lower rw\n6 5 0:5 / /hidden rw - autofs upper rw\n"
	calls := []string{}
	rows, err := filesystemInfoFromMounts(mounts, func(path string, st *unix.Statfs_t) error {
		calls = append(calls, path)
		if path != "/real" {
			t.Errorf("must not stat %s", path)
		}
		st.Bsize = 1024
		st.Blocks = 100
		return nil
	})
	if err != nil || len(rows) != 1 || rows[0].MountedOn != "/real" {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if len(calls) != 1 {
		t.Fatalf("stat calls=%v", calls)
	}
}

func TestFilesystemUnavailableRootFallsBackToAlias(t *testing.T) {
	mounts := "1 0 8:1 / /gone rw - ext4 /dev/root rw\n2 0 8:1 /sub /alias rw - ext4 /dev/root rw\n"
	rows, err := filesystemInfoFromMounts(mounts, func(path string, st *unix.Statfs_t) error {
		if path == "/gone" {
			return errors.New("unmounted")
		}
		st.Bsize = 1024
		st.Blocks = 100
		return nil
	})
	if err != nil || len(rows) != 1 || rows[0].MountedOn != "/alias" {
		t.Fatalf("alias fallback: %v %v", rows, err)
	}
}

func TestFilesystemFragmentSizeAndUndefinedUsage(t *testing.T) {
	for _, tc := range []struct {
		name            string
		fragment, block int64
		size            string
	}{
		{"fragment differs", 2048, 8192, "200K"}, {"fallback", 0, 1024, "100K"}, {"fragment with no block size", 1024, 0, "100K"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := filesystemInfoFromMounts("1 0 8:1 / / rw - ext4 /dev/root rw", func(_ string, st *unix.Statfs_t) error {
				st.Frsize = tc.fragment
				st.Bsize = tc.block
				st.Blocks = 100
				st.Bfree = 100
				return nil
			})
			if err != nil || len(rows) != 1 || rows[0].Size != tc.size || rows[0].UsePercent != "-" {
				t.Fatalf("rows=%v err=%v", rows, err)
			}
		})
	}
}

func TestFilesystemStorageTypesAreNotBlanketFiltered(t *testing.T) {
	for _, kind := range []string{"tmpfs", "devtmpfs", "squashfs", "fuse.sshfs"} {
		rows, err := filesystemInfoFromMounts("1 0 8:1 / /disk rw - "+kind+" disk rw", func(_ string, st *unix.Statfs_t) error { st.Frsize = 1024; st.Blocks = 10; return nil })
		if err != nil || len(rows) != 1 {
			t.Fatalf("storage type %s lost: %v", kind, err)
		}
	}
}

func TestHumanBytesRoundingBoundaries(t *testing.T) {
	for _, tc := range []struct {
		value float64
		want  string
	}{{0, "0"}, {1024, "1.0K"}, {1048064, "1.0M"}, {10726680821, "10G"}, {1048576, "1.0M"}, {1023, "1023"}} {
		if got := humanBytes(tc.value); got != tc.want {
			t.Errorf("humanBytes(%v)=%s want%s", tc.value, got, tc.want)
		}
	}
}

func TestNativeStatusReadsWithoutExecutables(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	arch, err := deviceArchitecture()
	if err != nil || arch == "" || strings.ContainsAny(arch, "\r\n") {
		t.Fatalf("machine architecture read failed: %v", err)
	}
	rows, err := FileSystemInfoGet()
	if err != nil || len(rows) == 0 {
		t.Fatalf("kernel filesystem read failed: %v", err)
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if seen[row.MountedOn] {
			t.Fatalf("duplicate mount path %q", row.MountedOn)
		}
		seen[row.MountedOn] = true
	}
}

// Opt-in only: the CI step enters a private mount namespace before starting
// Go. The test mounts only its own new temporary directory and unmounts both
// layers on every return path; no existing mount is changed.
func TestFilesystemOvermountLive(t *testing.T) {
	if os.Getenv("LOXILB_STATUS_TEST_MOUNTS") != "1" {
		t.Skip("requires isolated mount namespace and CAP_SYS_ADMIN")
	}
	target := t.TempDir()
	if err := unix.Mount("status-lower", target, "tmpfs", 0, "size=1048576,mode=0700"); err != nil {
		t.Fatal(err)
	}
	defer unix.Unmount(target, 0)
	if err := unix.Mount("status-upper", target, "tmpfs", 0, "size=2097152,mode=0700"); err != nil {
		t.Fatal(err)
	}
	defer unix.Unmount(target, 0)
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := filesystemInfoFromMounts(string(data), unix.Statfs)
	if err != nil {
		t.Fatal(err)
	}
	matches := 0
	for _, row := range rows {
		if row.MountedOn == target {
			matches++
			if row.FileSystem != "status-upper" || row.Size != "2.0M" {
				t.Fatalf("covered mount selected: %+v", row)
			}
		}
	}
	if matches != 1 {
		t.Fatalf("visible mount rows=%d want1", matches)
	}
}
