package mail

import (
	"context"
	"sync"
)

// Fake records messages instead of sending them (tests). FailNext makes the
// next n sends fail.
type Fake struct {
	mu       sync.Mutex
	sent     []Message
	failNext int
}

// Send implements Mailer.
func (f *Fake) Send(_ context.Context, m Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext > 0 {
		f.failNext--
		return errFake
	}
	f.sent = append(f.sent, m)
	return nil
}

// FailNext makes the next n sends return an error.
func (f *Fake) FailNext(n int) {
	f.mu.Lock()
	f.failNext = n
	f.mu.Unlock()
}

// Sent returns a copy of the messages sent so far.
func (f *Fake) Sent() []Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Message(nil), f.sent...)
}

type fakeErr struct{}

func (fakeErr) Error() string { return "fake mailer: induced failure" }

var errFake = fakeErr{}
