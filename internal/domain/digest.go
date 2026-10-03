package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// Digest fingerprints a list of values, so different lists never share a fingerprint.
func Digest(n int, fields ...string) string {
	h := sha256.New()
	for _, f := range fields {
		h.Write([]byte(strconv.Itoa(len(f))))
		h.Write([]byte{':'})
		h.Write([]byte(f))
	}
	s := hex.EncodeToString(h.Sum(nil))
	return s[:min(n, len(s))]
}

// MatchID gives each bank and ledger pairing a lasting identity.
func MatchID(bankID, ledgerID string) string {
	return Digest(32, bankID, ledgerID)
}
