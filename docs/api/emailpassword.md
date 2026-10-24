# Email and password

The `emailpassword` plugin adds sign-up, sign-in, and sign-out with an email address and a password. It can also reset a forgotten password by a link sent to the user's address. See [Password reset](#password-reset).

## Setup

Pass the plugin to `Prepare`. It needs no tables of its own: users, accounts, and sessions are core tables.

```go
import "github.com/go-behemoth/behemoth/plugins/emailpassword"

plugin := emailpassword.New(emailpassword.Options{})

app, err := engine.Prepare([]core.Plugin{plugin}, engine.PrepareConfig{})
// ...
ac, err := engine.Boot(ctx, app, db, engine.BootConfig{ /* ... */ })
```

The plugin's name is `emailpassword` (`emailpassword.PluginName`). Use it in `PluginMeta.Dependencies` to build on the plugin, and in `HookOptions.Before`/`After` to order a handler around it.

To let users sign in with a username as well, add the [username](username.md) plugin. For a phone number, with a one-time code or the password, add the [phone number](phonenumber.md) plugin.

A user who signs up starts with `EmailVerified` false. To confirm addresses, add the [email verification](emailverification.md) plugin: it sends the link without any change here, and can refuse sign-ins until the address is confirmed.

## Options

The zero value works. Every field is optional.

