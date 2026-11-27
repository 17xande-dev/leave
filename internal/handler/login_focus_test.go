package handler

import (
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// inputTag returns the opening tag of the form control with the given name.
func inputTag(t *testing.T, body, name string) string {
	t.Helper()
	m := regexp.MustCompile(`<input[^>]*name="` + name + `"[^>]*>`).FindString(body)
	if m == "" {
		t.Fatalf("no %q input in page", name)
	}
	return m
}

func hasAutofocus(tag string) bool { return strings.Contains(tag, " autofocus") }

// The sign-in page puts focus where the next keystroke belongs: the username
// when it is empty, the password once a username is being kept.
func TestLoginFocusOnFirstVisit(t *testing.T) {
	hs := newHarness(t)
	_, body := hs.get(hs.client(), "/login")
	if !hasAutofocus(inputTag(t, body, "username")) {
		t.Error("GET /login: username has no autofocus")
	}
	if hasAutofocus(inputTag(t, body, "password")) {
		t.Error("GET /login: password has autofocus")
	}
}

func TestLoginFocusAfterRefusal(t *testing.T) {
	const secret = "s3cret-pw-not-for-echo"
	const name = `EXAMPLE\Staff<1>`
	hs := newHarness(t)

	// markup returns the focus-relevant markup of a refused sign-in.
	refused := func(t *testing.T, username, password string, wantStatus int) string {
		t.Helper()
		r, body := hs.post(hs.client(), "/login", url.Values{"username": {username}, "password": {password}})
		if r.StatusCode != wantStatus {
			t.Fatalf("status %d, want %d", r.StatusCode, wantStatus)
		}
		if strings.Contains(body, secret) {
			t.Error("submitted password appears in the page")
		}
		user, pass := inputTag(t, body, "username"), inputTag(t, body, "password")
		if hasAutofocus(user) {
			t.Error("username has autofocus after a refusal")
		}
		if !hasAutofocus(pass) {
			t.Error("password has no autofocus after a refusal")
		}
		if !strings.Contains(user, `value="`+template.HTMLEscapeString(username)+`"`) {
			t.Errorf("username not kept and escaped: %s", user)
		}
		if strings.Contains(pass, "value=") {
			t.Errorf("password carries a value: %s", pass)
		}
		return user + pass
	}

	t.Run("unknown user", func(t *testing.T) {
		refused(t, name, secret, http.StatusUnauthorized)
	})

	t.Run("known user wrong password has the same focus markup", func(t *testing.T) {
		a := refused(t, "nobody", "", http.StatusUnauthorized)
		b := refused(t, "staff1", "", http.StatusUnauthorized)
		if strings.ReplaceAll(a, "nobody", "staff1") != b {
			t.Errorf("markup differs:\n%s\n%s", a, b)
		}
	})

	t.Run("rate limited", func(t *testing.T) {
		c := hs.client()
		var body string
		var status int
		for range 6 {
			r, b := hs.post(c, "/login", url.Values{"username": {"ghost"}, "password": {secret}})
			status, body = r.StatusCode, b
		}
		if status != http.StatusTooManyRequests {
			t.Fatalf("status %d, want 429", status)
		}
		if strings.Contains(body, secret) {
			t.Error("password echoed on 429")
		}
		if hasAutofocus(inputTag(t, body, "username")) || !hasAutofocus(inputTag(t, body, "password")) {
			t.Error("429 page focus is not on the password")
		}
	})

	t.Run("overlong username is not kept so the username takes focus", func(t *testing.T) {
		r, body := hs.post(hs.client(), "/login", url.Values{"username": {strings.Repeat("a", maxUsernameBytes+1)}, "password": {secret}})
		if r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status %d", r.StatusCode)
		}
		if !hasAutofocus(inputTag(t, body, "username")) || hasAutofocus(inputTag(t, body, "password")) {
			t.Error("with nothing kept, focus must be on the username")
		}
	})
}
