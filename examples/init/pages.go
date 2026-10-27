package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Paths of the application's pages. They are the application's own routes,
// outside behemoth's base path: the pages the mailed links point to, and
// the page that signs in with a provider. The providers' callback page is
// at pathOAuthCallback (oauth.go).
const (
	pathMagicPage = "/auth/magic"
	pathResetPage = "/auth/reset"
	pathOAuthPage = "/auth/oauth"
)

// mountPages adds the two pages a mailed link opens, and with
// oauthProviders the page that signs in with a provider and the page the
// providers send the browser back to.
//
// A link never points at behemoth. It points at a page of the application,
// which posts the link's token to behemoth. Mail scanners and link previews
// open the links in a message with a GET; if that were enough to sign in or
// to reset a password, the scanner would use the link up before the user
// got to it. A provider sends the browser to a page of the application for
// a reason of its own: every call to behemoth stays a JSON POST, which
// answers a sign-in the way the other sign-in routes do.
func mountPages(engine *gin.Engine, oauthProviders []string) {
	page := func(html string) gin.HandlerFunc {
		return func(c *gin.Context) { c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(html)) }
	}
	engine.GET(pathMagicPage, page(magicPage))
	engine.GET(pathResetPage, page(resetPage))
	if len(oauthProviders) == 0 {
		return
	}
	ids, _ := json.Marshal(oauthProviders) // provider ids are [a-z0-9_-]
	engine.GET(pathOAuthPage, page(strings.Replace(oauthPage, "PROVIDERS", string(ids), 1)))
	engine.GET(pathOAuthCallback+"/:provider", page(callbackPage))
}

// oauthPage starts a sign-in or a link with a provider, and lists and
// unlinks the signed-in user's provider accounts. The example delivers
// session tokens in the Set-Auth-Token header, so the page keeps the token
// in sessionStorage and sends it as a bearer token; a token from another
// sign-in, such as one by password with curl, can be pasted in. A link
// needs a session that signed in within the last 15 minutes.
//
// Both starts answer with the provider's authorization URL and a binding
// value. The page keeps the binding value for the callback page, where no
// URL carries it, and sends the browser to the provider.
const oauthPage = `<!doctype html>
<meta charset="utf-8">
<title>Sign in with a provider</title>
<h1>Sign in with a provider</h1>
<p>Session token: <input id="session" size="70" placeholder="none: sign in below, or paste one"></p>
<div id="providers"></div>
<button id="accounts">My provider accounts</button>
<pre id="out"></pre>
<script>
const providers = PROVIDERS;
const session = document.getElementById("session");
session.value = sessionStorage.getItem("session") ?? "";
session.onchange = () => sessionStorage.setItem("session", session.value);
const out = document.getElementById("out");
const show = async (res) => { out.textContent = res.status + " " + JSON.stringify(await res.json(), null, 2); };
const call = (method, path, body) => {
  const headers = {"Content-Type": "application/json"};
  if (session.value) headers.Authorization = "Bearer " + session.value;
  return fetch("/api/auth" + path, {method, headers, body: body && JSON.stringify(body)});
};
const start = async (path, provider) => {
  const res = await call("POST", path, {provider, redirect_url: "/auth/oauth"});
  if (!res.ok) return show(res);
  const begun = await res.json();
  sessionStorage.setItem("oauth_binding", begun.binding);
  location.assign(begun.authorization_url);
};
for (const provider of providers) {
  const row = document.createElement("p");
  for (const [label, action] of [
    ["Sign in with " + provider, () => start("/sign-in/oauth", provider)],
    ["Link " + provider, () => start("/oauth/link", provider)],
    ["Unlink " + provider, async () => show(await call("POST", "/oauth/unlink", {provider}))],
  ]) {
    const button = document.createElement("button");
    button.textContent = label;
    button.onclick = action;
    row.append(button, " ");
  }
  document.getElementById("providers").append(row);
}
document.getElementById("accounts").onclick = async () => show(await call("GET", "/oauth/accounts"));
</script>
`

// callbackPage is where a provider sends the browser back, at
// /auth/callback/<provider id>. It takes the code out of the address bar and
// posts it, with the state and the binding value, to POST
// /api/auth/oauth/complete. A sign-in answers with the user and a new
// session token, which the page keeps. A link answers with the account it
// linked, and needs the session that started it, which the page sends.
const callbackPage = `<!doctype html>
<meta charset="utf-8">
<title>Signing in</title>
<h1>Signing in</h1>
<pre id="out"></pre>
<p><a href="/auth/oauth">Back</a></p>
<script>
(async () => {
  const out = document.getElementById("out");
  const provider = location.pathname.split("/").pop();
  const query = new URLSearchParams(location.search);
  history.replaceState(null, "", location.pathname);
  if (!query.get("code")) {
    out.textContent = "the provider sent no code: " + (query.get("error") ?? "");
    return;
  }
  const headers = {"Content-Type": "application/json"};
  const session = sessionStorage.getItem("session");
  if (session) headers.Authorization = "Bearer " + session;
  const res = await fetch("/api/auth/oauth/complete", {
    method: "POST", headers,
    body: JSON.stringify({
      provider, code: query.get("code"), state: query.get("state"), iss: query.get("iss") ?? undefined,
      binding: sessionStorage.getItem("oauth_binding") ?? undefined,
    }),
  });
  sessionStorage.removeItem("oauth_binding");
  const token = res.headers.get("Set-Auth-Token");
  if (token) sessionStorage.setItem("session", token);
  out.textContent = res.status + " " + JSON.stringify(await res.json(), null, 2);
})();
</script>
`

// magicPage finishes a magic link sign-in: the button posts the token to
// POST /api/auth/magic-link/verify. The example delivers session tokens in
// the Set-Auth-Token response header (httpapi.DeliveryHeader), so the page
// reads it from there; with a cookie delivery the browser would keep it.
const magicPage = `<!doctype html>
<meta charset="utf-8">
<title>Sign in</title>
<h1>Sign in</h1>
<button id="go">Sign me in</button>
<pre id="out"></pre>
<script>
const token = new URLSearchParams(location.search).get("token");
document.getElementById("go").onclick = async () => {
  const res = await fetch("/api/auth/magic-link/verify", {
    method: "POST",
    headers: {"Content-Type": "application/json"},
    body: JSON.stringify({token}),
  });
  const body = await res.json();
  const session = res.headers.get("Set-Auth-Token");
  document.getElementById("out").textContent =
    res.status + " " + JSON.stringify(body, null, 2) + (session ? "\nsession token: " + session : "");
};
</script>
`

// resetPage finishes a password reset: it asks for the new password and
// posts it with the token to POST /api/auth/password-reset/confirm. A reset
// creates no session, so the user signs in with the new password afterwards.
const resetPage = `<!doctype html>
<meta charset="utf-8">
<title>Reset your password</title>
<h1>Reset your password</h1>
<form id="form">
  <input id="password" type="password" placeholder="New password" autocomplete="new-password" required>
  <button>Set password</button>
</form>
<pre id="out"></pre>
<script>
const token = new URLSearchParams(location.search).get("token");
document.getElementById("form").onsubmit = async (event) => {
  event.preventDefault();
  const res = await fetch("/api/auth/password-reset/confirm", {
    method: "POST",
    headers: {"Content-Type": "application/json"},
    body: JSON.stringify({token, password: document.getElementById("password").value}),
  });
  document.getElementById("out").textContent = res.status + " " + JSON.stringify(await res.json(), null, 2);
};
</script>
`
