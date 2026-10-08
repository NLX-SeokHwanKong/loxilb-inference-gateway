/*
 * Copyright (c) 2026 NetLOX Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
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
	"strings"
	"testing"
	"unicode/utf8"
)

// The tokenizer runs synchronously on a proxy worker and the caller keeps
// only the first maxTokens ids, so the text handed to it must be bounded
// before the encode. These cases drive kvTokenizeWithCache with a tokenizer
// that records what it was given and answers one id per byte, so the bound
// and the cut guard are visible in what comes back.

// byteTokenizer answers one id per byte of its input and remembers the
// longest input it was handed.
type byteTokenizer struct {
	calls   int
	maxSeen int
	perByte int // ids per byte; 1 = one token per byte
}

func (b *byteTokenizer) Encode(text string, addSpecialTokens bool) []uint32 {
	b.calls++
	if len(text) > b.maxSeen {
		b.maxSeen = len(text)
	}
	n := len(text) * b.perByte
	ids := make([]uint32, n)
	for i := range ids {
		ids[i] = uint32(i + 1)
	}
	return ids
}

func (b *byteTokenizer) Close() {}

type byteBackend struct{ tok *byteTokenizer }

func (b *byteBackend) LoadModel(tokenizerPath string) KvTokenizer { return b.tok }
func (b *byteBackend) Name() string                               { return "byte-mock" }

func withByteTokenizer(t *testing.T, perByte int) *byteTokenizer {
	t.Helper()
	KvTokenCacheReset()
	KvTokenizerPoolReset()
	t.Cleanup(KvTokenCacheReset)
	t.Cleanup(KvTokenizerPoolReset)
	tok := &byteTokenizer{perByte: perByte}
	KvRegisterTokenizerBackend(&byteBackend{tok: tok})
	t.Cleanup(func() { KvRegisterTokenizerBackend(nil) })
	return tok
}

func TestKvTokenizeBoundCutsBeforeEncode(t *testing.T) {
	tok := withByteTokenizer(t, 1)
	const maxTokens = 4096
	limit := maxTokens * kvTokenizeBytesPerTokenDefault

	long := strings.Repeat("x", 1<<20) // a 1 MiB prompt
	ids := kvTokenizeWithCache(long, "bound-model", maxTokens, true)
	if len(ids) != maxTokens {
		t.Fatalf("ids = %d, want %d", len(ids), maxTokens)
	}
	if tok.maxSeen > limit {
		t.Fatalf("tokenizer saw %d bytes, want at most %d (the bound)", tok.maxSeen, limit)
	}
	if tok.maxSeen != limit {
		t.Fatalf("tokenizer saw %d bytes, want exactly the bound %d", tok.maxSeen, limit)
	}
}

func TestKvTokenizeBoundSharesEntryPastTheBound(t *testing.T) {
	tok := withByteTokenizer(t, 1)
	const maxTokens = 4096
	limit := maxTokens * kvTokenizeBytesPerTokenDefault

	head := strings.Repeat("h", limit)
	a := head + strings.Repeat("a", 10000)
	b := head + strings.Repeat("b", 10000)
	idsA := kvTokenizeWithCache(a, "bound-model", maxTokens, true)
	idsB := kvTokenizeWithCache(b, "bound-model", maxTokens, true)
	if tok.calls != 1 {
		t.Fatalf("encode calls = %d, want 1: texts equal up to the bound share one entry", tok.calls)
	}
	if len(idsA) != len(idsB) || len(idsA) != maxTokens {
		t.Fatalf("ids = %d/%d, want %d", len(idsA), len(idsB), maxTokens)
	}
	// and a text that differs INSIDE the bound is a different entry
	c := strings.Repeat("c", limit-1) + "h" + strings.Repeat("a", 10000)
	kvTokenizeWithCache(c, "bound-model", maxTokens, true)
	if tok.calls != 2 {
		t.Fatalf("encode calls = %d, want 2: a text that differs inside the bound is encoded", tok.calls)
	}
}

func TestKvTokenizeBoundKeepsRunesWhole(t *testing.T) {
	const maxTokens = 8
	limit := maxTokens * kvTokenizeBytesPerTokenDefault // 128 bytes
	// 3-byte runes: 128 is not a multiple of 3, so the limit falls inside one
	text := strings.Repeat("가", 100)
	cut, wasCut := kvTokenizeBound(text, maxTokens)
	if !wasCut {
		t.Fatal("expected a cut")
	}
	if !utf8.ValidString(cut) {
		t.Fatalf("cut text is not valid UTF-8 (%d bytes)", len(cut))
	}
	if len(cut) != limit-(limit%3) {
		t.Fatalf("cut at %d bytes, want %d (the last whole rune under the bound)", len(cut), limit-(limit%3))
	}
	short, wasCut := kvTokenizeBound("short", maxTokens)
	if wasCut || short != "short" {
		t.Fatalf("a text under the bound must come back untouched (%q, cut=%v)", short, wasCut)
	}
	same, wasCut := kvTokenizeBound(text[:limit], maxTokens)
	if wasCut || same != text[:limit] {
		t.Fatal("a text exactly at the bound is not cut")
	}
}

func TestKvTokenizeBoundCutGuard(t *testing.T) {
	// A tokenizer so compact that the bounded prefix yields FEWER ids than
	// maxTokens (one id per 32 bytes): the ids nearest the cut are
	// boundary-dependent and must be dropped.
	tok := withByteTokenizer(t, 1)
	const maxTokens = 4096
	limit := maxTokens * kvTokenizeBytesPerTokenDefault

	// Emulate a 32-bytes-per-token encode by having the tokenizer see the
	// bound but answer limit/32 ids.
	compact := &compactTokenizer{ratio: 32}
	KvRegisterTokenizerBackend(&compactBackend{tok: compact})
	KvTokenizerPoolReset()
	_ = tok

	long := strings.Repeat("y", limit+5000)
	ids := kvTokenizeWithCache(long, "compact-model", maxTokens, true)
	want := limit/32 - kvTokenizeCutGuard
	if len(ids) != want {
		t.Fatalf("cut text: ids = %d, want %d (%d from the bounded prefix minus the %d guard)",
			len(ids), want, limit/32, kvTokenizeCutGuard)
	}

	// The same compact tokenizer on a text UNDER the bound keeps every id:
	// nothing was cut, so no id is boundary-dependent.
	KvTokenCacheReset()
	short := strings.Repeat("y", limit/2)
	ids = kvTokenizeWithCache(short, "compact-model", maxTokens, true)
	if len(ids) != (limit/2)/32 {
		t.Fatalf("uncut text: ids = %d, want %d (no guard applied)", len(ids), (limit/2)/32)
	}
}

type compactTokenizer struct{ ratio int }

func (c *compactTokenizer) Encode(text string, addSpecialTokens bool) []uint32 {
	n := len(text) / c.ratio
	ids := make([]uint32, n)
	for i := range ids {
		ids[i] = uint32(i + 1)
	}
	return ids
}

func (c *compactTokenizer) Close() {}

type compactBackend struct{ tok *compactTokenizer }

func (b *compactBackend) LoadModel(tokenizerPath string) KvTokenizer { return b.tok }
func (b *compactBackend) Name() string                               { return "compact-mock" }