| Field | Default | Meaning |
| --- | --- | --- |
| `MinPasswordLength` | 8 | Shortest password accepted at sign-up and at a password reset, in characters. |
| `MaxPasswordLength` | 128 | Longest password accepted at sign-up and at a password reset, in characters. |
| `ValidatePassword` | none | An extra check on a new password, run after the length check. Return an error to reject it. |
| `ValidateEmail` | `utils.IsValidEmail` | The check on the email at sign-up. It receives the address trimmed and lowercased. |
| `Reset` | off | Password reset. Setting `Reset.LinkURL` turns it on; see [Password reset](#password-reset). |

```go
plugin := emailpassword.New(emailpassword.Options{
	MinPasswordLength: 12,
	ValidatePassword: func(password string) error {
		if isBreached(password) {
			return errors.New("this password has appeared in a data breach")
		}
		return nil
	},
})
```

- The rules apply where a password is set: at sign-up and at a password reset. Sign-in does not apply them, so raising `MinPasswordLength` later does not lock out users whose password is shorter.
- A minimum above the maximum, or a negative limit, is a configuration error from `Boot`.
- Passwords are hashed with the hasher configured in `BootConfig.Crypto`. The plugin has no hashing option of its own. See [Bound the password hashes that run at once](#bound-the-password-hashes-that-run-at-once).
- `crypto.Config.Password` sets the Argon2id parameters. A field left at zero takes the value of `crypto.DefaultParams`. A salt or key length under 16 bytes, or a `Memory` under 8 KiB per lane (`Parallelism`), is a configuration error from `Boot`.
- The sign-up response says `invalid email` or `invalid password`, with the code `email_invalid_input` or `password_invalid_input`. The text of the error your function returns is not sent to the client.

## Bound the password hashes that run at once

Sign-up, sign-in, and the password reset each hash a password. A sign-in for an unknown email hashes one too, so that its answer takes as long as a wrong password's. A hash with `crypto.DefaultParams` holds 64 MiB of memory while it runs. `crypto.Config.PasswordLimit` bounds how many hashes run at once in the process, so a burst of requests waits for the hasher instead of exhausting the process's memory:

```go
ac, err := engine.Boot(ctx, app, db, engine.BootConfig{
	Crypto: crypto.Config{
		Secrets: SECRET_SOURCE,
		PasswordLimit: crypto.PasswordLimit{
			MaxConcurrent: 4,               // zero means GOMAXPROCS
			MaxWait:       2 * time.Second, // zero means two seconds
		},
	},
	// ...
})
```

`SECRET_SOURCE` is your `crypto.SecretSource`, such as a `crypto.StaticSecretSource`.

The bound works as follows:

- A request over the bound waits for a running hash to finish. A request that waits longer than `MaxWait` gets `503` with `{"error": "the server is busy, please try again later", "code": "server_busy"}` and a `Retry-After` header of `MaxWait`, rounded up to whole seconds. Called from code, a flow returns an `*errs.DomainError` of category `errs.CategoryUnavailable` with the code `errs.CodeServerBusy`.
- A request whose context ends while it waits gets the context's error. A hash that has started runs to the end.
- A refusal is not a failed sign-in. No failed point fires, so a lockout built on `auth.signIn.failed` does not count it. Sign-in answers `503` for an unknown email as for a known one, so the answer does not show which addresses have an account.
- The memory the running hashes hold stays within `MaxConcurrent` times `Params.Memory`. With Go's default garbage collector settings, the process holds up to about twice that for hashing. Checking a password that was stored with more memory than `Params.Memory` counts for more than one hash.
- More hashes at once add memory and waiting time, and few hashes a second once every CPU is busy.
- The bound holds per process. Each replica of a deployment has its own, so more replicas take more sign-ins at once.
- In a container, `GOMAXPROCS` follows the container's CPU limit. A container without a CPU limit sees every CPU of its host, and the default bound can then need more memory than the container has. Set `MaxConcurrent` from the container's memory limit in that case. For example, with `DefaultParams` and 1 GiB, a bound of 4 holds about 512 MiB (4 × 64 MiB × 2).
- A negative field is a configuration error from `Boot`.

`behemoth.password.wait` and `behemoth.password.busy` show how long requests wait and how many are refused. See [Telemetry](telemetry.md).

## Routes

The routes sit directly under the router's base path.

| Route | Body | Result |
| --- | --- | --- |
| `POST /sign-up/email` | `email`, `password`, and optionally `firstname`, `lastname`, `image_url` | `201` with `{"user": {...}}`. Other fields in the body are not stored by this plugin. A plugin can take one: the [username](username.md) plugin stores `username`. |
| `POST /sign-in/email` | `email`, `password`. Other fields are not used by sign-in, but handlers on `auth.signIn.before` see them. | `200` with `{"user": {...}}`, or `{"status": "requires_second_factor"}` when a second factor is pending. The session token arrives by the configured delivery: a cookie, the `Set-Auth-Token` header, or `token` in this body. |
| `POST /sign-out` | none; needs a valid session | `200` with `{"status": "signed_out"}`, and the session is revoked. With a cookie delivery the response also removes the cookie. |

The user is keyed by column name (for example `id`, `email`, `email_verified`, and `image_url`). [Sessions](sessions.md) describes the token deliveries, the response body, and the user's JSON in full.

With `SessionConfig.CaptureIPAndAgent` on, the session created at sign-in records the client's IP address and user agent. The address is resolved with `RouterConfig.TrustedProxies` and `ClientIPHeader`, so behind a load balancer it is the client's and not the proxy's.

### Error responses

An error response is JSON with an `error` message and, for most errors, a `code`.

| Route | When | Status | Body |
| --- | --- | --- | --- |
| `POST /sign-up/email`, `POST /sign-in/email` | the body is not valid JSON | `400` | `{"error": "request validation error", "code": "request_validation_error"}` |
| `POST /sign-up/email` | `email` is missing, not a string, or refused by `ValidateEmail` | `400` | `{"error": "invalid email", "code": "email_invalid_input"}` |
| `POST /sign-up/email` | `password` is missing or fails the password rules | `400` | `{"error": "invalid password", "code": "password_invalid_input"}` |
| `POST /sign-up/email` | the email already has an account | `409` | `{"error": "an account with this email already exists", "code": "email_taken"}` |
| `POST /sign-in/email` | unknown email, wrong password, or a user who has no password | `401` | `{"error": "invalid email or password", "code": "invalid_credentials"}` |
| `POST /sign-out` | the request has no session token | `401` | `{"error": "missing session token", "code": "session_missing"}` |
| `POST /sign-out` | the token belongs to no session, or its session is revoked or expired | `401` | the message and code of the reason: `session_invalid`, `session_revoked`, `session_expired`. With a cookie delivery the response also removes the cookie. See [Sessions](sessions.md#protect-your-own-routes). |
| any | a hook handler rejected the request with a typed error | the error's status | the error's public message and code |
| any | too many requests. Sign-in and sign-up each allow 10 attempts a minute per client address (`engine.RuleSignInAddress`, `engine.RuleSignUpAddress`) | `429` with a `Retry-After` header: the seconds left until the limit resets | `{"error": "too many requests, please try again later", "code": "rate_limited"}` |
| `POST /sign-up/email`, `POST /sign-in/email` | too many passwords are being hashed at once; see [Bound the password hashes that run at once](#bound-the-password-hashes-that-run-at-once) | `503` with a `Retry-After` header | `{"error": "the server is busy, please try again later", "code": "server_busy"}` |
| any | Behemoth failed (the database is down, for example) | `500` | a generic message; the error's own text is logged, not sent |

Sign-in gives the same answer for an unknown email and a wrong password, so the response does not show which addresses have an account. Sign-up does show it: a `409` says the address is registered. Two sign-ups for the same new address at the same moment get one account, and the other gets the same `409` with `email_taken`.

A unique column another plugin adds to users, such as the username plugin's, gets its own `409` code at sign-up: `username_taken`.

The codes are exported: `emailpassword.ErrorCodeInvalidEmail`, `ErrorCodeInvalidPassword`, `ErrorCodeEmailTaken`, and `ErrorCodeInvalidCredentials`. Branch on the code and not on the message.

A handler on one of the plugin's hook points that rejects a request should return one of the `errs` package's types. An untyped error (`errors.New`) is treated as a failure of the system: the client gets a `500` and the error is logged.

## Hook points

The plugin fires the `auth.signUp.*`, `auth.signIn.*`, and `auth.signOut.*` points. See [Flow points](hooks.md#flow-points-tier-2) for their payloads and failure codes.

## Call the flows from code

The routes are thin wrappers around two methods on the plugin. Use them from a CLI, a job, a test, or another plugin, after `Boot`:

```go
user, err := plugin.SignUp(ctx, behemoth.M{
	"email":    "ada@example.com",
	"password": "correct horse battery",
})

result, err := plugin.SignIn(ctx, emailpassword.EmailAndPasswordCredentials{
	Email: "ada@example.com", Password: "correct horse battery",
})
// result.User, result.Session, result.RawToken
```

The two flows take their input in different forms. Sign-up's fields are open-ended (the profile fields, plus whatever a plugin adds), so it takes a `behemoth.M`. Sign-in needs exactly an email and a password, so it takes a struct, with an `Extra` map for input that only hook handlers read:

```go
result, err := plugin.SignIn(ctx, emailpassword.EmailAndPasswordCredentials{
	Email:    "ada@example.com",
	Password: "correct horse battery",
	Extra:    behemoth.M{"captchaToken": token},
})
```

A handler on `auth.signIn.before` reads it as `payload["captchaToken"]`, next to `payload["email"]`. The route fills `Extra` from the request body, so the same handler works for a request and for a call from code:

```go
reg.OnBefore(hooks.HookSignInBefore, func(hctx *core.HookContext, payload behemoth.M) (behemoth.M, error) {
	token, _ := payload["captchaToken"].(string)
	if !captchaPasses(hctx.Ctx, token) {
		return nil, errs.NewInvalidInputError("signin", "captcha", "captcha check failed", nil)
	}
	return nil, nil
}, nil)
```

Both flows follow these rules:

- They fire the same hook points as the routes, with the same payloads.
- `SignUp` stores `email`, `password`, and the profile fields listed above. Other keys in the input are not stored, but handlers on `auth.signUp.before` see them, which is how a plugin accepts a field of its own (an invite code, for example).
- `Extra` is not stored, and a key in it named `email` or `password` is ignored: the struct's fields are used.
- Pass the context of the request you are handling when there is one. Hook handlers get the request from it, and the session records its IP address and user agent. With any other context, `httpapi.RequestFrom(hctx.Ctx)` is nil and the session has neither.
- `SignIn` returns a `*emailpassword.SignInResult`, which is `core.SignInResult`: the user, the session, the raw token, and `Method` (`"emailpassword"`). Handlers on `auth.signIn.after` receive the same value.
- `SignIn` returns the raw session token and delivers nothing. From a route of your own, hand the result to `httpapi.WriteSignIn`, which is what the plugin's route does. See [Write a sign-in route](sessions.md#write-a-sign-in-route).
- Called before `Boot`, both return a configuration error.
- `SignUp` returns typed errors for its own rejections too: check `errs.IsCode(err, emailpassword.ErrorCodeEmailTaken)`, `ErrorCodeInvalidEmail`, or `ErrorCodeInvalidPassword`.
- `SignIn` returns a typed error for a refused credential, so you can tell it from a failure:

```go
result, err := plugin.SignIn(ctx, creds)
switch {
case err == nil:
	// signed in
case errs.IsCode(err, emailpassword.ErrorCodeInvalidCredentials):
	// unknown email, wrong password, or no password: ask again
default:
	// a hook's rejection, a rate limit, or a failure such as a database outage
}
```

### Sign in by another identifier

`SignInWith` is `SignIn` for a user who is named by something other than an email. A plugin that adds an identifier calls it and supplies the lookup. The password check, the second-factor checkpoint, the session, and the `auth.signIn.*` points are the same as for `SignIn`. The [username](username.md) plugin signs in this way:

```go
result, err := plugin.SignInWith(ctx, emailpassword.IdentifierCredentials{
	Key:      "username", // the payload key, and the Values key for rate-limit rules
	Value:    name,
	Password: password,
	Method:   "username", // SignInResult.Method
	Find: func(ctx context.Context, s *store.Store, value string) (*models.User, error) {
		return s.FindUserBy(ctx, "username", value)
	},
})
```

`SignInWith` behaves as follows:

- `Find` returns a NotFound error for an unknown identifier. Sign-in then answers `invalid_credentials`, as for a wrong password.
- Handlers on `auth.signIn.before` see `Value` under `Key`, where an email sign-in has `email`. `Find` gets the value they left.
- `Value` is published in `Values` under `Key` as it is, so pass it in the form a rate-limit rule should count by. No email is published.
- The error message names the key: `invalid username or password`.

### Set a password from your code

```go
admin := identity.WithCaller(ctx, identity.System("support-tool"))
if err := plugin.ValidatePassword(newPassword); err != nil { /* password_invalid_input */ }
user, err := plugin.SetPassword(admin, userID, newPassword)
```

`SetPassword` sets the password of the account you name. It is an admin operation: it requires `emailpassword.PermissionSetPassword` (`user:set_password`), which only a system caller holds. A signed-in user gets `403` with the code `insufficient_scope`, and an anonymous caller the session error. Your code names itself with `identity.System(name)`, and the audit log records that name.

`plugin.SetPasswordUnchecked(ctx, userID, newPassword)` is the same without the check of the caller. It is for a plugin that has established the right by its own means: the [phone number](phonenumber.md) plugin calls it after a code sent to the user's number was confirmed, for a caller who is anonymous. Don't call it from a route that takes the user id from its input.

Both behave as follows:

- They apply the password rules, give a user without a password one, end every session of the user, and create none.
- They send the `core.MailPasswordChanged` notice when a mail sender is configured.
- They fire no points, and work with password reset by email off.
- Call `ValidatePassword` first when a later refusal would cost you something, such as a code already used up.

`passwords.SignOut(ctx)` ends the session of the caller on `ctx` and fires the sign-out points. It is the flow behind `POST /sign-out`, and requires the permission `emailpassword.PermissionSignOut` (`session:sign_out`), which a signed-in user holds:

- **The caller is the one the router put on the request's context**, or the one another transport put there. A caller that is not signed in gets the reason as a `401` session error, as from `httpapi.RequireSession`.
- **It ends the caller's own session only.** A job that acts for a user (`identity.System("job").For(userID)`) has no session, and gets `session_missing`. To end another session from code, call `ac.SessionManager.Revoke`.
- **It writes nothing to a response,** so the session's cookie stays with its browser. A route of yours that calls it removes the cookie afterwards with `rctx.TokenCarrier.Clear(rctx)`; see [Sessions](sessions.md#write-a-route-that-ends-the-session).

## Reject a sign-up from a hook

A hook handler can reject a sign-up with its own status and message. Return a typed error from a handler on `auth.signUp.before`, `data.user.beforeCreate`, or `data.user.afterCreate`:

```go
reg.OnBefore(hooks.HookUserBeforeCreate, func(hctx *core.HookContext, row behemoth.M) (behemoth.M, error) {
	if blocked(row[models.UserEmail]) {
		return nil, errs.NewInvalidInputError("signup", "user", "this email domain is not allowed", nil)
	}
	return row, nil
}, nil)
```

The client gets the status of the error's category (`400` here) and its public message, and `auth.signUp.failed` fires with code `rejected_by_hook`. An untyped error (`errors.New`) from a data hook also stops the sign-up, but it counts as a failure of the system. The client gets a `500`, the error is logged, and no failed point fires. `errs.IsRejection(err)` reports which of the two an error is. It is true for a typed error of category validation, conflict, unauthorized, forbidden, or rate limited.

## Password reset

A user who forgot their password asks for a link by email and sets a new password with it. Reset is off until you set `Options.Reset.LinkURL`.

The plugin does not send email. It hands the link to the mail sender you give Behemoth at `Boot`. See [Mail](mail.md).

| Step | Who | What happens |
| --- | --- | --- |
| 1 | client | posts an email to `POST /password-reset/request` |
| 2 | plugin | finds the user, issues a single-use token, and hands the link to your mail sender |
| 3 | you | send the message |
| 4 | user | opens the link, which is a page of your application: `https://app.example.com/auth/reset?token=...` |
| 5 | your page | asks for the new password and posts it with the token to `POST /password-reset/confirm` |
| 6 | plugin | consumes the token, sets the password and ends the user's sessions |

### Setup

```go
plugin := emailpassword.New(emailpassword.Options{
	Reset: emailpassword.ResetOptions{
		LinkURL: "https://app.example.com/auth/reset",
	},
})

ac, err := engine.Boot(ctx, app, db, engine.BootConfig{
	Mail: core.MailConfig{
		Sender: core.MailSenderFunc(func(ctx context.Context, msg core.MailMessage) error {
			return mailQueue.Enqueue(ctx, msg.To, subjectFor(msg.Kind), msg.URL)
		}),
	},
	// ...
})
```

Reset uses the core `tokens` table and, for its limit per email, the `rate_limits` table or Redis.

Your sender gets two kinds of message, both as a `core.MailMessage`:

| `Kind` | When | Content |
| --- | --- | --- |
| `core.MailPasswordReset` | a link was requested | `URL`, `Token` and `ExpiresAt` of the link |
| `core.MailPasswordChanged` | a reset went through | no link. It tells the owner their password was changed, in case it was not them. |

| `ResetOptions` field | Default | Meaning |
| --- | --- | --- |
| `LinkURL` | empty: reset is off | The page of your application the link points to. Absolute, `http` or `https`. The plugin adds the token as the `token` query parameter and keeps any query the URL already has. |
| `TTL` | 1 hour | How long a link works. `TokenConfig.TTLOverrides[core.TokenKindPasswordReset]` takes precedence. |
| `RequestLimit` | 5 per 15 minutes | Reset requests allowed per email. |
| `WaitForSend` | false | false: the request returns once the message is queued, and a failed send is logged. true: the request waits for your sender, and a failed send revokes the link. |
| `LeaveEmailUnverified` | false | Set it to stop a completed reset from marking the email as verified. |

With reset on, a missing mail sender, a `LinkURL` that is not an absolute URL, and a negative `TTL` are configuration errors from `Boot`.

`examples/init` runs reset end to end: a sender that prints the link (`mail.go`) and the page the link opens (`pages.go`).

### Routes

| Route | Body | Result |
| --- | --- | --- |
| `POST /password-reset/request` | `email`, and optionally `metadata` (an object passed to your sender in `MailMessage.Metadata`) | `200` with `{"status": "ok"}`, whether or not the email has an account |
| `POST /password-reset/confirm` | `token`, `password` | `200` with `{"status": "password_reset"}`. No session token: the user signs in with the new password. |

| Route | When | Status | Code |
| --- | --- | --- | --- |
| request | the body is not valid JSON, or `email` is not an address | `400` | `request_validation_error`, `email_invalid_input` |
| request | too many requests for this email or from this client address | `429` | `rate_limited` |
| confirm | `token` or `password` is missing | `400` | `token_invalid_input`, `password_invalid_input` |
| confirm | the password fails the plugin's rules | `400` | `password_invalid_input`. The link stays usable. |
| confirm | the link is unknown, expired, already used, or replaced by a newer one | `401` | `invalid_reset_token` |
| confirm | too many passwords are being hashed at once. The link stays usable. | `503` with `Retry-After` | `server_busy` |

The request route gives one answer for an unknown email, a known one, and a send that failed, so the response does not show which addresses have an account. Confirm gives one answer for every link it refuses.

### What a reset does

A reset has these effects:

- **It sets the password under the same rules as sign-up.** `MinPasswordLength`, `MaxPasswordLength`, and `ValidatePassword` apply. The password is checked before the token, so a refused password does not cost the user the link.
- **The link works once.** It is used up at the moment the password is written. If that write fails (a database error, a data hook's veto), the link is spent and the user asks for a new one.
- **A new link replaces the old one.** A user has one live reset link.
- **It ends every session of the user.** They are revoked with the reason `password_reset` (`emailpassword.SessionRevokedPasswordReset`).
- **It does not sign the user in.** The user signs in with the new password, so a second factor still applies.
- **It gives a password to a user who had none.** A user created by a magic link or a provider can ask for a reset link, and using it creates their credential account.
- **It marks the email as verified.** The link was sent to the address, so using it shows the user controls it. Set `LeaveEmailUnverified` if verified should only mean that your own verification passed.
- **It is tied to the address it was sent to.** If the user's email changes before the link is used, the link is refused.
- **It tells the user.** A `core.MailPasswordChanged` message goes to the account's address in the background. If it can't be queued or sent, that is logged and the reset still stands.
- **Asking for a link changes nothing.** The old password and the sessions keep working until the link is used.

### Call the flows from code

```go
sent, err := plugin.RequestPasswordReset(ctx, emailpassword.ResetRequest{Email: "ada@example.com"})
// sent.UserID, sent.TokenID, sent.ExpiresAt

user, err := plugin.ResetPassword(ctx, rawToken, newPassword)
```

The flows behave as follows:

- Unlike the route, `RequestPasswordReset` reports an unknown email (`emailpassword.ErrNoAccount`) and a failed send.
- `ResetPassword` returns a typed error with the code `emailpassword.ErrorCodeInvalidResetToken` for a link it refuses, and a validation error for a refused password.
- With reset off, both return a configuration error.

### Hooks

The plugin declares six points when reset is on.

| Point | Phase | Payload or result |
| --- | --- | --- |
| `auth.passwordReset.beforeRequest` (`emailpassword.HookResetRequestBefore`) | before | `email` and `metadata`. A handler may rewrite them or stop the request. |
| `auth.passwordReset.afterRequest` (`emailpassword.HookResetRequestAfter`) | after | a `*emailpassword.ResetRequestResult`: user id, email, token id, expiry. Not the token. |
| `auth.passwordReset.requestFailed` (`emailpassword.HookResetRequestFailed`) | failed | codes `user_not_found`, `issue_failed`, `send_failed`, `rejected_by_hook` |
| `auth.passwordReset.before` (`emailpassword.HookResetBefore`) | before | `password`, and `email` when the token is valid. A handler can stop the reset; what it writes to the payload is not used. |
| `auth.passwordReset.after` (`emailpassword.HookResetAfter`) | after | the `*models.User` |
| `auth.passwordReset.failed` (`emailpassword.HookResetFailed`) | failed | codes `invalid_token`, `user_not_found`, `email_changed`, `rejected_by_hook` |

The after and failed points are audited under their point names. Issuing and consuming the token also fire the token manager's points with the kind `password_reset`, and each ended session fires the session revoke points.

### Rate limits

| Rule | Limits | Default |
| --- | --- | --- |
| `emailpassword.RuleResetRequestAddress` | reset requests per client address | 10 a minute |
| `emailpassword.RuleResetConfirmAddress` | reset attempts per client address | 10 a minute |
| `emailpassword.RuleResetRequestEmail` | reset requests per email | `Reset.RequestLimit`, 5 per 15 minutes |

The rules are attached to the flows' before points, `auth.passwordReset.beforeRequest` and `auth.passwordReset.before`, so they also hold for `RequestPasswordReset` and `ResetPassword` called from your own code or another transport. A call from code with no client address is counted per email only. Change a limit, or turn a rule off, by its name with `RateLimitConfig`; see [Hooks](hooks.md#change-a-rule-by-its-name).

The rule per email counts requests for unknown emails too, so a `429` does not show whether the address has an account.

## Limits

The plugin has no password change for a signed-in user. To add one, write a plugin that depends on `emailpassword`, checks the caller and the current password itself, and calls `SetPasswordUnchecked`, so that the password rules stay in one place.
