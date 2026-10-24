# Email and password plugin

This document explains how `plugins/emailpassword` fits into `Prepare` and `Boot`, where its email and password rules live, and how its optional password reset works.

## What the plugin contributes

| `core.Plugin` method | Result | Why |
| --- | --- | --- |
| `Meta` | name `emailpassword`, no dependencies, no mount path | routes sit directly under the base path (`/sign-in/email`) |
| `Declare` | the permission `session:sign_out` (`PermissionSignOut`), and with reset on: the `password_reset` token kind, six `auth.passwordReset.*` points and three rate-limit rules | core declares the sign-up, sign-in and sign-out points (`CoreDeclareHookPoints`) and owns `users`, `accounts` and `sessions` |
| `Register` | nothing | the plugin fires points; it does not handle any |
| `Middlewares` | nothing | no route has a session middleware: `SignOut` requires its caller itself (`Access.RequireUser`) |
| `Init` | checks `Options`, wraps the flows with `core.WithLifecycle`; with reset on, also requires a mail sender | runs in `Boot`, after the dispatcher exists |
| `Routes` | sign-up, sign-in, sign-out, and the two reset routes when reset is on | reads the session manager, so it must run after `Init`; `Boot` guarantees that order |

The sign-in route answers through `httpapi.WriteSignIn`, which writes the body every sign-in route shares and delivers the session token by the configured delivery; see [`../sessions/token_delivery.md`](../sessions/token_delivery.md). The sign-up route answers `{"user": ...}` itself: it has no session.

`SignIn` returns a `*core.SignInResult`, the type every sign-in plugin hands to `auth.signIn.after`. `emailpassword.SignInResult` is an alias kept for callers. `Method` is `"emailpassword"`.

The plugin knows nothing about email verification. A user it signs up is created with `EmailVerified` false, and the email verification plugin, if installed, sends the link from its handler on `data.user.created`; see [`emailverification.md`](emailverification.md).

Compare with a plugin such as the example `activity` (`examples/init/plugin.go`): that one declares a table in `Declare` and has a mount path. This one adds behaviour on top of core tables, and declares one permission and, with reset on, the reset's parts.

## The flows

The plugin has these flows:

- **Sign-up** (`Plugin.signUpBody`) validates the email and password with the plugin's `Options`, hashes the password with `AuthContext.Crypto.Passwords`, and creates the user and its credential account in one store transaction.
- **Sign-in** (`Plugin.signInBody`) verifies the password against the credential account and creates a session. It passes only the session's state to `SessionManager.Create`; the manager takes the IP address and user agent from the request on the context, so the flow also runs without a request. It applies no password rules, so a password accepted under an older policy still works.
- **Sign-out** (`Plugin.SignOut(ctx)`) ends the session of the caller on the context. It requires `session:sign_out` through `Access.RequireUser`, and a session: a caller that is not signed in gets the reason as a session error, and a system caller acting for a user gets `session_missing`. The route, `handleSignOut`, then takes the token back from the client with `TokenCarrier.Clear`, which removes the cookie under a cookie delivery. `SignOut` itself writes no response: see *Taking the token back* in [`../sessions/token_delivery.md`](../sessions/token_delivery.md). Ending another session is not a self-service flow; code calls `SessionManager.Revoke`.

`signUpBody` and `signInBody` are methods on the plugin, which `Init` wraps with `WithLifecycle`. `signUpBody` reads the options; `signInBody` reads none today and is a method so both flows have the same shape.

Sign-up stores `email`, the password hash and three profile fields: `firstname`, `lastname`, and `image_url`. It has no username: the username plugin adds the column and fills it from its own handlers; see [`username.md`](username.md).

The wrapped flows are reached through two exported methods, `Plugin.SignUp(ctx, input)` and `Plugin.SignIn(ctx, creds)`. Each builds the flow's `HookContext` (`Plugin.operation`): the `AuthContext` kept by `Init`, the request found on `ctx` or nil, and `Values` copied from the operation `ctx` belongs to, if any. The route handlers call these methods with `rctx.Ctx`, so a route, a CLI and another plugin all enter the flow the same way. Called before `Boot` has run `Init`, they return a configuration error.

