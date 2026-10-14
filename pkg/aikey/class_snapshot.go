/*
 * Copyright (c) 2025 LoxiLB Authors
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

package aikey

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	tk "github.com/loxilb-io/loxilib"

	cmn "github.com/loxilb-io/loxilb/common"
)

// ClassSnapshot is everything the store says about objectives and classes,
// as of one read. Immutable once published: readers hold the pointer and
// never lock.
type ClassSnapshot struct {
	Objectives map[string]cmn.AIObjectiveEntry
	// Tenants and Keys hold only the rows that set a class field.
	Tenants map[string]cmn.AIClassPolicy
	Keys    map[string]cmn.AIClassPolicy
	// ReadAt is when the store answered with this content.
	ReadAt time.Time
	// Source is cmn.AIClassSourceStore or cmn.AIClassSourceLKG.
	Source string
}

// Configured reports whether the snapshot holds anything a request's class
// could depend on. With nothing configured no request is looked at.
func (c *ClassSnapshot) Configured() bool {
	return c != nil && (len(c.Objectives) > 0 || len(c.Tenants) > 0 || len(c.Keys) > 0)
}

// The last-known-good file: the content of the last snapshot the store
// answered with, kept beside the configuration snapshot so that a gateway
// that starts while the store is unreachable serves the classes it served
// before, not none.
const (
	// ClassLKGFileName is the file's name under the configuration directory.
	ClassLKGFileName = "aigw-lkg.json"
	// classLKGVersion is the file's format version. A file of another
	// version is not read.
	classLKGVersion = 1

	classRefreshEnv        = "LLB_AI_OBJECTIVE_REFRESH_SEC"
	classRefreshDefaultSec = 10
)

type classLKGFile struct {
	Version int `json:"version"`
	// SourceTimestamp is when the store answered with this content.
	SourceTimestamp time.Time                    `json:"source_timestamp"`
	Objectives      []cmn.AIObjectiveEntry       `json:"objectives"`
	Tenants         map[string]cmn.AIClassPolicy `json:"tenants"`
	Keys            map[string]cmn.AIClassPolicy `json:"keys"`
}

// classState is the class side of a Service. Its own struct so the zero
// Service the tests build keeps working.
type classState struct {
	snap atomic.Pointer[ClassSnapshot]
	// dir is the configuration directory the file lives in; "" = no file.
	dir atomic.Pointer[string]
	// written is the content of the file as last written or read, without
	// its timestamp: an unchanged read does not rewrite the file.
	written atomic.Pointer[[]byte]
	// lkgTicks counts refresh intervals served from the file.
	lkgTicks atomic.Uint64
	// onChange is told after every published snapshot.
	onChange atomic.Pointer[func(*ClassSnapshot)]
	started  atomic.Bool
}

// Classes returns the snapshot in force, nil when nothing was ever read.
func (s *Service) Classes() *ClassSnapshot {
	if s == nil {
		return nil
	}
	return s.class.snap.Load()
}

// ClassSource reports where the class configuration in force came from, when
// the store answered with it, and how many refresh intervals have been
// served from the last-known-good file.
func (s *Service) ClassSource() (source string, readAt time.Time, lkgIntervals uint64) {
	snap := s.Classes()
	if snap == nil {
		return cmn.AIClassSourceNone, time.Time{}, 0
	}
	return snap.Source, snap.ReadAt, s.class.lkgTicks.Load()
}

// SetClassChangeHook installs a function called after every published
// snapshot, on the goroutine that published it.
func (s *Service) SetClassChangeHook(fn func(*ClassSnapshot)) {
	if fn == nil {
		s.class.onChange.Store(nil)
		return
	}
	s.class.onChange.Store(&fn)
}

func (s *Service) publishClasses(snap *ClassSnapshot) {
	s.class.snap.Store(snap)
	if fn := s.class.onChange.Load(); fn != nil {
		(*fn)(snap)
	}
}

// RefreshClasses reads objectives and class rows from the store and
// publishes them. On any failure the snapshot in force stays.
func (s *Service) RefreshClasses() error {
	db, err := s.store()
	if err != nil {
		return err
	}
	objs, err := readObjectives(db)
	if err != nil {
		return fmt.Errorf("aikey: read objectives: %w", err)
	}
	tenants, err := readClassRows(db, sqlSelectTenantClasses)
	if err != nil {
		return fmt.Errorf("aikey: read tenant classes: %w", err)
	}
	keys, err := readClassRows(db, sqlSelectKeyClasses)
	if err != nil {
		return fmt.Errorf("aikey: read key classes: %w", err)
	}
	snap := &ClassSnapshot{Objectives: objs, Tenants: tenants, Keys: keys,
		ReadAt: time.Now().UTC(), Source: cmn.AIClassSourceStore}
	prev := s.class.snap.Load()
	s.publishClasses(snap)
	if prev != nil && prev.Source == cmn.AIClassSourceLKG {
		tk.LogIt(tk.LogInfo, "[AIKey] Objectives and classes read from the store again (%d objectives, %d tenants, %d keys); the last-known-good file is no longer served\n",
			len(objs), len(tenants), len(keys))
	}
	s.writeClassLKG(snap)
	return nil
}

// refreshClassesAfterWrite applies a local write at once. A failure is not
// the write's: the row is stored, and the next interval reads it.
func (s *Service) refreshClassesAfterWrite() {
	if err := s.RefreshClasses(); err != nil {
		tk.LogIt(tk.LogError, "[AIKey] Classes not refreshed after a write: %v\n", err)
	}
}

func lkgContent(snap *ClassSnapshot) classLKGFile {
	f := classLKGFile{Version: classLKGVersion, Tenants: snap.Tenants, Keys: snap.Keys,
		Objectives: make([]cmn.AIObjectiveEntry, 0, len(snap.Objectives))}
	for _, o := range snap.Objectives {
		f.Objectives = append(f.Objectives, o)
	}
	// encoding/json sorts map keys; the list is sorted here, so equal
	// content is equal bytes.
	sortObjectives(f.Objectives)
	if f.Tenants == nil {
		f.Tenants = map[string]cmn.AIClassPolicy{}
	}
	if f.Keys == nil {
		f.Keys = map[string]cmn.AIClassPolicy{}
	}
	return f
}

func sortObjectives(l []cmn.AIObjectiveEntry) {
	for i := 1; i < len(l); i++ {
		for j := i; j > 0 && l[j].Name < l[j-1].Name; j-- {
			l[j], l[j-1] = l[j-1], l[j]
		}
	}
}

// writeClassLKG writes the file when the content differs from what it
// holds. A failure is logged and nothing else: the file is a convenience of
// the next start, never a condition of this one.
func (s *Service) writeClassLKG(snap *ClassSnapshot) {
	dirp := s.class.dir.Load()
	if dirp == nil || *dirp == "" {
		return
	}
	f := lkgContent(snap)
	body, err := json.Marshal(f)
	if err != nil {
		return
	}
	if last := s.class.written.Load(); last != nil && bytes.Equal(*last, body) {
		return
	}
	f.SourceTimestamp = snap.ReadAt
	data, err := json.MarshalIndent(f, "", " ")
	if err != nil {
		return
	}
	if err := writeFileAtomic(*dirp, ClassLKGFileName, append(data, '\n')); err != nil {
		tk.LogIt(tk.LogError, "[AIKey] Last-known-good file %s not written: %v\n",
			filepath.Join(*dirp, ClassLKGFileName), err)
		return
	}
	s.class.written.Store(&body)
}

// loadClassLKG publishes the file's content when nothing has been read yet.
// Reports whether it did.
func (s *Service) loadClassLKG() bool {
	if s.class.snap.Load() != nil {
		return false
	}
	dirp := s.class.dir.Load()
	if dirp == nil || *dirp == "" {
		return false
	}
	path := filepath.Join(*dirp, ClassLKGFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			tk.LogIt(tk.LogError, "[AIKey] Last-known-good file %s not read: %v\n", path, err)
		}
		return false
	}
	var f classLKGFile
	if err := json.Unmarshal(data, &f); err != nil {
		tk.LogIt(tk.LogError, "[AIKey] Last-known-good file %s is not readable: %v\n", path, err)
		return false
	}
	if f.Version != classLKGVersion {
		tk.LogIt(tk.LogError, "[AIKey] Last-known-good file %s has version %d, this gateway reads %d: not used\n",
			path, f.Version, classLKGVersion)
		return false
	}
	snap := &ClassSnapshot{Objectives: make(map[string]cmn.AIObjectiveEntry, len(f.Objectives)),
		Tenants: f.Tenants, Keys: f.Keys, ReadAt: f.SourceTimestamp, Source: cmn.AIClassSourceLKG}
	for _, o := range f.Objectives {
		snap.Objectives[o.Name] = o
	}
	if snap.Tenants == nil {
		snap.Tenants = map[string]cmn.AIClassPolicy{}
	}
	if snap.Keys == nil {
		snap.Keys = map[string]cmn.AIClassPolicy{}
	}
	if !s.class.snap.CompareAndSwap(nil, snap) {
		return false
	}
	if fn := s.class.onChange.Load(); fn != nil {
		(*fn)(snap)
	}
	if body, err := json.Marshal(lkgContent(snap)); err == nil {
		s.class.written.Store(&body)
	}
	tk.LogIt(tk.LogWarning, "[AIKey] The key store has not answered: serving objectives and classes from %s, as the store had them at %s (%d objectives, %d tenants, %d keys)\n",
		path, f.SourceTimestamp.Format(time.RFC3339), len(snap.Objectives), len(snap.Tenants), len(snap.Keys))
	return true
}

// classTick is one refresh interval.
func (s *Service) classTick() {
	if err := s.RefreshClasses(); err != nil {
		s.loadClassLKG()
	}
	if snap := s.class.snap.Load(); snap != nil && snap.Source == cmn.AIClassSourceLKG {
		s.class.lkgTicks.Add(1)
	}
}

// ClassRefreshInterval is LLB_AI_OBJECTIVE_REFRESH_SEC: default 10 s, at
// least 1 s.
func ClassRefreshInterval() time.Duration {
	sec := classRefreshDefaultSec
	if v := os.Getenv(classRefreshEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			sec = n
		}
	}
	if sec < 1 {
		sec = 1
	}
	return time.Duration(sec) * time.Second
}

// StartClassRefresher keeps the class snapshot current for the life of the
// process: one read at once, then one every ClassRefreshInterval. dir is
// the configuration directory for the last-known-good file ("" = no file).
// A second call does nothing.
func (s *Service) StartClassRefresher(dir string) {
	if s == nil || !s.class.started.CompareAndSwap(false, true) {
		return
	}
	s.class.dir.Store(&dir)
	go func() {
		t := time.NewTicker(ClassRefreshInterval())
		defer t.Stop()
		for {
			s.classTick()
			<-t.C
		}
	}()
}

// writeFileAtomic writes dir/name through a temporary file, fsync, rename
// and a directory fsync, mode 0600: a reader sees the previous complete file
// or the new one.
func writeFileAtomic(dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+name+"-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, filepath.Join(dir, name)); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
