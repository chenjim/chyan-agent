package memory

import (
	"crypto/rand"
	"strconv"
	"strings"
	"time"
)

// newID returns a time-ordered short base36 id like "m1a2b3": "m" plus the
// base36 Unix millisecond padded to 10 chars plus 3 random base36 chars. The
// fixed-width time part keeps lexicographic order equal to creation order,
// and the random suffix makes same-millisecond ids unique.
func newID(t time.Time) string {
	tp := strconv.FormatUint(uint64(t.UnixMilli()), 36)
	if len(tp) < 10 {
		tp = strings.Repeat("0", 10-len(tp)) + tp
	}
	const letters = "0123456789abcdefghijklmnopqrstuvwxyz"
	var rnd [1]byte
	suffix := make([]byte, 6)
	for i := range suffix {
		if _, err := rand.Read(rnd[:]); err != nil {
			panic("memory: entropy unavailable: " + err.Error())
		}
		suffix[i] = letters[int(rnd[0])%36]
	}
	return "m" + tp + string(suffix)
}
