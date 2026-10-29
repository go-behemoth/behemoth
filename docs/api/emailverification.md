# Email verification

The `emailverification` plugin confirms that a user controls their email address. It sends the user a link, and sets `EmailVerified` on the user when the link is followed.

It works with any way of creating users. You don't call it from your sign-up code: it notices a new user and sends the link.

It also has the flow for [changing an address](#change-an-email-address), in which the new address has to confirm before it replaces the old one.

## How it works

| Step | Who | What happens |
| --- | --- | --- |
| 1 | anything | a user is created with `EmailVerified` false: a password sign-up, another plugin, your own code |
| 2 | plugin | issues a single-use token and hands a link to your mail sender, in the background |
| 3 | user | opens the link, which is a page of your application: `https://app.example.com/auth/verify-email?token=...` |
| 4 | your page | posts the token to `POST /email/verify` |
| 5 | plugin | consumes the token and sets `EmailVerified` |

As with a [magic link](magiclink.md), the link points at your page and the page posts the token, so a mail scanner that opens the link does not use it up.

## Setup

```go
import "github.com/go-behemoth/behemoth/plugins/emailverification"

verification := emailverification.New(emailverification.Options{
	LinkURL: "https://app.example.com/auth/verify-email",
})

app, err := engine.Prepare([]core.Plugin{emailpassword.New(emailpassword.Options{}), verification}, engine.PrepareConfig{})
// ...
ac, err := engine.Boot(ctx, app, db, engine.BootConfig{
	Mail: core.MailConfig{Sender: sender}, // see Mail
	// ...
})
```

The plugin's name is `emailverification` (`emailverification.PluginName`). It depends on no other plugin and needs no table of its own. The link goes out as a `core.MailMessage` with `Kind` set to `core.MailEmailVerification`; [Mail](mail.md) describes the sender.

## Options

| Field | Default | Meaning |
| --- | --- | --- |
| `LinkURL` | required | The page of your application the link points to. Absolute, `http` or `https`. The plugin adds the token as the `token` query parameter. |
| `TTL` | 24 hours | How long a link works. `TokenConfig.TTLOverrides[core.TokenKindEmailVerification]` takes precedence. |
| `SendLimit` | 5 per 15 minutes | Links sent per email. See [Rate limits](#rate-limits). |
| `RequireVerified` | false | Refuse the sign-in of a user whose email is not verified. See [Require a verified email](#require-a-verified-email). |
| `ChangeLinkURL`, `RevertLinkURL` | none | The two pages of the email change flow. Setting both turns the flow on. See [Change an email address](#change-an-email-address). |
| `ChangeTTL` | 1 hour | How long the new address has to confirm a change. |
| `RevertTTL` | 72 hours | How long the old address can undo a change. |
| `ChangeLimit` | 3 an hour | Change requests per user. |

A missing mail sender, a `LinkURL` that is not an absolute URL, and a negative `TTL` are configuration errors from `Boot`. So is one of `ChangeLinkURL` and `RevertLinkURL` without the other.

## When a link is sent

| Event | What the plugin does |
| --- | --- |
| a user is created with `EmailVerified` false | sends a link, once the write has committed |
| a user is created with `EmailVerified` true | nothing. A plugin that trusts the address, such as an OAuth provider that vouches for it, creates the user verified. |
| a user's email is changed by a direct write (`Store.UpdateUser`) | clears `EmailVerified` and sends a link to the new address. The new address is in effect at once. To change an address safely, use [the change flow](#change-an-email-address). |
| a user's email changes in an update that also sets `EmailVerified` | leaves the flag as the update set it. This is for code that has checked the address itself, such as an admin screen. |
| any other update to a user | nothing |
| `POST /email/send-verification`, or `SendVerification` from code | sends a link, unless the address is verified already |

Sending behaves as follows:

- **Sending never holds the request up.** The link is handed to your mail sender in the background, so a sign-up returns without waiting for it. If the send fails, the failure is logged and the user asks for another link.
- **Asking again is harmless.** A new link replaces the earlier one, and a request for an address that is verified already sends nothing.
- **A failed automatic send does not undo the sign-up.** The user exists. The failure is logged, and the send route is how the user gets a link.
- **A magic link verifies the address too.** Following a sign-in link sets `EmailVerified`, so a user who signs in that way needs no verification link.

## Routes

The routes sit directly under the router's base path. Neither needs a session.

| Route | Body | Result |
| --- | --- | --- |
| `POST /email/send-verification` | `email`, and optionally `redirect_url` and `metadata` (an object) | `200` with `{"status": "ok"}` |
| `POST /email/verify` | `token` | `200` with `{"user": ..., "redirect_url": "..."}` |

**The send route answers the same for every email.** An unknown address, one that is verified already, and one that is sent a link all get a `200` with the same body, so the response does not show which addresses are registered.

Verifying does not sign the user in. No session is created.

### Error responses

| Route | When | Status | Code |
| --- | --- | --- | --- |
| both | the body is not valid JSON | `400` | `request_validation_error` |
| send | `email` is missing or not an email address | `400` | `email_invalid_input` |
| send | `redirect_url` is not allowed | `400` | `redirect_url_invalid_input` |
| verify | `token` is missing | `400` | `token_invalid_input` |
| verify | the link is unknown, expired, already used, or replaced by a newer one | `401` | `invalid_verification_link` |
| both | too many requests | `429` with `Retry-After` | `rate_limited` |

`redirect_url` follows the same rule as for a magic link: a path on your own site, or a URL on one of `RouterConfig.TrustedOrigins`. See [The redirect URL](magiclink.md#the-redirect-url).

## Require a verified email

With `RequireVerified`, a sign-in by a user whose email is not verified is refused with `403` and the code `email_not_verified`. The check works as follows:

- It applies to every sign-in that reaches `auth.signIn.credentialsVerified`, so a password sign-in is covered. A magic link sign-in passes, because the link verifies the address first.
- It is checked after the credentials. A wrong password is still answered with `invalid_credentials`, so the response does not show whether an address is verified to someone who does not know the password.
- The user gets a new link from `POST /email/send-verification`, which is why that route needs no session.

Show a "check your inbox" screen in your client when it receives `email_not_verified`.

## Change an email address

A user's address is how they sign in, and where sign-in links and future password resets go. Writing a new address straight into the user row makes whoever holds that address the owner of the account, before anyone has shown they hold it. The change flow keeps the old address until the new one confirms, and tells the old one.

| Step | Who | What happens |
| --- | --- | --- |
| 1 | user, signed in recently | posts the new address to `POST /email/change`. The account does not change. |
| 2 | plugin | sends the **current** address a notice with a link that undoes the change |
| 3 | plugin | sends the **new** address a link that confirms it |
| 4 | new address | opens the link; your page posts the token to `POST /email/change/confirm`. The account now has the new address, marked verified. |
| at any time | old address | opens the notice's link; your page posts the token to `POST /email/change/revert`. The change is dropped or undone, and the user is signed out everywhere. |

### Turn it on

Set both pages. The flow is off without them, and its routes are not mounted.

```go
verification := emailverification.New(emailverification.Options{
	LinkURL:       "https://app.example.com/auth/verify-email",
	ChangeLinkURL: "https://app.example.com/auth/change-email",      // posts the token to /email/change/confirm
	RevertLinkURL: "https://app.example.com/auth/undo-email-change", // posts the token to /email/change/revert
})
```

Your mail sender gets two more kinds of message. Both carry the two addresses in `msg.Data`, under `emailverification.DataOldEmail` and `emailverification.DataNewEmail`:

| `Kind` | Sent to | `URL` | Write it as |
| --- | --- | --- | --- |
| `core.MailEmailChangeNotice` | the current address | the page at `RevertLinkURL` | "A change of your address to `newEmail` was requested. If this was not you, undo it." |
| `core.MailEmailChange` | the new address | the page at `ChangeLinkURL` | "Confirm that this is your new address." |

`Data` comes from the plugin. `Metadata` is still what the request carried.

The flow keeps an account's old address in the core `reserved_values` table while the link at it works (see [What to know about the revert link](#what-to-know-about-the-revert-link)). Generate and apply a migration that creates the table before you deploy a version that has it. [Write Behemoth's tables from a plugin](core-tables.md#reserved-values) describes the table.

### Routes

| Route | Needs | Body | Result |
| --- | --- | --- | --- |
| `POST /email/change` | a fresh session | `email` (the new address), and optionally `redirect_url` and `metadata` | `200` with `{"status": "ok"}` |
| `POST /email/change/confirm` | nothing | `token` | `200` with `{"user": ..., "redirect_url": "..."}` |
| `POST /email/change/revert` | nothing | `token` | `200` with `{"status": "reverted"}` |

| Route | When | Status | Code |
| --- | --- | --- | --- |
| change | there is no session | `401` | |
| change | the session did not sign in recently enough | `401` | `session_not_fresh` |
| change | `email` is not an address, or is the one the account has | `400` | `email_invalid_input` |
| change | `redirect_url` is not allowed | `400` | `redirect_url_invalid_input` |
| change | the notice could not be handed to your mail sender | `500` | |
| confirm, revert | the link is unknown, expired, used, or overtaken by a later request or a revert | `401` | `invalid_email_change_link` |
| confirm, revert | the address belongs to another account by now, or is kept for another account that changed away from it | `409` | `email_taken` |
| all | too many requests | `429` with `Retry-After` | `rate_limited` |

The change flow behaves as follows:

- **The change route answers the same when the new address is taken.** It returns the `200` of a request that went through, and sends nothing to that address, so a signed-in user can't use it to find out which addresses have an account. The current address still gets its notice.
- **A new request replaces the pending one.** The earlier confirmation link stops working.
- **Confirming leaves the user's sessions alone.**

### A recent sign-in is required

`RequestChange`, the flow behind `POST /email/change`, requires the permission `emailverification.PermissionEmailChange` (`email:change`) with a fresh session: the session has to be valid and has to have signed in, or passed its second factor, within `SessionConfig.FreshAge` (15 minutes by default). A session that is older gets `401` with the code `session_not_fresh`. Your client asks the user to sign in again and retries. The flow checks it, so it holds however the flow is reached.

This is what stops someone who got hold of a long-lived session, for example from a shared computer, from moving the account to their own address. Nothing refreshes a session in place, so signing in again is the only way to get a fresh one.

You can put a fresh-session check on a route of your own ([Sessions](sessions.md#fresh-sessions) has more):

```go
func (p *Plugin) Routes() []httpapi.Route {
	return []httpapi.Route{{
		Method:      http.MethodPost,
		Path:        "/account/delete",
		Handler:     deleteAccount,
		Middlewares: []httpapi.Middleware{httpapi.RequireFreshSession(p.ac.SessionManager)},
	}}
}
```

It does what `httpapi.RequireSession` does first, so use it alone.

### What the revert link does

The link in the notice works from the moment the change is requested until `RevertTTL` has passed, whether or not the new address has confirmed:

- A change that is still pending is dropped.
- A change that was applied is undone: the old address is put back and marked verified.
- Every session of the user ends, because the change was requested from one of them.
- Revert links from later requests stop working.

The last point matters after several changes in sequence. Suppose the account goes from `a@` to `b@` to `c@`, and `a@` never asked for any of it. The link in `a@`'s mailbox puts `a@` back, whatever happened since, and then the link in `b@`'s mailbox no longer works. If `b@`'s link is used first, `a@`'s link still works afterwards. The older address takes precedence.

### What to know about the revert link

- **The old mailbox can still affect the account for `RevertTTL`.** That is the purpose: the owner can take the account back. It also means that a user who changed address because the old mailbox was compromised can have the change undone from that mailbox. Shorten `RevertTTL` if that is the likelier case for your users.
- **The old address is kept for the account while the link works.** No other account can take it in that time. A sign-up with it, a change to it, and a write of it through the store are refused as for a taken address, with `409` and `email_taken`. The account itself can change back to it. The reservation ends when `RevertTTL` has passed, when the link puts the address back, when a revert by an older link stops the link, and when the user is deleted.
- **A write that bypasses the reservation can still take the old address.** A write to the `users` table that does not go through the store is not checked. The revert can't put such an address back: it ends the sessions and answers `409` with `email_taken`. Handle that code by sending the user to your support channel.
- **The request waits for the notice.** If your mail sender returns an error for it, the change does not start. The confirmation to the new address is sent in the background, like a verification link.
- **A direct write still bypasses all of this.** `Store.UpdateUser` with a new `email` takes effect at once. Use `RequestChange` in your own code.

### Call the change flow from code

```go
// The address that changes is the one of the user the caller acts for.
ctx = identity.WithCaller(ctx, identity.System("support-tool").For(user.ID))
result, err := verification.RequestChange(ctx, emailverification.ChangeRequest{
	NewEmail: "ada@new.example.com",
})
confirmed, err := verification.ConfirmChange(ctx, rawToken) // confirmed.User, confirmed.RedirectURL
user, err := verification.RevertChange(ctx, rawToken)
```

- `RequestChange` changes the address of the user the caller on `ctx` acts for. A signed-in user needs a fresh session. Code in the process names the user with `identity.System(name).For(userID)`, which passes without a session; a system caller that names no user is a configuration error.
- It returns `emailverification.ErrEmailTaken` when the address belongs to another account, or is kept for one that changed away from it, which the route hides. It returns `emailverification.ErrNoAccount` for an unknown user.
- With the flow off, all three return a configuration error.

## What a link does

A verification link has these properties:

- **It works once**, and a new link replaces the old one.
- **It is tied to the address it was sent to.** If the user's email changes before the link is used, the link is refused.
- **It goes with the user.** `Store.DeleteUser` removes a user's links.

## Call the flows from code

```go
result, err := verification.SendVerification(ctx, emailverification.SendRequest{
	UserID:      user.ID,                // or Email
	RedirectURL: "/welcome",
	Metadata:    behemoth.M{"locale": "fr"},
})

verified, err := verification.Verify(ctx, rawToken)
// verified.User, verified.RedirectURL
```

The flows behave as follows:

- `SendVerification` reports what the route hides: `emailverification.ErrNoAccount` when no user matches and `emailverification.ErrAlreadyVerified` when there is nothing to do (check with `errors.Is`).
- It returns once the message is queued. A `nil` error does not mean the message was delivered.
- Called before `Boot`, both return a configuration error.

## Hooks

The plugin declares six points.

| Point | Phase | What the handler gets |
| --- | --- | --- |
| `auth.emailVerification.beforeSend` (`emailverification.HookSendBefore`) | before | payload `email`, `redirect_url`, `metadata`. A handler may rewrite the last two or stop the send. The address can't be changed. |
| `auth.emailVerification.afterSend` (`HookSendAfter`) | after | a `*emailverification.SendResult`: user id, email, token id, expiry. Not the token. |
| `auth.emailVerification.sendFailed` (`HookSendFailed`) | failed | codes `user_not_found`, `issue_failed`, `send_failed` (the message could not be queued), `rejected_by_hook` |
| `auth.emailVerification.beforeVerify` (`HookVerifyBefore`) | before | payload `email`, when the token names one. Veto only. |
| `auth.emailVerification.afterVerify` (`HookVerifyAfter`) | after | the verified `*models.User` |
| `auth.emailVerification.verifyFailed` (`HookVerifyFailed`) | failed | codes `invalid_link`, `user_not_found`, `email_changed`, `rejected_by_hook` |

The after and failed points are audited under their point names. `auth.emailVerification.afterVerify` is where to react to a confirmed address: grant access, send a welcome message.

With the change flow on, it declares nine more, three per step:

| Points | What the handler gets |
| --- | --- |
| `auth.emailChange.beforeRequest`, `.afterRequest`, `.requestFailed` | before: payload `userID`, `email` (the new address), `redirect_url`, `metadata`; the last two may be rewritten. After: a `*emailverification.ChangeResult`. Failed: codes `issue_failed`, `notice_failed`, `email_taken`, `send_failed`, `rejected_by_hook`. |
| `auth.emailChange.beforeConfirm`, `.afterConfirm`, `.confirmFailed` | before: payload `email`, veto only. After: the `*models.User` with its new address. Failed: codes `invalid_link`, `user_not_found`, `email_changed`, `email_taken`, `rejected_by_hook`. |
| `auth.emailChange.beforeRevert`, `.afterRevert`, `.revertFailed` | before: payload `email` (the address to put back), veto only. After: the `*models.User`. Failed: codes `invalid_link`, `user_not_found`, `email_taken`, `rejected_by_hook`. |

Their constants are `emailverification.HookChangeRequestBefore`, `HookChangeConfirmAfter` and so on. The after and failed points are audited, so the audit log shows who asked for a change, when it was applied, and whether it was undone. `auth.emailChange.afterRevert` is a signal worth alerting on: someone said a change to their account was not theirs.

The plugin also registers handlers of its own on `data.user.created`, `data.user.beforeUpdate`, and `data.user.updated`, with the change flow on `data.user.beforeCreate`, and with `RequireVerified` on `auth.signIn.credentialsVerified`. Use `emailverification.PluginName` in `HookOptions.Before`/`After` to order a handler around them.

## Rate limits

| Rule | Limits | Default |
| --- | --- | --- |
| `emailverification.RuleSendAddress` | sends a client asks for (`POST /email/send-verification`, `SendVerification`) per client address | 10 a minute |
| `emailverification.RuleVerifyAddress` | verify attempts per client address | 10 a minute |
| `emailverification.RuleSendEmail` | links sent per email | `Options.SendLimit`, 5 per 15 minutes |

With the change flow on there are four more: 10 a minute per client address on each of its three flows (`RuleChangeAddress`, `RuleChangeConfirmAddress`, `RuleChangeRevertAddress`), and `RuleChangeUser`, which allows `Options.ChangeLimit` requests per user (3 an hour) from any caller. Each request mails the current address, and this rule bounds that.

The rules are attached to the flows' before points, so they hold however a flow is called. The rule per address on sends counts the sends a client asks for, and not the automatic one for a new user or a changed address. Change a limit, or turn a rule off, by its name with `RateLimitConfig`; see [Hooks](hooks.md#change-a-rule-by-its-name).

The rule per email is attached to `auth.emailVerification.beforeSend`, so it counts every send: the automatic one for a new user, the route, and `SendVerification` from code. From the route it counts requests for unknown addresses too, so a `429` does not show whether the address has an account.

## Limits

The plugin has these limits:

- **Verifying does not sign the user in.** It sets the flag and creates no session.
- **A revert of a pending change does not stop a confirmation that is already running.** When the revert link is used before the change was confirmed, and the confirmation link is used at the same moment, the confirmation can still apply the change after the revert has answered. After a change was confirmed, a revert always wins over a confirmation or a newer revert that runs at the same moment.
- **A session is not refreshed in place.** A user whose session is no longer fresh signs in again before changing their address.
