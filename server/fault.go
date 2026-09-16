package server

import (
	"log"
	"strconv"
	"strings"
	"sync/atomic"
)

// faultSpec is a test-only corruption injector: the first `remaining`
// responses of chunk (fileIndex, chunkIndex) have their first byte flipped.
// It is constructed exclusively from QRCP_TEST_FAULT="f:c:n".
type faultSpec struct {
	fileIndex  int32
	chunkIndex int32
	remaining  int32
}

// parseFaultSpec parses the fault specification, or returns nil.
func parseFaultSpec(value string) *faultSpec {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ":")
	if len(parts) != 3 {
		log.Printf("qctp: ignoring malformed QRCP_TEST_FAULT %q, want f:c:n", value)
		return nil
	}
	fi, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	ci, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	n, err3 := strconv.Atoi(strings.TrimSpace(parts[2]))
	if err1 != nil || err2 != nil || err3 != nil || fi < 0 || ci < 0 || n <= 0 {
		log.Printf("qctp: ignoring malformed QRCP_TEST_FAULT %q, want f:c:n", value)
		return nil
	}
	return &faultSpec{fileIndex: int32(fi), chunkIndex: int32(ci), remaining: int32(n)}
}

// consume reports whether this response must be corrupted, decrementing the
// remaining budget atomically.
func (f *faultSpec) consume(fileIndex, chunkIndex int) bool {
	if int32(fileIndex) != f.fileIndex || int32(chunkIndex) != f.chunkIndex {
		return false
	}
	for {
		current := atomic.LoadInt32(&f.remaining)
		if current <= 0 {
			return false
		}
		if atomic.CompareAndSwapInt32(&f.remaining, current, current-1) {
			return true
		}
	}
}
