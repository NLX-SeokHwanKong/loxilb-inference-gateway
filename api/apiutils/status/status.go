//go:build linux

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
	"os"

	"github.com/loxilb-io/loxilb/api/models"
	"golang.org/x/sys/unix"
)

// DeviceInfoGet - Get Device Status
func DeviceInfoGet() (*models.DeviceInfoEntry, error) {
	result := new(models.DeviceInfoEntry)
	machineIDFile := "/etc/machine-id"
	bootIDFile := "/proc/sys/kernel/random/boot_id"
	OSFile := "/etc/issue"
	versionFile := "/proc/version_signature"
	hostnameFile := "/etc/hostname"
	uptimeFile := "/proc/uptime"
	// Get File data
	machinID, err := os.ReadFile(machineIDFile)
	if err != nil {
		return nil, err
	}
	result.MachineID = string(machinID)

	bootID, err := os.ReadFile(bootIDFile)
	if err != nil {
		return nil, err
	}
	result.BootID = string(bootID)

	OS, err := os.ReadFile(OSFile)
	if err != nil {
		return nil, err
	}
	result.OS = string(OS)

	kernelVersion, err := os.ReadFile(versionFile)
	if err != nil {
		return nil, err
	}
	result.Kernel = string(kernelVersion)

	hostname, err := os.ReadFile(hostnameFile)
	if err != nil {
		return nil, err
	}
	result.HostName = string(hostname)
	uptime, err := os.ReadFile(uptimeFile)
	if err != nil {
		return nil, err
	}
	result.Uptime = string(uptime)

	architecture, err := deviceArchitecture()
	if err != nil {
		return nil, err
	}
	result.Architecture = architecture

	return result, nil
}

// deviceArchitecture reports the kernel machine architecture, without the
// trailing newline formerly produced by the uname -p command.
func deviceArchitecture() (string, error) {
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		return "", err
	}
	return unix.ByteSliceToString(uts.Machine[:]), nil
}

// Get FileSystem Status
func FileSystemInfoGet() ([]*models.FileSystemInfoEntry, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	return filesystemInfoFromMounts(string(data), unix.Statfs)
}
