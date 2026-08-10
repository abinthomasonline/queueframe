package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestConcurrentAdminPasswordChangesOnlyOneOldCredentialSucceeds(t *testing.T) {
	database := testStore(t)
	ctx := context.Background()
	const oldPassword = "initial-admin-password"
	if err := database.BootstrapAdmin(ctx, "admin", oldPassword); err != nil {
		t.Fatal(err)
	}
	admin, err := database.AuthenticateAdmin(ctx, "admin", oldPassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateAdminWebSession(ctx, admin.ID, time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}

	passwords := []string{"first-replacement-password", "second-replacement-password"}
	start := make(chan struct{})
	errorsByPassword := make([]error, len(passwords))
	var wait sync.WaitGroup
	for index, password := range passwords {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			errorsByPassword[index] = database.ChangeAdminPassword(ctx, admin.ID, oldPassword, password, time.Now())
		}()
	}
	close(start)
	wait.Wait()

	successes := 0
	winner := ""
	for index, err := range errorsByPassword {
		if err == nil {
			successes++
			winner = passwords[index]
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent password changes = %d, errors = %#v", successes, errorsByPassword)
	}
	if _, err := database.AuthenticateAdmin(ctx, "admin", winner); err != nil {
		t.Fatalf("winning password does not authenticate: %v", err)
	}
	if _, err := database.AuthenticateAdmin(ctx, "admin", oldPassword); !errors.Is(err, ErrInvalidAdminCredentials) {
		t.Fatalf("old password authentication error = %v, want invalid credentials", err)
	}
	if count, err := database.CountAdminSessions(ctx, admin.ID, time.Now()); err != nil || count != 0 {
		t.Fatalf("sessions after password race = %d, %v", count, err)
	}
}

func TestConcurrentOldPasswordLoginAndChangeCannotLeaveSession(t *testing.T) {
	database := testStore(t)
	ctx := context.Background()
	const oldPassword = "initial-admin-password"
	const newPassword = "replacement-admin-password"
	if err := database.BootstrapAdmin(ctx, "admin", oldPassword); err != nil {
		t.Fatal(err)
	}
	admin, err := database.AuthenticateAdmin(ctx, "admin", oldPassword)
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	type loginResult struct {
		token string
		err   error
	}
	loginDone := make(chan loginResult, 1)
	changeDone := make(chan error, 1)
	go func() {
		<-start
		_, token, err := database.AuthenticateAdminAndCreateWebSession(ctx, "admin", oldPassword, time.Hour, time.Now())
		loginDone <- loginResult{token: token, err: err}
	}()
	go func() {
		<-start
		changeDone <- database.ChangeAdminPassword(ctx, admin.ID, oldPassword, newPassword, time.Now())
	}()
	close(start)
	login := <-loginDone
	if err := <-changeDone; err != nil {
		t.Fatalf("password change failed: %v", err)
	}
	if login.err != nil && !errors.Is(login.err, ErrInvalidAdminCredentials) {
		t.Fatalf("concurrent login failed unexpectedly: %v", login.err)
	}
	if login.err == nil {
		if _, err := database.AdminByWebSession(ctx, login.token, time.Now()); err == nil {
			t.Fatal("old-password login left a session alive after password change")
		}
	}
	if count, err := database.CountAdminSessions(ctx, admin.ID, time.Now()); err != nil || count != 0 {
		t.Fatalf("sessions after login/password interleaving = %d, %v", count, err)
	}
	if _, err := database.AuthenticateAdmin(ctx, "admin", newPassword); err != nil {
		t.Fatalf("replacement password does not authenticate: %v", err)
	}
}