Before entering the wrapped flow, both put the email they were called with in `Values` under `hooks.HookValueEmail`, in its stored form (`publishEmail`, which uses `store.NormalizeEmail`). A rate-limit rule on `auth.signUp.before` or `auth.signIn.before` keys on it, since a rule's `KeyFunc` does not get the payload. A call without an email leaves no entry, and `publishEmail` removes one copied from an enclosing operation, so a rule per email does not apply to that call.

### The password hashes

Every hash goes through `AuthContext.Crypto.Passwords` with the operation's context: `hashPassword` for `Hash`, and `signInBody` for `Verify`. The hasher traces each call and bounds how many hashes run at once, so it can refuse one with `server_busy`; see [`../crypto/passwords.md`](../crypto/passwords.md).

Where sign-in refuses before it has checked a password, for an unknown email or a user without a credential account, it hashes the attempted password and throws the hash away (`decoyHash`). Sign-up does the same for a taken email. The hash makes the answer take as long as a wrong password's.

`[Invariant]` The decoy hash's error is returned, as `Verify`'s is. Under load, a busy hasher then answers `503` for every email. If the error were dropped, an unknown email would get `401` while a known one got `503`, and the answer would tell a caller which addresses have an account. `TestEmailPasswordAnswersBusyTheSameForEveryEmail` checks it. A hasher error fires no failed point: the caller is not at fault, and a lockout must not count it.

### Sign-in's input

`Plugin.SignIn` takes an `EmailAndPasswordCredentials`: `Email`, `Password`, and an `Extra` map. `signInBody` reads the two fields. `Extra` exists for handlers on `auth.signIn.before`.

The struct implements `behemoth.Serializable`, so `WithLifecycle` converts it with its own methods and not with a JSON round trip:

| Step | Method | What happens |
| --- | --- | --- |
| route to struct | `FromMap(body)` | `handleSignIn` decodes the body into a `behemoth.M` first, so unknown fields survive |
| struct to payload | `ToMap()` | a copy of `Extra`, then `email` and `password` set from the fields |
| payload to struct | `FromMap(payload)` | `email` and `password` fill the fields, every other key goes to a new `Extra` |

Rules that follow from the two methods:

- The payload is flat. `Extra: {"captchaToken": "x"}` is `payload["captchaToken"]`, the same lookup a handler uses on `auth.signUp.before`.
- The fields win. An `Extra` key named `email` or `password` is overwritten in `ToMap`, so a caller can't hand handlers one email and the flow another.
- The payload is a copy. A handler's write does not reach the map the caller passed.
- `Extra` is replaced, not merged, on the way back. A key a handler deleted is gone, and `Extra` is nil when no other key is left.
- A missing or null `email` or `password` becomes an empty string, which sign-in refuses with `invalid_credentials`. A value of another type is a validation error (`400`), from the route's body and from a handler's rewrite alike.

### Sign-in by another identifier

`signInBody` takes an `IdentifierCredentials`, not an `EmailAndPasswordCredentials`. Its first step is `creds.Find(ctx, store, value)`, and everything after it is the same for every identifier: the hash for an unknown user, the credential account, the password check, `auth.signIn.credentialsVerified`, and the session.

| Entry point | `Key` | `Find` | Publishes |
| --- | --- | --- | --- |
| `Plugin.SignIn(ctx, EmailAndPasswordCredentials)` | `email` | `Store.FindUserByEmail` | `HookValueEmail`, normalized (`publishEmail`) |
| `Plugin.SignInWith(ctx, IdentifierCredentials)` | the caller's, such as `username` | the caller's | `Value` under `Key`, as it is; removes an inherited `HookValueEmail` |

