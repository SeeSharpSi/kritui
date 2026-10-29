package app

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"

	"seesharpsi/kritui/templ"

	kritui_db "seesharpsi/kritui/db"
)

const (
	authInvalidCredentialsMessage = "Username or password is incorrect."
	authMissingCredentialsMessage = "Enter your username and password."
	authUsernameTakenMessage      = "That username is already taken."
)

func (a *authService) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	setAuthPageHeaders(w)
	if _, active, err := a.sessionUser(r); err != nil {
		a.authInternalError(w, "check login session", err)
		return
	} else if active {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.renderAuthPage(r.Context(), w, http.StatusOK, templates.AuthPageData{
		Signup: false,
		Next:   safeNextPath(r.URL.Query().Get("next")),
	})
}

func (a *authService) handleSignupPage(w http.ResponseWriter, r *http.Request) {
	setAuthPageHeaders(w)
	if _, active, err := a.sessionUser(r); err != nil {
		a.authInternalError(w, "check signup session", err)
		return
	} else if active {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.renderAuthPage(r.Context(), w, http.StatusOK, templates.AuthPageData{
		Signup: true,
		Next:   safeNextPath(r.URL.Query().Get("next")),
	})
}

func (a *authService) handleLogin(w http.ResponseWriter, r *http.Request) {
	setAuthPageHeaders(w)
	if !a.allowAttempt(a.loginLimiter, w, r) {
		return
	}
	if !parseAuthForm(w, r) {
		return
	}

	username := r.PostForm.Get("username")
	password := r.PostForm.Get("password")
	next := authFormNext(r)
	if username == "" || password == "" {
		a.renderAuthPage(r.Context(), w, http.StatusBadRequest, templates.AuthPageData{
			Signup:   false,
			Username: username,
			Error:    authMissingCredentialsMessage,
			Next:     next,
		})
		return
	}

	user, err := kritui_db.GetUserByUsername(r.Context(), a.database, username)
	switch {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		// Perform bounded password work for an unknown or disabled username so
		// response timing does not reveal whether the account exists.
		verifyPassword(authDummyPasswordHash(), password)
		a.renderAuthPage(r.Context(), w, http.StatusUnauthorized, templates.AuthPageData{
			Signup:   false,
			Username: username,
			Error:    authInvalidCredentialsMessage,
			Next:     next,
		})
		return
	default:
		a.authInternalError(w, "look up login user", err)
		return
	}

	verified := false
	if user.PasswordHash != "" {
		verified = verifyPassword(user.PasswordHash, password)
	} else {
		verifyPassword(authDummyPasswordHash(), password)
	}
	if !verified {
		a.renderAuthPage(r.Context(), w, http.StatusUnauthorized, templates.AuthPageData{
			Signup:   false,
			Username: username,
			Error:    authInvalidCredentialsMessage,
			Next:     next,
		})
		return
	}

	if err := a.rotateSession(w, r, user.ID); err != nil {
		a.authInternalError(w, "start login session", err)
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (a *authService) handleSignup(w http.ResponseWriter, r *http.Request) {
	setAuthPageHeaders(w)
	if !a.allowAttempt(a.signupLimiter, w, r) {
		return
	}
	if !parseAuthForm(w, r) {
		return
	}

	username := r.PostForm.Get("username")
	password := r.PostForm.Get("password")
	next := authFormNext(r)

	if err := kritui_db.ValidateUsername(username); err != nil {
		a.renderAuthPage(r.Context(), w, http.StatusBadRequest, templates.AuthPageData{
			Signup:   true,
			Username: username,
			Error:    err.Error(),
			Next:     next,
		})
		return
	}
	if err := validatePassword(password); err != nil {
		a.renderAuthPage(r.Context(), w, http.StatusBadRequest, templates.AuthPageData{
			Signup:   true,
			Username: username,
			Error:    err.Error(),
			Next:     next,
		})
		return
	}

	encoded, err := hashPassword(password)
	if err != nil {
		a.authInternalError(w, "hash signup password", err)
		return
	}
	user, err := kritui_db.CreateUser(r.Context(), a.database, username, encoded)
	if errors.Is(err, kritui_db.ErrUsernameTaken) {
		a.renderAuthPage(r.Context(), w, http.StatusConflict, templates.AuthPageData{
			Signup:   true,
			Username: username,
			Error:    authUsernameTakenMessage,
			Next:     next,
		})
		return
	}
	if err != nil {
		a.authInternalError(w, "create signup user", err)
		return
	}

	if err := a.rotateSession(w, r, user.ID); err != nil {
		a.authInternalError(w, "start signup session", err)
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (a *authService) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(authSessionCookieName); err == nil && validSessionToken(cookie.Value) {
		if err := kritui_db.DeleteSession(r.Context(), a.database, hashSessionToken(cookie.Value)); err != nil && !errors.Is(err, sql.ErrNoRows) {
			// Clear the browser credential even though the stored session could
			// not be revoked, and report a generic server failure.
			a.clearSessionCookie(w, r)
			a.authInternalError(w, "revoke logout session", err)
			return
		}
	}
	a.clearSessionCookie(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func authFormNext(r *http.Request) string {
	next := r.PostForm.Get("next")
	if next == "" {
		next = r.URL.Query().Get("next")
	}
	return safeNextPath(next)
}

func setAuthPageHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
}

func (a *authService) renderAuthPage(ctx context.Context, w http.ResponseWriter, status int, data templates.AuthPageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := templates.AuthPage(data).Render(ctx, w); err != nil {
		log.Printf("auth: render account page: %v", err)
	}
}

func (a *authService) authInternalError(w http.ResponseWriter, operation string, err error) {
	log.Printf("auth: %s: %v", operation, err)
	http.Error(w, "Internal Server Error", http.StatusInternalServerError)
}

func parseAuthForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, authFormMaxBytes)
	if err := r.ParseForm(); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "Request body is too large.", http.StatusRequestEntityTooLarge)
			return false
		}
		http.Error(w, "Invalid form submission.", http.StatusBadRequest)
		return false
	}
	return true
}
