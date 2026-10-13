package main

import (
	"testing"
	"time"
)

// The parent is there for three polls, then gone: lost fires exactly once,
// after the change, and never while the parent still answers.
func TestWatchParentFiresOnceWhenParentChanges(t *testing.T) {
	calls := 0
	getppid := func() int {
		calls++
		if calls <= 3 {
			return 4242
		}
		return 1 // re-parented to init
	}
	fired := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		watchParent(4242, getppid, time.Millisecond, func() { fired <- struct{}{} })
		close(done)
	}()
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("lost never fired after the parent changed")
	}
	<-done
	if calls != 4 {
		t.Fatalf("lost fired after %d polls, want 4 (three with the parent present, one without)", calls)
	}
	select {
	case <-fired:
		t.Fatal("lost fired twice")
	default:
	}
}

func TestWatchParentQuietWhileParentLives(t *testing.T) {
	fired := make(chan struct{}, 1)
	go watchParent(7, func() int { return 7 }, time.Millisecond, func() { fired <- struct{}{} })
	select {
	case <-fired:
		t.Fatal("lost fired although getppid still returns the parent")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestEnvIntOrDefault(t *testing.T) {
	t.Setenv("ABOOKIFY_TEST_INT", "31337")
	if got := envIntOrDefault("ABOOKIFY_TEST_INT", 5); got != 31337 {
		t.Fatalf("got %d", got)
	}
	t.Setenv("ABOOKIFY_TEST_INT", "nope")
	if got := envIntOrDefault("ABOOKIFY_TEST_INT", 5); got != 5 {
		t.Fatalf("bad value should fall back, got %d", got)
	}
	if got := envIntOrDefault("ABOOKIFY_TEST_UNSET_X", 9); got != 9 {
		t.Fatalf("unset should fall back, got %d", got)
	}
}
