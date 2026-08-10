package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestQueueframeCanonicalDatabasePathResolvesParentSymlink(t *testing.T) {
	root := t.TempDir()
	realDirectory := filepath.Join(root, "real")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(realDirectory, alias); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}

	got, err := canonicalDatabasePath(filepath.Join(alias, "orders.db"))
	if err != nil {
		t.Fatal(err)
	}
	resolvedDirectory, err := filepath.EvalSymlinks(realDirectory)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(resolvedDirectory, "orders.db")
	if got != want {
		t.Fatalf("canonical path = %q, want %q", got, want)
	}
}

func TestWaitForSchedulerDoesNotReturnWhileSchedulerIsActive(t *testing.T) {
	done := make(chan error, 1)
	release := make(chan struct{})
	schedulerErr := errors.New("scheduler finished")
	go func() {
		<-release
		time.Sleep(40 * time.Millisecond)
		done <- schedulerErr
	}()

	started := time.Now()
	close(release)
	gotSchedulerErr, drainErr := waitForScheduler(done, 5*time.Millisecond)
	if !errors.Is(gotSchedulerErr, schedulerErr) {
		t.Fatalf("scheduler error = %v, want %v", gotSchedulerErr, schedulerErr)
	}
	if drainErr == nil {
		t.Fatal("expected missed drain deadline to be reported")
	}
	if elapsed := time.Since(started); elapsed < 30*time.Millisecond {
		t.Fatalf("wait returned while scheduler was active after %s", elapsed)
	}
}

func TestWaitForSchedulerReturnsNormallyBeforeDeadline(t *testing.T) {
	done := make(chan error, 1)
	done <- context.Canceled
	gotSchedulerErr, drainErr := waitForScheduler(done, time.Second)
	if !errors.Is(gotSchedulerErr, context.Canceled) || drainErr != nil {
		t.Fatalf("waitForScheduler() = (%v, %v)", gotSchedulerErr, drainErr)
	}
}