- **`IdentifierCredentials` is `Serializable`,** by the rules of the table above: the payload is flat, with `Value` under `Key`. `FromMap` reads `Value`, `Password`, and `Extra` back and keeps `Key`, `Method`, and `Find`, so a handler can rewrite the identifier and can't change how it is looked up. The two types share `credentialFields`.
- **`Find` is a function and is not in the payload.** The hook payload stays plain data.
- **`SignIn` builds the same struct** with an unexported `normalize`, which keeps the email in a failed sign-in's audit metadata lowercased as before. `SignInWith` clears it: a caller's value is recorded as given.
- **`SignInWith` refuses a `Key` that is empty or `password`, and a nil `Find`,** with a configuration error.
- **The refusal's message names the key** (`invalid username or password`). The code is `invalid_credentials` for all.
- **`SignInResult.Method` is `creds.Method`,** or this plugin's name when it is empty.

The username plugin is the one caller; see [`username.md`](username.md) for the decision.

`SignOut` is an exported function that takes a `HookContext` and only uses the `AuthContext` on it, so other plugins can call it. It turns the context into one operation with `core.AsOperation`.

## Password reset

Reset lives in `reset.go` and is off until `Options.Reset.LinkURL` is set. `Declare`, `Init`, and `Routes` each branch on `ResetOptions.enabled()` and call `declareReset`, `initReset`, or add the two routes. With reset off the exported flows return a configuration error (`resetOperation`).

The two flows have the shape of the magic link plugin's request and verify; see [`magiclink.md`](magiclink.md). The request side is the same code path with another token kind and mail kind. The second half differs: a magic link ends in a session, a reset ends in a new password and no session.

### Requesting a link

`Plugin.RequestPasswordReset` publishes the email (`publishEmail`) and runs `resetRequestBody`, wrapped with `auth.passwordReset.beforeRequest`, `.afterRequest`, and `.requestFailed`:

| Step | What | On failure |
| --- | --- | --- |
| 1 | normalize the email and check its shape (`utils.IsValidEmail`) | a validation error |
| 2 | `Store.FindUserByEmail` | `Fail` with `user_not_found`, `ErrNoAccount` |
| 3 | `TokenManager.RevokeAllForSubject(password_reset, user.ID)` | `Fail` with `issue_failed` |
| 4 | `TokenManager.Issue`, subject the user's id, the email in the metadata | `Fail` with `issue_failed` |
| 5 | `Mailer.SendAsync`, or `Send` with `WaitForSend` | the token is revoked, `Fail` with `send_failed` |

- **Step 1 does not use `Options.ValidateEmail`.** That option is the policy for new accounts. An account created under an older policy can still reset its password.
- **The user needs no credential account.** A user created by a magic link gets a link too.
- **The route hides everything after the lookup.** `handleResetRequest` answers `200` for `ErrNoAccount` and for a `resetAfterLookupError`, and logs the second at Error. The response time still differs by the token writes; `docs/ongoing.md` has the entry.

### Using a link

`Plugin.ResetPassword` first reads the token without consuming it (`TokenManager.Verify`). The email in it goes in the before payload and in `Values`, and the token itself is handed to the flow in `resetInput.verified`. It then runs `resetBody`, wrapped with `auth.passwordReset.before`, `.after`, and `.failed`:

| Step | What | On failure |
| --- | --- | --- |
| 1 | `validatePassword`, the rules sign-up uses | a validation error; the token is untouched |
| 2 | the token `Verify` read is usable | `Fail` with `invalid_token` |
| 3 | `Store.FindUserByID(token.Subject)` | `Fail` with `user_not_found` |
| 4 | compare the email in the token's metadata with the user's | `Fail` with `email_changed` |
| 5 | `hashPassword` | the error is returned |
| 6 | `TokenManager.Consume(password_reset, token)` | `Fail` with `invalid_token`: another reset used the link since step 2 |
| 7 | in one `Store.Transaction`: update the credential account's hash, or create the account; set `email_verified` unless it is set or `LeaveEmailUnverified` | a typed veto of a data hook fires `Fail` with `rejected_by_hook` |
| 8 | `SessionManager.RevokeAllForUser(user.ID, "password_reset", "")` | the error is returned; the password is already changed |
| 9 | `Mailer.SendAsync` a `MailPasswordChanged` notice to the user's address | logged at Warn; the reset stands |
| 10 | return the `*models.User` | |

