//go:build linux

/*
 * Copyright (c) 2026 NetLOX Inc
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may obtain a copy at http://www.apache.org/licenses/LICENSE-2.0
 */
package status

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/loxilb-io/loxilb/api/models"
)

// ProcessInfoGet reads procfs without creating children that compete with the
// process-wide SIGCHLD reaper. CPU is lifetime CPU / elapsed lifetime, as in ps.
func ProcessInfoGet() ([]*models.ProcessInfoEntry, error) {
	ticks, err := clockTicks()
	if err != nil {
		return nil, err
	}
	return processInfoFromProc("/proc", "/etc/passwd", ticks)
}

// AT_CLKTCK (17) is supplied by the Linux kernel in the native-word aux vector.
// Reading it avoids both an external getconf and an assumed kernel tick rate.
func clockTicks() (float64, error) {
	data, err := os.ReadFile("/proc/self/auxv")
	if err != nil {
		return 0, fmt.Errorf("read clock tick rate: %w", err)
	}
	word := strconv.IntSize / 8
	for len(data) >= 2*word {
		var key, value uint64
		if word == 8 {
			key = binary.NativeEndian.Uint64(data)
			value = binary.NativeEndian.Uint64(data[word:])
		} else {
			key = uint64(binary.NativeEndian.Uint32(data))
			value = uint64(binary.NativeEndian.Uint32(data[word:]))
		}
		if key == 17 && value > 0 {
			return float64(value), nil
		}
		if key == 0 {
			break
		}
		data = data[2*word:]
	}
	return 0, fmt.Errorf("procfs auxiliary vector has no clock tick rate")
}

type processStat struct {
	pid, command, state string
	priority, nice      int64
	cpu, start, virtual uint64
	resident            int64
}

func parseProcessStat(data string) (processStat, error) {
	var p processStat
	left, right := strings.IndexByte(data, '('), strings.LastIndexByte(data, ')')
	if left < 1 || right <= left {
		return p, fmt.Errorf("invalid process stat command")
	}
	p.pid = strings.TrimSpace(data[:left])
	if _, err := strconv.ParseUint(p.pid, 10, 32); err != nil {
		return p, err
	}
	p.command = data[left+1 : right]
	f := strings.Fields(data[right+1:]) // starts at field 3 (state)
	if len(f) < 22 || len(f[0]) != 1 {
		return p, fmt.Errorf("short process stat")
	}
	p.state = f[0]
	values := make([]uint64, 0, 4)
	for _, i := range []int{11, 12, 19, 20} {
		n, err := strconv.ParseUint(f[i], 10, 64)
		if err != nil {
			return p, fmt.Errorf("invalid process stat field %d: %w", i+3, err)
		}
		values = append(values, n)
	}
	if values[0] > ^uint64(0)-values[1] {
		return p, fmt.Errorf("process CPU counter overflow")
	}
	p.cpu, p.start, p.virtual = values[0]+values[1], values[2], values[3]
	for i, target := range map[int]*int64{15: &p.priority, 16: &p.nice, 21: &p.resident} {
		n, err := strconv.ParseInt(f[i], 10, 64)
		if err != nil {
			return p, err
		}
		*target = n
	}
	if p.resident < 0 {
		return p, fmt.Errorf("negative process resident pages")
	}
	return p, nil
}

