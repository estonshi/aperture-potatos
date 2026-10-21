package kernel

import (
	"crypto/rand"
	"encoding/binary"
	"time"
)

// Clock is the kernel's only source of wall time, so tests can fix every timestamp; timers still use the time package and are made deterministic with testing/synctest.
type Clock interface {
	Now() time.Time
}

// IDGenerator mints ids for sessions, turns, messages and the other kernel objects.
type IDGenerator interface {
	// NewID returns prefix, an underscore and a suffix unique within the process, e.g. NewID("msg") = "msg_01k6…".
	NewID(prefix string) string
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// SystemClock reads the wall clock in UTC.
func SystemClock() Clock { return systemClock{} }

type randomIDs struct{}

const crockford = "0123456789abcdefghjkmnpqrstvwxyz"

// NewID uses a millisecond timestamp before 80 random bits, so ids of one kind sort roughly by creation time.
func (randomIDs) NewID(prefix string) string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], uint64(time.Now().UnixMilli())<<16)
	_, _ = rand.Read(b[6:])
	hi, lo := binary.BigEndian.Uint64(b[:8]), binary.BigEndian.Uint64(b[8:])
	var s [26]byte
	for i := 25; i >= 0; i-- {
		s[i] = crockford[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return prefix + "_" + string(s[:])
}

// RandomIDs is the default IDGenerator.
func RandomIDs() IDGenerator { return randomIDs{} }
