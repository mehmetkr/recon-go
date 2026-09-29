package match

import (
	"math/rand/v2"
	"slices"
	"testing"
)

func TestSimilarityVectors(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want simScore
	}{
		{"PAYMENTREFINV1042ACME", "INV1042", simScore{true, 0, 7}}, // contained
		{"ABCDE", "ABXDE", simScore{true, 1, 5}},
		{"ABCDE", "ABXCDE", simScore{true, 1, 5}},
		{"ABCDE", "ABDEX", simScore{true, 1, 5}},
		{"INV10023", "INV10X23", simScore{true, 1, 8}},
		{"INV10023", "INV100023", simScore{true, 1, 8}},
		{"INV10023", "INV1023", simScore{true, 1, 7}},
		{"INV10023", "INV1002", simScore{true, 0, 7}},
		{"ZBCDE", "BCDEQQ", simScore{true, 1, 5}}, // every character of the reference counts
		{"INV10023", "INV19923", simScore{true, 2, 8}},
		{"INV10023", "INV1X0023", simScore{true, 1, 8}},
		{"ABCDEFGHIJ", "ABCDEGHIJ", simScore{true, 1, 9}},
		{"ABAAA", "BAABA", simScore{true, 2, 5}}, // same length: the smaller one is the reference
		{"AAZZ9", "900000", simScore{true, 4, 5}},
		{"INV10023", "XINV10023Y", simScore{true, 0, 8}}, // extra text around it is free
		{"ABCD", "ABCDEFGH", simScore{}},                 // too short to count
		{"", "", simScore{}},
	} {
		if got, back := scoreRefs(tt.a, tt.b), scoreRefs(tt.b, tt.a); got != tt.want || back != tt.want {
			t.Errorf("scoreRefs(%q, %q) = %+v, reversed %+v; want %+v", tt.a, tt.b, got, back, tt.want)
		}
	}
	// Comparing in either direction must give the same answer.
	for _, p := range [][2]string{{"ABCDEFGHIJ", "ABXDEFGHYJ"}, {"QWERTY", "WERTYQ"}, {"AAAAB", "BAAAA"}} {
		if a, b := scoreRefs(p[0], p[1]), scoreRefs(p[1], p[0]); a != b {
			t.Errorf("scoreRefs(%q, %q) = %+v but reversed = %+v", p[0], p[1], a, b)
		}
	}
}

// levenshtein is the classic edit distance, used here as a yardstick.
func levenshtein(a, b string) int {
	d := make([][]int, len(a)+1)
	for i := range d {
		d[i] = make([]int, len(b)+1)
		d[i][0] = i
	}
	for j := range d[0] {
		d[0][j] = j
	}
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			sub := d[i-1][j-1]
			if a[i-1] != b[j-1] {
				sub++
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, sub)
		}
	}
	return d[len(a)][len(b)]
}

// TestSimilarityMatchesBruteForce checks the fast comparison against a slow, obvious one.
func TestSimilarityMatchesBruteForce(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 5))
	randRef := func() string {
		b := make([]byte, 5+rng.IntN(8))
		for i := range b {
			b[i] = "ABC9"[rng.IntN(4)]
		}
		return string(b)
	}
	for range 5000 {
		a, b := randRef(), randRef()
		pattern, text := a, b
		if len(b) < len(a) || (len(b) == len(a) && b < a) {
			pattern, text = b, a
		}
		want := len(pattern)
		for i := 0; i <= len(text); i++ {
			for j := i; j <= len(text); j++ {
				want = min(want, levenshtein(pattern, text[i:j]))
			}
		}
		if got := scoreRefs(a, b); got != (simScore{true, want, len(pattern)}) {
			t.Fatalf("scoreRefs(%q, %q) = %+v, want %d edits in %d", a, b, got, want, len(pattern))
		}
	}
}

func TestStrengthAndKey(t *testing.T) {
	for _, tt := range []struct {
		sim    simScore
		strong bool
		key    simKey
	}{
		{simScore{true, 0, 7}, true, simKey{0, 1}},
		{simScore{true, 0, 5}, true, simKey{0, 1}},
		{simScore{true, 1, 5}, true, simKey{1, 5}},  // 0.8
		{simScore{true, 2, 10}, true, simKey{1, 5}}, // 0.8, same key as one in five
		{simScore{true, 1, 4}, false, noEvidence},   // 0.75
		{simScore{true, 2, 9}, false, noEvidence},
		{simScore{true, 2, 8}, false, noEvidence},
		{simScore{true, 7, 8}, false, noEvidence},
		{simScore{}, false, noEvidence}, // too short to count
	} {
		if s, k := tt.sim.strong(), tt.sim.tieKey(); s != tt.strong || k != tt.key {
			t.Errorf("%+v: strong %v, key %+v; want %v, %+v", tt.sim, s, k, tt.strong, tt.key)
		}
	}
}

func TestKeyOrder(t *testing.T) {
	keys := []simKey{
		simScore{true, 0, 7}.tieKey(), simScore{true, 1, 5}.tieKey(), simScore{true, 2, 10}.tieKey(),
		simScore{true, 1, 8}.tieKey(), simScore{true, 1, 11}.tieKey(), noEvidence, simScore{}.tieKey(),
		simScore{true, 3, 8}.tieKey(),
	}
	for _, a := range keys {
		for _, b := range keys {
			if a.compare(b) != -b.compare(a) || (a.compare(b) == 0) != (a == b) {
				t.Errorf("compare(%v, %v) is inconsistent", a, b)
			}
			for _, c := range keys {
				if a.compare(b) <= 0 && b.compare(c) <= 0 && a.compare(c) > 0 {
					t.Errorf("order is not transitive: %v, %v, %v", a, b, c)
				}
			}
		}
	}
	slices.SortFunc(keys, simKey.compare)
	if want := []simKey{{0, 1}, {1, 11}, {1, 8}, {1, 5}, {1, 5}, noEvidence, noEvidence, noEvidence}; !slices.Equal(keys, want) {
		t.Errorf("sorted = %v, want %v", keys, want)
	}
}