- **Steps 2, 3, 4 and 6 answer with `errInvalidResetToken`,** one typed unauthorized error with the code `invalid_reset_token`. The audit event of `auth.passwordReset.failed` has the real code.
- **The password is checked before the token.** Its answer is the same for any token, so it tells nothing about one, and a typo does not cost the user the link.
- **The token is consumed as late as it can be.** Steps 2 to 5 use the token as `Verify` read it and change nothing, so a refusal or a failed hash there leaves the link usable. Only the write of step 7 comes after the consume. This differs from the magic link plugin, which consumes first: there the steps after it are the sign-in itself.
- **`Consume` is still the at-most-once step.** Two resets racing with one link both pass steps 2 to 5, and one loses at step 6.
- **Only a token that checked out is hashed for.** An invalid token stops at step 2, so the route can't be used to make the server hash.
- **The before payload is veto-only.** `resetInput.FromMap` reads nothing back. A handler sees `password` and `email` and can refuse; it can't swap the password or the token.

### Tests

| Test | File | Covers |
| --- | --- | --- |
| `TestPasswordResetRequestAndConfirm` | `tests/plugins/passwordreset_test.go` | both flows from code: unknown email, validation, what the sender receives, a new link replacing the old, a refused password keeping the link, the old password and sessions ending, the notice, a link working once, email verified, the points' payloads and audit events |
| `TestPasswordResetRoutes` | same | the request route's identical answers, validation statuses, the confirm route's body and its `401`, no session token |
| `TestPasswordResetRequestsAreLimitedPerEmail` | same | the rule per email, for a known and an unknown address |
| `TestPasswordResetRefusals` | same | a changed email, a deleted user |
| `TestPasswordResetWithoutAPasswordAndLeavingEmailUnverified` | same | a first password for a user without one, `LeaveEmailUnverified` |
| `TestPasswordResetOptions` | same | off by default with no routes and no sender needed; the configuration errors |
| `TestPasswordResetSendsInBackground` | same | the default send does not hold the request, and a failed send leaves the link valid |

## Setting a password for another plugin

`Plugin.SetPassword(ctx, userID, password)` is an admin operation: it requires `user:set_password` (`PermissionSetPassword`) through `Access.RequireAdmin`. `SetPasswordUnchecked` is the same without that check, for a plugin with its own proof; the phone number plugin's reset calls it for an anonymous caller, who stays the caller of the sessions it ends. Both are the write half of a reset without the link: `validatePassword`, `hashPassword`, `storePasswordHash` in a transaction, then `afterPasswordChange`. `resetBody` uses the same two helpers, so a reset by link and one through `SetPassword` end the same way.

| Helper | What |
| --- | --- |
| `storePasswordHash(ctx, tx, userID, hash)` | updates the credential account, or creates it for a user who had no password |
| `afterPasswordChange(ctx, user)` | `RevokeAllForUser` with `SessionRevokedPasswordReset`, then the `MailPasswordChanged` notice in the background |

- `SetPassword` does not touch `EmailVerified`. A reset by link marks the email verified because the link was sent to it; a caller of `SetPassword` proved something else.
- The notice is skipped when no mail sender is configured or the user has no address. Reset by link requires a sender, so nothing changes there.
- `Plugin.ValidatePassword` is exported next to it for a caller that would lose something by a late refusal. The phone number plugin checks the password before it consumes its code.
- Both work with `Options.Reset` off.

The phone number plugin is the one caller; see [`phonenumber.md`](phonenumber.md).

## Limits to know

These limits apply:

