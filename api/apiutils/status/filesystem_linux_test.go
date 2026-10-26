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
	"errors"
	"golang.org/x/sys/unix"
	"testing"
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
