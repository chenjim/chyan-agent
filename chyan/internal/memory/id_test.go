package memory

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

var idPattern = regexp.MustCompile(`^m[0-9a-z]{16}$`)

func TestNewIDFormat(t *testing.T) {
	id := newID(time.UnixMilli(1760000000000))
	if !idPattern.MatchString(id) {
		t.Fatalf("id %q does not match %s", id, idPattern)
	}
}

func TestNewIDTimeOrdered(t *testing.T) {
	t1 := time.UnixMilli(1760000000000)
	t2 := t1.Add(time.Second)
	if newID(t1) >= newID(t2) {
		t.Fatalf("ids not time ordered: %q >= %q", newID(t1), newID(t2))
	}
}

func TestNewIDUniqueInSameMillisecond(t *testing.T) {
	t0 := time.UnixMilli(1760000000000)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := newID(t0)
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestNewIDSmallTimestampPadded(t *testing.T) {
	id := newID(time.UnixMilli(5))
	if !strings.HasPrefix(id, "m000000000") {
		t.Fatalf("id %q not zero-padded", id)
	}
}