- `[Not built]` Password change for a signed-in user. It needs a way to re-check the current password; see *Nothing refreshes a session in place* in `docs/ongoing.md`.
- `[Limit]` A reset consumes its token just before it writes the password, outside the write's transaction. If that write fails, the link is spent and the user asks for another. `docs/ongoing.md` has what consuming inside the transaction would need.
- `[Not built]` Rehash on sign-in. `PasswordHasher.NeedsRehash` exists but sign-in does not call it.
- Sign-up, sign-in, and sign-out return every error to the router unchanged (`handleSignUp`, `handleSignIn`, `handleSignOut`), which maps it with `RouterConfig.ErrorMapper`. This is how a data hook's veto on `data.user.beforeCreate` reaches the client with its own status and message; `signUpBody` also fires `auth.signUp.failed` with `rejected_by_hook` for it (`errs.IsRejection`). See *Sign-in's refusals are typed* below. The session check in front of sign-out, `httpapi.RequireSession`, does the same: see *`RequireSession` returns its refusals* in [`../sessions/token_delivery.md`](../sessions/token_delivery.md).
- `Options.ValidateEmail` and `ValidatePassword` errors are replaced by `invalid email` and `invalid password` in the response, so a custom message does not reach the client. The function's error is the typed error's cause, for logs.

### The email and password rules belong to the plugin

**Context:** Sign-up read `AuthContext.Validator` and `AuthContext.PasswordOptions`. `Boot` set neither and `BootConfig` had no field for them, so a booted application panicked on the first sign-up. Only this plugin read them.

**Options considered:**

- *Wire both through `BootConfig`, with a default validator in core.* Every plugin can read one shared policy. An application without passwords (OAuth or magic links only) still carries a password policy, and core gains a `Validator` interface with one consumer.
- *Make them options of the plugin: `emailpassword.New(Options)`.* The dependency is visible where it is used, the zero value has defaults, and `AuthContext` loses two fields. A second plugin that sets passwords can't read the policy from the `AuthContext`.

**Decision:** The second option. `core.Validator`, `core.PasswordOptions`, and the two `AuthContext` fields are removed. The hasher was never a policy choice of the plugin: it comes from `AuthContext.Crypto.Passwords`, which `Boot` builds from `BootConfig.Crypto`. The default email check is `utils.IsValidEmail`, a plain function other plugins can call. Plugins that set passwords are expected to depend on this one and go through it.

**Revisit if:** plugins that don't depend on `emailpassword` need to set passwords. A shared policy on `AuthContext` would then be justified.

### Sign-in takes a struct with an `Extra` map

**Context:** Sign-up takes a `behemoth.M`, so a field a plugin adds to the request (an invite code) reaches `auth.signUp.before`. Sign-in decoded the body into a two-field struct, so a captcha token or a device id was gone before the before chain ran. A handler could not read it from `hctx.Request` either, since the body had been consumed, and a caller of `Plugin.SignIn` had no way to pass it.

**Options considered:**

- *Take a `behemoth.M`, like sign-up.* One convention and the least code. `SignIn(ctx, behemoth.M{"emial": ...})` compiles, the two required values are checked at run time only, and the exported signature changes.
- *Add `Extra behemoth.M` and keep the JSON round trip.* Small. Handlers get `payload["extra"]["captchaToken"]` on sign-in and `payload["inviteCode"]` on sign-up, so one captcha handler for both points needs two lookups.
- *Add `Extra` and implement `behemoth.Serializable`.* The required input stays typed, the payload is flat, and `Plugin.SignIn` keeps its signature. It costs two methods and a rule for a key that collides with a field.

**Decision:** The third. A fixed shape checked by the compiler is preferred over a map, and sign-in's shape is fixed: two required values and an open tail. Sign-up keeps its map because its profile fields are open-ended. Other plugins with a flow of fixed input are expected to follow the same shape: a struct for what the flow reads, an `Extra` map for what only handlers read, and `ToMap`/`FromMap` that keep the payload flat.

**Revisit if:** several plugins repeat the same two methods. A helper in `core` that flattens a struct with an `Extra` field would then be worth having.

### Sign-in's refusals are typed

**Context:** `signInBody` returned `errors.New("invalid email or password")` for an unknown email, a user without a credential account and a wrong password. An untyped error could therefore be a wrong password or a broken system, and `handleSignIn` could not tell them apart. It wrapped every error in a validation error, so a database outage was answered with `400` and never reached the router's log. `handleSignOut` wrote its own `500` with `err.Error()` as the body, which sent internal error text to the client.

**Options considered:**

