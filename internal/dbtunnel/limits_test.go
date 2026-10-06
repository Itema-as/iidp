package dbtunnel_test

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Itema-as/iidp/internal/dbtunnel"
)

// fakeClock is a Clock whose time moves only when the test advances it.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []waiter
}

type waiter struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, waiter{at: c.now.Add(d), ch: ch})
	return ch
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
			continue
		}
		kept = append(kept, w)
	}
	c.waiters = kept
}

// armed waits until something waits on the clock: Relay has started, and
// timed its first limit.
func (c *fakeClock) armed(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		c.mu.Lock()
		n := len(c.waiters)
		c.mu.Unlock()
		if n > 0 {
			return
		}
	}
	t.Fatal("Relay never timed a limit")
}

// relaying runs Relay between two pipes, and returns the test's ends of
// them: what the client writes and what the database reads.
func relaying(t *testing.T, clock *fakeClock) (client, database net.Conn, ended <-chan dbtunnel.Ended) {
	t.Helper()
	client, tunnelClient := net.Pipe()
	database, tunnelDatabase := net.Pipe()
	t.Cleanup(func() {
		client.Close()
		database.Close()
	})
	out := make(chan dbtunnel.Ended, 1)
	go func() { out <- dbtunnel.Relay(context.Background(), tunnelClient, tunnelDatabase, clock) }()
	clock.armed(t)
	return client, database, out
}

// roundTrip sends a byte from the client to the database and one back,
// which is traffic both ways.
func roundTrip(t *testing.T, client, database net.Conn) {
	t.Helper()
	buf := make([]byte, 1)
	if _, err := client.Write([]byte("Q")); err != nil {
		t.Fatalf("the client cannot write: %v", err)
	}
	if _, err := io.ReadFull(database, buf); err != nil {
		t.Fatalf("the database reads nothing: %v", err)
	}
	if _, err := database.Write([]byte("Z")); err != nil {
		t.Fatalf("the database cannot write: %v", err)
	}
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("the client reads nothing: %v", err)
	}
}

func waitEnded(t *testing.T, ended <-chan dbtunnel.Ended) dbtunnel.Ended {
	t.Helper()
	select {
	case e := <-ended:
		return e
	case <-time.After(10 * time.Second):
		t.Fatal("the session did not end")
		return dbtunnel.Ended{}
	}
}

func stillOpen(t *testing.T, ended <-chan dbtunnel.Ended, when string) {
	t.Helper()
	select {
	case e := <-ended:
		t.Fatalf("%s: the session ended (%s), want it still open", when, e.Reason)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestASessionEndsAfterThirtyMinutesWithoutTraffic(t *testing.T) {
	clock := newFakeClock()
	client, database, ended := relaying(t, clock)

	clock.advance(29 * time.Minute)
	stillOpen(t, ended, "29 idle minutes")
	roundTrip(t, client, database)
	// Traffic starts the 30 minutes again.
	clock.advance(29 * time.Minute)
	stillOpen(t, ended, "29 minutes after traffic")
	clock.advance(time.Minute)

	e := waitEnded(t, ended)
	if e.Reason != dbtunnel.EndedIdle || e.Duration != 59*time.Minute || e.FromClient != 1 || e.ToClient != 1 {
		t.Errorf("ended = %+v, want idle after 59 minutes with a byte each way", e)
	}
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Error("the client's connection is still open")
	}
}

func TestASessionEndsAfterEightHoursHoweverBusy(t *testing.T) {
	clock := newFakeClock()
	client, database, ended := relaying(t, clock)

	for elapsed := time.Duration(0); elapsed < 8*time.Hour-20*time.Minute; elapsed += 20 * time.Minute {
		clock.advance(20 * time.Minute)
		roundTrip(t, client, database)
	}
	stillOpen(t, ended, "7 hours 40 minutes of traffic")
	clock.advance(20 * time.Minute)

	e := waitEnded(t, ended)
	if e.Reason != dbtunnel.EndedMaxSession || e.Duration != 8*time.Hour {
		t.Errorf("ended = %+v, want the 8-hour limit", e)
	}
	if _, err := database.Read(make([]byte, 1)); err == nil {
		t.Error("the database's connection is still open")
	}
}

func TestASessionEndsWithEitherSide(t *testing.T) {
	clock := newFakeClock()
	client, _, ended := relaying(t, clock)
	client.Close()
	if e := waitEnded(t, ended); e.Reason != dbtunnel.EndedByClient {
		t.Errorf("the client closing: %+v", e)
	}

	_, database, ended := relaying(t, clock)
	database.Close()
	if e := waitEnded(t, ended); e.Reason != dbtunnel.EndedByDatabase {
		t.Errorf("the database closing: %+v", e)
	}
}
