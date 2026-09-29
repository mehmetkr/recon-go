package match

// MinRefLen is the shortest reference worth comparing.
const MinRefLen = 5

// References must agree to at least four parts in five.
const (
	ThresholdNum = 4
	ThresholdDen = 5
)

// simScore describes how closely two references agree, even inside longer text.
type simScore struct {
	Evidence bool // false when either reference is too short
	Dist     int  // edits needed to find one inside the other
	Len      int  // length of the shorter reference
}

// scoreRefs compares two references, in either order.
func scoreRefs(a, b string) simScore {
	if len(a) < MinRefLen || len(b) < MinRefLen {
		return simScore{}
	}
	pattern, text := a, b
	if len(b) < len(a) || (len(b) == len(a) && b < a) {
		pattern, text = b, a
	}
	return simScore{Evidence: true, Dist: semiGlobal(pattern, text), Len: len(pattern)}
}

// semiGlobal counts the edits needed to find a reference inside a longer text.
func semiGlobal(pattern, text string) int {
	prev := make([]int, len(text)+1) // the match may start anywhere
	cur := make([]int, len(text)+1)
	for i := 1; i <= len(pattern); i++ {
		cur[0] = i // every character of the reference counts
		for j := 1; j <= len(text); j++ {
			sub := prev[j-1]
			if pattern[i-1] != text[j-1] {
				sub++
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, sub)
		}
		prev, cur = cur, prev
	}
	best := prev[0]
	for _, v := range prev[1:] { // the match may end anywhere
		best = min(best, v)
	}
	return best
}

// strong reports whether two references agree closely enough.
func (s simScore) strong() bool {
	return s.Evidence && ThresholdDen*s.Dist <= (ThresholdDen-ThresholdNum)*s.Len
}

// simKey ranks agreement, so equal agreement always ties.
type simKey struct{ Dist, Len int }

// noEvidence ranks after any real agreement, so weak likeness never decides.
var noEvidence = simKey{Dist: 1, Len: 0}

// tieKey returns the ranking key for this agreement.
func (s simScore) tieKey() simKey {
	if !s.strong() {
		return noEvidence
	}
	g := gcd(s.Dist, s.Len)
	return simKey{s.Dist / g, s.Len / g}
}

// compare ranks keys from closest agreement to none.
func (k simKey) compare(o simKey) int {
	switch {
	case k == o:
		return 0
	case k == noEvidence:
		return 1
	case o == noEvidence:
		return -1
	}
	// Both keys are real agreements, so the comparison is exact.
	l, r := k.Dist*o.Len, o.Dist*k.Len
	switch {
	case l < r:
		return -1
	case l > r:
		return 1
	}
	return 0
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