- *Return typed errors from the handlers and keep the untyped refusal.* The router maps an untyped error to `500`, so a wrong password would become a `500`.
- *Make the refusal a typed error and return everything to the router.* `errInvalidCredentials` is an unauthorized error (`errs.NewUnauthorized`) with the code `invalid_credentials`. After that, every error out of `SignIn` is a typed rejection or a failure of the system, and the handler has nothing to decide.

**Decision:** The second, for sign-in. Sign-out needed only the first half: `SignOut` already returned typed errors. Both handlers now end in `return err`.

What the router does with each error:

| Error from the flow | Status | Body |
| --- | --- | --- |
| `errInvalidCredentials` (all three refusals) | `401` | `{"error": "invalid email or password", "code": "invalid_credentials"}` |
| a hook handler's typed rejection | the status of its category | its public message and code |
| a rate limit | `429` with `Retry-After` | the rate limit message |
| a busy password hasher (`errs.CodeServerBusy`), added with `crypto.PasswordLimit` | `503` with `Retry-After` | the busy message |
| a database error, or any category without a status | `500` | the category's public message, never the error's text |
| an untyped error (a hook handler's `errors.New`, an unexpected failure) | `500` | `{"error": "internal server error"}` |

The three refusals share one code and one message on purpose: a different answer for an unknown email would tell a caller which addresses have an account. The failure code a hook handler sees on `auth.signIn.failed` still tells them apart (`user_not_found`, `no_credential_account`, `invalid_credentials`).

Two changes a client can observe: a wrong password is a `401` where it was a `400`, and an untyped error from a handler on a sign-in point is a `500` where it was a `400`. The second matches what the hook docs ask for, a typed error from a handler that rejects.

### Sign-up's refusals are typed too

**Context:** After the change above, sign-up still returned untyped errors for an invalid email, an invalid password and a taken email, and `handleSignUp` wrote every untyped error itself as `400 {"error": "<text>"}`. A client could tell the three apart only by their English text. The same branch caught failures: the `user create failed` that replaced an untyped transaction error, and an untyped error from the password hasher, whose text went to the client. Both were a `400` and were not logged, because the handler had written the response and returned nil.

**Options considered:**

- *Keep the `400`s and add a `code` field in the handler.* No status changes. The handler still decides what is a rejection by the absence of a type, so a failure stays a `400`.
- *Typed errors with exported codes, and `return err` in the handler.* The same rule as sign-in: every error out of `SignUp` is a typed rejection or a failure of the system.

**Decision:** The second. `errInvalidEmail` and `errInvalidPassword` are validation errors (`email_invalid_input`, `password_invalid_input`), and `errEmailTaken` is a conflict (`email_taken`). The codes are the ones the reset flow and the email change flow already use for the same conditions. An untyped transaction error is returned as it is and no longer replaced: the router never sends an untyped error's text.

| Error from the flow | Status | Code |
| --- | --- | --- |
| `errInvalidEmail` | `400` | `email_invalid_input` |
| `errInvalidPassword` | `400` | `password_invalid_input` |
| `errEmailTaken` | `409` | `email_taken` |
| a body that is not JSON | `400` | `request_validation_error` |
| an untyped error (the hasher, the transaction, a handler's `errors.New`) | `500` | none |

Three changes a client can observe: the three rejections carry a `code`, a taken email is a `409` where it was a `400`, and a failure is a `500` where it was a `400`.

Two sign-ups racing for one email both pass the lookup, and the database refuses the loser's insert. Its duplicate key error names the `email` column ([`../database/adapters/unique_keys.md`](../database/adapters/unique_keys.md)), and `signUpBody` answers it as the lookup would have: `auth.signUp.failed` with `user_exists`, and `email_taken`. Another unique column a plugin added to `users` comes back from the store as that column's conflict, such as `username_taken`, and goes through `FailIfRejected`. `TestSignUpThatLosesTheRaceForAnEmail` (`tests/plugins/signup_race_test.go`) covers the email.

### Password reset is part of this plugin and off by default

**Context:** The docs expected reset to be a plugin of its own that depends on this one. A reset sets a password, and the password rules (`validatePassword`) and the traced hash (`hashPassword`) are private to this package.

**Options considered:**

- *A `passwordreset` plugin that depends on `emailpassword`.* An application without reset carries none of it. This plugin would have to export a `SetPassword` for it, and an application wires two plugins for what users see as one feature.
- *Part of this plugin, always on.* Nothing to wire. Every application with passwords would then need a mail sender and the `tokens` table, and `emailpassword.New(Options{})` would stop working without them.
- *Part of this plugin, on when `Options.Reset.LinkURL` is set.* The rules stay private, and an application that does not set the URL sees no change.

**Decision:** The third. The link URL is required for reset anyway, so it doubles as the switch and no separate `Enabled` field can disagree with it.

**Revisit if:** reset by link should become a plugin of its own. `SetPassword` now exists (the phone number plugin needed it), so such a plugin has what it would call.

### A reset ends the user's sessions and does not start one

**Context:** After the password is set, the flow could hand back a session, as a magic link does.

**Options considered:**

- *Sign the user in.* One step fewer for the user. The reset link becomes a sign-in credential with a one hour life, and the flow would have to fire the `auth.signIn.*` points to keep a second factor in the path.
- *Set the password, end every session, create none.* The user signs in with the new password through the normal flow, second factor included.

**Decision:** The second. Ending the sessions is the point of a reset after a compromise, and `RevokeAllForUser` takes no exception here because the caller has no session.

**Revisit if:** applications ask for the shorter path. It would be an option that runs the sign-in flow after the reset.

### A reset gives a password to a user who has none

**Context:** A user created by a magic link or a provider has no credential account. A reset request for their email has to do something.

**Options considered:**

- *Send nothing.* Reset stays strictly "replace a password". The request has to look the account up before deciding, and the user gets no explanation since the route answers the same either way.
- *Send the link and create the credential account at the reset.* The link proves control of the address, which is what sign-up by password asks for too.

**Decision:** The second. The account is created in the transaction that would otherwise update it.

**Revisit if:** an application wants accounts that can never have a password. A handler on `auth.passwordReset.beforeRequest` can refuse them today.

### Following a link marks the email verified, with an opt-out

**Context:** A reset link and a magic link are both sent to the account's address, so using one shows the user controls it. Both flows set `email_verified` for that reason. An application may want the flag to mean only that the user passed the application's own verification flow, for example when it records consent there.

**Options considered:**

- *Always mark.* No option. The application can't keep the flag for its own flow.
- *Never mark.* With `RequireVerified`, a user who reset their password by email would still be told to verify that same address.
- *Mark by default, with `LeaveEmailUnverified` on both plugins.* The zero value keeps the earlier behaviour.

**Decision:** The third, as `ResetOptions.LeaveEmailUnverified` and `magiclink.Options.LeaveEmailUnverified`. The field is phrased as the opt-out so that the zero value is the default.

### The reset link is sent in the background by default

**Context:** The request route answers the same for a known and an unknown email. A sender the request waits for is only called for a known one, so its latency shows which is which.

**Options considered:**

- *`Send`, as the magic link plugin defaults to.* A failed send is seen and the link revoked.
- *`SendAsync`.* The sender is out of the response time. A failed send is logged by the mailer and the link stays valid until it expires.

**Decision:** `SendAsync` by default, with `ResetOptions.WaitForSend` for an application whose sender only enqueues. The magic link plugin has the same option and default.

### A completed reset sends a notice

**Context:** A reset link reaches whoever reads the account's mailbox. If that is not the owner (a shared or compromised mailbox), the owner should learn that the password changed. The email change flow sends a notice for the same reason.

**Options considered:**

- *Leave it to a handler on `auth.passwordReset.after`.* No new mail kind. Every application has to write the same handler, and most would not.
- *Send a `MailPasswordChanged` message with `Send`.* A failure is seen. The password is already set, so there is nothing to undo, and the reset would wait for the sender.
- *Send it with `SendAsync`.* The reset does not wait or fail for it. A lost notice is only in the log.

**Decision:** The third. The message has no link and no token. There is no option to turn it off: an application that does not want it returns nil for the kind in its sender.

**Revisit if:** the notice should carry a way to act, such as a link that locks the account.
