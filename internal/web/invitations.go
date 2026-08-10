package web

import (
	"net/http"
	"strconv"
	"time"
)

func (a *App) adminInvitationsPage(w http.ResponseWriter, r *http.Request) {
	a.renderAdminInvitations(w, r, "")
}

func (a *App) renderAdminInvitations(w http.ResponseWriter, r *http.Request, generatedURL string) {
	now := time.Now()
	invitations, err := a.store.ListSignupInvitations(r.Context())
	if err != nil {
		http.Error(w, "signup invitations are unavailable", http.StatusServiceUnavailable)
		return
	}
	a.render(w, r, "admin_invites", pageData{
		Title: "Invitations", Admin: adminFrom(r),
		SignupInvitations:  projectSignupInvitations(invitations, now),
		GeneratedInviteURL: generatedURL,
	})
}

func (a *App) createSignupInvitation(w http.ResponseWriter, r *http.Request) {
	days, err := strconv.Atoi(r.Form.Get("expires_in_days"))
	if err != nil || days < 1 || days > 30 {
		a.redirect(w, r, "/admin/invites", "error", "Invitation lifetime must be between 1 and 30 days.")
		return
	}
	admin := adminFrom(r)
	now := time.Now()
	_, rawToken, err := a.store.CreateSignupInvitation(r.Context(), admin.ID, time.Duration(days)*24*time.Hour, now)
	if err != nil {
		a.redirect(w, r, "/admin/invites", "error", "The invitation could not be created.")
		return
	}
	// The raw bearer is used only to build this one response. It is never
	// redirected, audited, logged, stored, or returned by a later list call.
	inviteURL := *a.origin
	inviteURL.Path = "/"
	inviteURL.RawPath = ""
	query := inviteURL.Query()
	query.Set("invite", rawToken)
	inviteURL.RawQuery = query.Encode()
	inviteURL.Fragment = ""
	a.renderAdminInvitations(w, r, inviteURL.String())
}

func (a *App) revokeSignupInvitation(w http.ResponseWriter, r *http.Request) {
	invitationID, err := parseID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid invitation", http.StatusBadRequest)
		return
	}
	admin := adminFrom(r)
	if err := a.store.RevokeSignupInvitation(r.Context(), invitationID, admin.ID, time.Now()); err != nil {
		a.redirect(w, r, "/admin/invites", "error", "That invitation is no longer available to revoke.")
		return
	}
	a.redirect(w, r, "/admin/invites", "notice", "The invitation was revoked.")
}