func processInfoFromProc(root, passwd string, ticks float64) ([]*models.ProcessInfoEntry, error) {
	if ticks <= 0 || math.IsNaN(ticks) || math.IsInf(ticks, 0) {
		return nil, fmt.Errorf("invalid clock tick rate")
	}
	uptimeData, err := os.ReadFile(filepath.Join(root, "uptime"))
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(string(uptimeData))
	if len(fields) == 0 {
		return nil, fmt.Errorf("empty proc uptime")
	}
	uptime, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || uptime <= 0 || math.IsNaN(uptime) || math.IsInf(uptime, 0) {
		return nil, fmt.Errorf("invalid proc uptime")
	}
	memData, err := os.ReadFile(filepath.Join(root, "meminfo"))
	if err != nil {
		return nil, err
	}
	var totalKB uint64
	for _, line := range strings.Split(string(memData), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[0] == "MemTotal:" && f[2] == "kB" {
			totalKB, err = strconv.ParseUint(f[1], 10, 64)
			if err != nil {
				return nil, err
			}
			break
		}
	}
	if totalKB == 0 {
		return nil, fmt.Errorf("missing proc MemTotal")
	}
	users := map[string]string{}
	// User names are optional. Numeric UIDs remain useful when passwd is unavailable.
	if data, e := os.ReadFile(passwd); e == nil {
		for _, line := range strings.Split(string(data), "\n") {
			f := strings.Split(line, ":")
			if len(f) >= 4 {
				users[f[2]] = f[0]
			}
		}
	}
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	result := make([]*models.ProcessInfoEntry, 0)
	page := uint64(os.Getpagesize())
	for _, dir := range dirs {
		if _, err := strconv.ParseUint(dir.Name(), 10, 32); err != nil {
			continue
		}
		path := filepath.Join(root, dir.Name())
		data, err := os.ReadFile(filepath.Join(path, "stat"))
		if err != nil {
			if transientProcessError(err) {
				continue
			}
			return nil, err
		}
		p, err := parseProcessStat(string(data))
		if err != nil {
			return nil, fmt.Errorf("parse process %s: %w", dir.Name(), err)
		}
		if p.pid != dir.Name() {
			return nil, fmt.Errorf("process PID does not match directory")
		}
		data, err = os.ReadFile(filepath.Join(path, "status"))
		if err != nil {
			if transientProcessError(err) {
				continue
			}
			return nil, err
		}
		uid := ""
		for _, line := range strings.Split(string(data), "\n") {
			f := strings.Fields(line)
			if len(f) >= 3 && f[0] == "Uid:" {
				uid = f[2]
				break
			}
		} // effective UID, as in top
		if uid == "" {
			return nil, fmt.Errorf("process %s has no effective UID", p.pid)
		}
		statm, err := os.ReadFile(filepath.Join(path, "statm"))
		if err != nil {
			if transientProcessError(err) {
				continue
			}
			return nil, err
		}
		f := strings.Fields(string(statm))
		if len(f) < 3 {
			return nil, fmt.Errorf("short process statm")
		}
		shared, err := strconv.ParseUint(f[2], 10, 64)
		if err != nil {
			return nil, err
		}
		// A PID may disappear or be reused between the independent procfs reads.
		again, err := os.ReadFile(filepath.Join(path, "stat"))
		if err != nil {
			if transientProcessError(err) {
				continue
			}
			return nil, err
		}
		current, err := parseProcessStat(string(again))
		if err != nil {
			return nil, err
		}
		if current.start != p.start {
			continue
		}
		cpu := 0.0
		if elapsed := uptime - float64(p.start)/ticks; elapsed > 0 {
			cpu = 100 * float64(p.cpu) / ticks / elapsed
		}
		user := users[uid]
		if user == "" {
			user = uid
		}
		priority := strconv.FormatInt(p.priority, 10)
		if p.priority < 0 {
			priority = "rt"
		}
		centis := uint64(float64(p.cpu) * 100 / ticks)
		result = append(result, &models.ProcessInfoEntry{Pid: p.pid, User: user, Priority: priority, Nice: strconv.FormatInt(p.nice, 10), VirtMemory: strconv.FormatUint(p.virtual/1024, 10), ResidentSize: strconv.FormatUint(uint64(p.resident)*page/1024, 10), SharedMemory: strconv.FormatUint(shared*page/1024, 10), Status: p.state, CPUUsage: fmt.Sprintf("%.1f", cpu), MemoryUsage: fmt.Sprintf("%.1f", 100*float64(p.resident)*float64(page)/1024/float64(totalKB)), Time: fmt.Sprintf("%d:%02d.%02d", centis/6000, (centis/100)%60, centis%100), Command: p.command})
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("no readable processes in procfs")
	}
	sort.Slice(result, func(i, j int) bool {
		a, _ := strconv.ParseFloat(result[i].CPUUsage, 64)
		b, _ := strconv.ParseFloat(result[j].CPUUsage, 64)
		if a != b {
			return a > b
		}
		x, _ := strconv.ParseUint(result[i].Pid, 10, 32)
		y, _ := strconv.ParseUint(result[j].Pid, 10, 32)
		return x < y
	})
	return result, nil
}

func transientProcessError(err error) bool { return os.IsNotExist(err) || os.IsPermission(err) }
