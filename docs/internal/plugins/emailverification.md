# Email verification plugin

This document explains how `plugins/emailverification` works: how it finds out that a user needs a link without any sign-up code calling it, how its send and verify flows run, and why its sends never wait for the mail sender. It builds on the same pieces as the magic link plugin, and [`magiclink.md`](magiclink.md) explains those that are shared: the link that points at the application, the POST verify, the token's subject, the trusted-origin check and what a route hides.

The code lives in `plugins/emailverification/`: `emailverification.go` has the verification itself, and `change.go` the flow for changing an address, which the second half of this document covers.

## What the plugin contributes

| `core.Plugin` method | Result |
| --- | --- |
| `Meta` | name `emailverification`, no dependencies, no mount path |
| `Declare` | the `email_verification` token kind (single use, `Options.TTL`, database backend), six hook points, three rate-limit rules |
| `Register` | handlers on three user data points, a fourth with the change flow on, and with `RequireVerified` on `auth.signIn.credentialsVerified` |
| `Init` | checks the options and the mailer, parses `LinkURL`, wraps the two flows with `core.WithLifecycle` |
| `Routes` | `POST /email/send-verification`, `POST /email/verify` |

It is the first plugin that registers handlers. The email/password and magic link plugins only fire points.

## How it hears about users

The plugin has no dependency on a sign-up plugin, and no sign-up plugin calls it. It registers on the store's data points, which fire for every write to `users` whatever caused it.

| Point | Tier | Handler |
| --- | --- | --- |
| `data.user.created` | after commit | `sendTo(user)`: send a link if the user is unverified |
| `data.user.beforeCreate` | in the transaction | with the change flow on: refuse an address reserved for another user (`Store.CheckReservedValue`) |
| `data.user.beforeUpdate` | in the transaction | if the update changes the address: with the change flow on, refuse an address reserved for another user; set `email_verified` to false unless the update sets it, and leave a note in `Values` |
| `data.user.updated` | after commit | if the note is there: `sendTo(user)` |
| `auth.signIn.credentialsVerified` | flow | with `RequireVerified`: refuse an unverified user |

Compare the alternatives: a call from each sign-up plugin would make each of them know this one, and an OAuth plugin written later would have to remember it. A handler on `auth.signUp.after` would miss users created by a CLI or by a plugin that does not fire that point.

### The change of address

These two handlers are the fallback for a write that changes the address directly. They keep `EmailVerified` true to its name. They don't keep the old address until the new one confirms; that is the change flow's job (below), and the flow's own writes pass through them untouched.

An update's payload holds only the columns being changed, so the handler on `data.user.beforeUpdate` has to read the current row to tell a change from a rewrite of the same value:

1. No `email` key in the changes: return. This is the common case, and it costs nothing.
2. Read the user through `hctx.Tx`, with the id from `Values[HookValueUserID]`. The read is in the write's transaction.
3. Compare the normalized addresses. Equal: return.
4. If the changes don't set `email_verified`, set it to false. An update that sets it is left alone.
5. Set `Values["emailverification.emailChanged"]`.

`data.user.updated` shares the update's `Values`, so its handler sees the note and sends the link once the transaction has committed. The send has to wait for the commit: a link for an address the database then rolls back would confirm nothing.

The trigger follows these rules:

- **The plugin's own update does not trigger it.** `Verify` updates `email_verified` only, so step 1 returns.
- **The magic link plugin's update does not either**, for the same reason.

### After-commit sends are best effort

Both sends run in after-commit handlers. An error there is logged by the dispatcher and the write stands. That is the wanted behaviour: a user should not fail to sign up because a link could not be issued. The send route is the user's way to ask again.

## Sending a link

`sendVerification` resolves a request by user id before the flow, publishes the email in `Values` for the rate-limit rule, and calls the flow wrapped with `auth.emailVerification.beforeSend`, `.afterSend`, and `.sendFailed`. `sendBody`:

| Step | What | On failure |
| --- | --- | --- |
| 1 | check the email's shape and the redirect URL (`core.TrustedRedirect`) | a validation error |
| 2 | find the user by email, unless the request came with one | not found: `Fail` with `user_not_found`, return `ErrNoAccount` |
| 3 | the user is verified already | return `ErrAlreadyVerified`; no point fires |
| 4 | `RevokeAllForSubject(email_verification, user.ID)`, then `Issue` with `{email, redirect_url}` | `Fail` with `issue_failed` |
| 5 | `ac.Mailer.SendAsync(ctx, MailMessage{Kind: MailEmailVerification, ...})` | the message was not queued: revoke the token, `Fail` with `send_failed` |
| 6 | return a `*SendResult`; `WithLifecycle` fires `.afterSend` | |

- **Step 5 does not wait.** `afterSend` means the message was queued, not delivered. A send that fails later is logged by the mailer.
- **A handler can't change who the link goes to.** `SendRequest.FromMap` reads the redirect URL and the metadata back from the payload and ignores `email` once the request is in the flow.
- **The route hides steps 2 to 5**, as the magic link route does: `ErrNoAccount`, `ErrAlreadyVerified`, and an `afterLookupError` all become the `200` of a sent link.

## Verifying a link

`Verify` reads the token without consuming it, for the redirect URL and the email in the before payload, then runs `verifyBody` wrapped with `.beforeVerify`, `.afterVerify`, and `.verifyFailed`:

1. `Consume` the token. A rejection is `invalid_link`.
2. Find the user by the token's subject. Gone: `user_not_found`.
3. Compare the email in the token's metadata with the user's. Different: `email_changed`.
4. Set `email_verified` with `Store.UpdateUser`, unless it is set already.

Every refusal answers with one typed unauthorized error, `invalid_verification_link`. No session is created.

## Design decisions

### A plugin of its own

**Context:** Email verification could live in the email/password plugin as an option, in core, or in a plugin.

**Options considered:**

- *An option of the email/password plugin.* One plugin to install for the common case. An application that signs users in by OAuth or magic link only would load the password plugin to verify an address.
- *Core.* Always there. Core has no routes and composes no messages.
- *A plugin that listens to user writes.* Works with every way of creating a user. One more plugin to pass to `Prepare`.

**Decision:** A plugin. What it verifies is a column of the core `users` table, and nothing about it involves a password.

### Sends are always in the background

**Context:** The send is triggered by other operations: a sign-up, a profile update. The mailer offers `Send`, which waits, and `SendAsync`.

**Options considered:**

- *Wait.* A failure is known at once. Every sign-up waits for the mail provider, inside an after-commit handler that runs synchronously.
- *An option, as in the magic link plugin.* Flexible. There is no case here where waiting buys anything.
- *Never wait.* A failure is only logged.

**Decision:** Never wait. The operation is idempotent: a lost message costs the user a retry, a new link replaces the old one, and a request for a verified address does nothing. The magic link plugin differs because it acts on a failed send by revoking the link.

**Revisit if:** applications want the send to fail a sign-up. That would also need the send to run before the commit.

### A direct write of a new address takes effect at once

**Context:** When a write changes a user's address without going through the change flow, the handlers have to do something with it.

**Options considered:**

- *Replace and mark unverified.* The flag stays honest. The unconfirmed address is the account's identifier at once.
- *Refuse the write.* Forces everything through the change flow. An admin screen or an import could no longer set an address.

**Decision:** Replace and mark unverified. The handlers are a net under writes the plugin does not control, and the safe path is `RequestChange`. `docs/ongoing.md` has an entry on refusing such writes when the flow is on.

### `RequireVerified` hooks `auth.signIn.credentialsVerified`

**Context:** A sign-in of an unverified user has to be refused somewhere.

**Options considered:**

- *`auth.signIn.before`.* Earliest. The payload differs by sign-in method and has no user, and refusing before the password is checked tells anyone whether an address is verified.
- *`auth.signIn.credentialsVerified`.* It has the user id whatever the method, and runs after the credentials were accepted.

**Decision:** `credentialsVerified`. The sign-in flows report the refusal as `rejected_by_hook` in the audit event, like any handler's refusal, with the plugin's error as the cause.

## The email change flow

`change.go` adds a flow in which the new address has to confirm before it replaces the old one, and the old address is told and can undo it. It is off unless `Options.ChangeLinkURL` and `RevertLinkURL` are set; `Declare`, `Init`, and `Routes` each check `changeEnabled()`.

| Declared when on | Value |
| --- | --- |
| token kinds | `email_change` (to the new address, `ChangeTTL`) and `email_change_revert` (to the old address, `RevertTTL`); both single use, database backend, subject the user's id |
| hook points | `auth.emailChange.` `beforeRequest`, `afterRequest`, `requestFailed`; `beforeConfirm`, `afterConfirm`, `confirmFailed`; `beforeRevert`, `afterRevert`, `revertFailed`. After and failed are audited |
| hook rules per address | `emailverification.change.address`, `.change.confirm.address`, `.change.revert.address` on the three before points: 10 a minute per client address (`core.KeyByClientIP`) |
| hook rule | `emailverification.change.user` on `beforeRequest`: `Options.ChangeLimit` per `HookValueUserID` |

No table or column holds the pending change. The two addresses travel in the tokens' metadata:

| Token | Metadata | Read by |
| --- | --- | --- |
| `email_change` | `from` (the address the user had), `to` (the one asked for), `redirect_url` | `changeConfirmBody` |
| `email_change_revert` | `restore` (the address the user had) | `changeRevertBody` |

### Request

`RequestChange` publishes the user's id in `Values` and runs `changeRequestBody`:

| Step | What | On failure |
| --- | --- | --- |
| 1 | check the new address's shape, the redirect URL, and that it differs from the current address | a validation error |
| 2 | `RevokeAllForSubject(email_change, user)`: a new request replaces the pending one | `Fail` with `issue_failed` |
| 3 | issue the revert token, and reserve the current address until the token expires (`Store.ReserveValue`) | revoke the token, `Fail` with `issue_failed` |
| 4 | `Mailer.Send` the notice to the current address | revoke the token, `Fail` with `notice_failed` |
| 5 | look for another user with the new address, and for a reservation of it for another user | found: `Fail` with `email_taken`, return `ErrEmailTaken` |
| 6 | issue the change token and `Mailer.SendAsync` the confirmation to the new address | revoke the token, `Fail` with `send_failed` |

- **Nothing in `users` changes.**
- **Revert tokens are never revoked when a new one is issued.** Each belongs to its request. Why is under *The older revert link wins*.
- **Step 5 comes after the notice.** A request for a taken address then does the same work as one for a free address up to the last step, and the route answers both with the same `200`.
- **The current address is reserved at the request, not at the confirm.** The account holds the address until the confirm, so there is no moment in which it is free and not reserved, and no transaction is needed around the confirm's write. A request that fails after step 3 leaves the reservation to expire: an earlier request's link may need the same address reserved. Why the address is reserved at all is under *The old address is reserved while its link works*.
- **The new address is not reserved.** Step 5 is a courtesy check. The confirm finds out for real, from the unique constraint and the data handlers.

### Confirm

`ConfirmChange` reads the token without consuming it (`readChangeToken`: the user for `Values`, the address for the payload, the redirect URL for the result), then `changeConfirmBody`:

1. `Consume` the token. A rejection is `invalid_link`.
2. Find the user. Gone: `user_not_found`.
3. The user's address has to be the token's `from`. If not, a revert or another change came first: `email_changed`.
4. `Store.UpdateUserIfEmail` with `email` and `email_verified: true` in one write, guarded on the address read in step 2. A duplicate-key error is `email_taken`, answered with a `409`; a reservation of the new address for another user ends the same way, from the data handler. A guard that no longer holds is `email_changed`: a revert or another change wrote the account after step 2.

Step 4 goes through the plugin's own fallback handlers. `data.user.beforeUpdate` sees an update that sets the flag and leaves it alone. `data.user.updated` finds the note and a verified user, and sends nothing.

### Revert

`changeRevertBody`, after consuming the token and finding the user:

1. `RevokeAllForSubject(email_change, user)`: a pending change is dropped.
2. `revokeLaterReverts`: `Store.ListUnrevokedTokensForSubject(email_change_revert, user)`, used tokens included, and revoke every token that was not created before this one. It returns the addresses that only the revoked tokens that were still unused could put back.
3. `restoreAddress`, up to eight attempts of:
   1. read the user;
   2. read this token again: revoked means an older link's revert ran in the meantime, and the answer is `invalid_link`;
   3. if the user's address is `restore`, stop;
   4. `Store.UpdateUserIfEmail` with `email: restore` and `email_verified: true`, guarded on the address read in 3.1. A guard that no longer holds starts the next attempt. A duplicate-key error is remembered as taken and not returned yet.
4. Release the reservation of `restore`, if the account has it now, and of the addresses step 2 returned.
5. `SessionManager.RevokeAllForUser` with the reason `email_change_reverted`.
6. If step 3 found the address taken: `Fail` with `email_taken` and return the `409`.

The same link therefore cancels a change that is pending and undoes one that was applied. Steps 1, 2 and 5 run in both cases.

Step 3.2 comes after the read in 3.1 on purpose. An older revert revokes this token (its step 2) before it writes (its step 3.4). A read that sees the older revert's write is followed by a check that sees the revocation. A read from before that write leaves a guard that the write breaks.

### What the flow needed from core

| Addition | Where | Why |
| --- | --- | --- |
| `TokenManager.ListForSubject(kind, subject)` | `core/token.go`, `auth/token.go`, `Store.ListTokensForSubject` | the revert revoked some of a user's revert tokens and not all. It returns the usable tokens of a kind, oldest first; a key-value kind can't be listed. The revert now uses the store's listing below |
| `Store.ListUnrevokedTokensForSubject(kind, subject)` | `store/tokens.go` | step 2 of the revert also revokes later tokens that were used, whose reverts may be running. The token manager lists usable tokens only, and its interface did not grow for one caller |
| `Store.UpdateUserIfEmail(id, email, changes)` | `store/users.go` | the guarded writes of the confirm and the revert, in the shape of the store's guarded session update (`UpdateSessionAt`) |
| `reserved_values`, `Store.ReserveValue`, `CheckReservedValue`, `ReleaseReservedValue`, `FindReservedValue`, `PurgeExpiredReservedValues` | `models/reserved_value.go`, `store/reserved_values.go` | the old address's reservation. A core table, generic over the column ([`../models/models.md`](../models/models.md)) |
| `SessionManager.IsFresh(session)` and `httpapi.RequireFreshSession` | `core/session.go`, `auth/session.go`, `httpapi/session.go` | `SessionConfig.FreshAge` and `Session.FreshAt` existed and nothing read them. A session is fresh when it is active and `FreshAt` is within `FreshAge` (`DefaultFreshAge`, 15 minutes, when zero). `FreshAt` is set by `Create` and `Promote` |
| `MailMessage.Data`, two mail kinds | `core/mail.go` | the notice has to name the new address, which nothing else on the message carries |

`RequireFreshSession` wraps `RequireSession` and then checks `IsFresh`. A stale session gets a typed session error, `session_not_fresh`, which maps to `401`.

### Design decisions

#### The pending change lives in token metadata

**Context:** Between the request and the confirm, the new address has to be kept somewhere.

**Options considered:**

- *A `pending_email` column on `users`.* Visible in the row, and a settings page can show it directly. A schema change for every application, and a second place that has to agree with the token.
- *The token's metadata.* No schema change. The pending address expires with the token and is removed with the user. Showing "a change is pending" needs `ListForSubject`.

**Decision:** The metadata. The token already is the pending change: it has the lifetime, the single use and the revocation.

**Revisit if:** applications need to query pending changes across users.

#### One link at the old address, issued at the request

**Context:** The old address has to be able to stop a change it did not ask for.

**Options considered:**

- *A notice without a link.* The owner learns of it and can do nothing.
- *A cancel link that works until the confirm.* Someone who took over a session confirms from their own mailbox within seconds, so the link is nearly always too late.
- *Approval from the old address before the change.* The strongest. A user who lost the old mailbox, which is a common reason to change, can't change at all.
- *A link that works before and after the confirm, for some days.* It drops a pending change and undoes an applied one. The old mailbox keeps a say over the account for that long.

**Decision:** The fourth, sent when the change is requested and not when it is applied, so the owner hears of an attempt as early as possible. `RevertTTL` is 72 hours by default.

**Revisit if:** the window proves too long for users who change address because the old mailbox is compromised. A shorter default, or an option to send the notice without a link, would cover that.

#### The older revert link wins

**Context:** After two changes, `a@` to `b@` to `c@`, two revert links exist: one at `a@` that restores `a@`, one at `b@` that restores `b@`. If someone took the account over at the first change, `b@` is theirs.

**Options considered:**

- *A new request revokes the earlier revert links.* One live link per user, as for the other tokens. Whoever took the account over makes a second request and the owner's link is dead.
- *Using a revert link revokes all the others.* The holder of `b@` uses their link first and kills the owner's.
- *Links never affect each other.* The owner restores `a@`, and the holder of `b@` then uses their link to put `b@` back.
- *Using a link revokes the links issued after it.* A link can cancel later ones and never earlier ones.

**Decision:** The fourth. The address that held the account earliest has the last word, in whatever order the links are used. It needs to revoke a subset of a subject's tokens, which is what `ListForSubject` was added for. The comparison is "not created before" and not "created after", because some databases store the creation time to the second.

A used link is revoked too, since its revert may still be running: *Writes are guarded on the address that was read* explains why that is enough for the older link to win when the two reverts run at the same time.

#### The old address is reserved while its link works

**Context:** After a change from `a@` to `b@`, the revert link at `a@` puts `a@` back. If another account takes `a@` first, the revert can't. Someone who took an account over could sign up with `a@` on purpose; that needs no access to its mailbox.

**Options considered:**

- *Nothing.* The revert answers `email_taken`, and the owner goes to support.
- *At the revert, take the address from an account that never verified it*, and give that account a placeholder address. A sign-up with the address can only be unverified, since its mailbox is the owner's. The other account was given the address and loses it later, which is unfair to someone who signed up with it by mistake while verification is off.
- *Reserve the address while the link works*, and refuse it to every other account from the data handlers. The other account is refused at the start, before it has built anything on the address. Someone with a real claim to the address waits up to `RevertTTL`.

**Decision:** Reserve it, in a core table generic over the column (`column_name`, `column_value`), so that the username plugin can keep released usernames the same way. The reservation lasts as long as the link that can restore the address: it is written with the link's expiry and released when the revert puts the address back, or revokes the link. The data handlers refuse a reserved address with the same conflict as a taken one, so sign-up, OAuth, a confirm and a direct write all answer `email_taken` without knowing about reservations.

The handlers are registered only with the change flow on, so an application that does not use it never reads the table. `Store.DeleteUser` does delete from it for every user, which makes the migration a requirement for every application, as for `verification_codes`.

**Revisit if:** an address the account never verified should not be reserved (`docs/ongoing.md`), or applications want to release an address early.

#### Writes are guarded on the address that was read

**Context:** The confirm and the revert each read the user, decide from the address, and then write. A confirm or a newer revert that read the account before the owner's revert, and wrote after it, undid the revert: the owner's link was spent, and the account ended on an address the attacker controls.

**Options considered:**

- *Guard each write on the address that was read* (`UpdateUserIfEmail`, as `UpdateSessionAt` guards on the session's version), have a revert check its own link before each attempt, and revoke used later links. Works on every adapter through the `UpdateOne` convention that the store contract checks.
- *A version column on `users`*, written and checked by every confirm and revert. Also covers a revert that has nothing to write, which the first option does not. A schema change to `users` for one plugin.
- *A transaction per user that writes the user row first*, so that the two flows run one after the other. No schema change. Depends on each database's locking, which the contract tests do not cover.

**Decision:** The first. A revert that finds the account already at its address writes nothing, so a confirm of a change from that address, whose link was used before the revert revoked it, can still write after it. That case needs the confirm link and the revert link used at the same moment for a change that is still pending. It is deferred; `docs/ongoing.md` has it under "A confirm can still land after a revert that had nothing to write".

**Revisit if:** the remaining case matters enough for a column on `users`.

#### The notice waits, the confirmation does not

**Context:** The mailer offers `Send`, which waits and returns the sender's error, and `SendAsync`.

**Decision:** `Send` for the notice: the request fails if the notice can't be handed over, and no confirmation link is issued. The notice is the owner's safeguard, so a change does not start without it. `SendAsync` for the confirmation, like a verification link: if it is lost the user asks again.

**Revisit if:** the wait bothers applications whose sender is slow. They can give a sender that enqueues.

#### The request needs a fresh session

**Context:** The request route acts for the session's user.

**Options considered:**

- *Any valid session.* A session left open on a shared computer, or stolen, is enough to move the account.
- *The password again in the request.* Certain. It ties the plugin to the password plugin, and a user who signs in by magic link has none.
- *A fresh session.* Works for every sign-in method. A user whose session is older signs in again.

**Decision:** A fresh session, with `RequireFreshSession`. It is the use `FreshAge` was put in `SessionConfig` for.

The check has since moved from the route into the flow: `RequestChange` requires `email:change` through `Access.RequireFreshUser`, and the route has no middleware. The rule now holds for every caller of the flow, and code in the process passes by naming the user (`identity.System(name).For(userID)`).

**Revisit if:** a way to re-check credentials on an existing session is added. The user would then not have to sign in again.

## Tests

| Test | File | Covers |
| --- | --- | --- |
| `TestEmailVerificationAfterSignUp` | `tests/plugins/emailverification_test.go` | a password sign-up returns while the sender is blocked; the message; verify once; a verified or unknown user is sent nothing; the audit events |
| `TestEmailVerificationAfterEmailChange` | same | the flag cleared and a link sent on a change, nothing on other updates or the same address respelled, an update that sets the flag itself |
| `TestEmailVerificationRoutes` | same | the send route's identical answers, validation, the verify route's body and its `401` |
| `TestEmailVerificationRequiredToSignIn` | same | `403` until verified, and a wrong password still `401` |
| `TestEmailVerificationSendsAreLimitedPerEmail` | same | the rule per email counts the automatic send and unknown emails |
| `TestEmailVerificationOptions` | same | the configuration errors, and a flow called before `Boot` |
| `TestEmailChangeConfirmAndRevert` | `tests/plugins/emailchange_test.go` | the two messages and their `Data`; nothing changes before the confirm; the confirm keeps sessions; the revert restores the address and ends them; each link works once; the audit events |
| `TestEmailChangeRevertBeforeConfirm` | same | a new request replaces the pending one; a revert drops a pending change and ends later revert links |
| `TestEmailChangeOlderRevertLinkWins` | same | `a@` to `b@` to `c@`, with the two links used in either order |
| `TestEmailChangeToAnAddressThatIsTaken` | same | at the request, between request and confirm, and at the revert, after a write that bypassed the reservation |
| `TestEmailChangeConfirmCannotUndoARevert` | same | a confirm that read the account before a revert and writes after it is refused (the revert runs in a `lookupGap`) |
| `TestEmailChangeNewerRevertCannotUndoAnOlderOne` | same | the same for a revert by a newer link against one by an older link |
| `TestEmailChangeKeepsTheOldAddress` | same | the old address refused to a sign-up, another account's change, and a direct write; the revert puts it back and releases it |
| `TestEmailChangeReservationEnds` | same | the account changes back to its old address; the reservation ends with its time, with the account, and when an older link revokes the link |
| `TestEmailChangeNeedsItsNotice` | same | a failed notice fails the request, sends no confirmation and leaves no live link |
| `TestEmailChangeRoutes` | same | no session, a fresh one, a stale one; the same answer for a taken address; validation; confirm and revert by route |
| `TestEmailChangeRequestsAreLimitedPerUser` | same | the rule per user |
| `TestEmailChangeIsOptIn` | same | the flow off, and one page without the other |
| `TestTokenListForSubject` | `tests/store/managers_test.go` | `ListForSubject`: order, and what is left out |
| `TestStoreContract/*/Tokens` | `tests/store/contract_test.go` | `ListForSubject` and `ListUnrevokedTokensForSubject` on every database backend |
| `TestStoreContract/*/Users` | same | `UpdateUserIfEmail` |
| `TestStoreContract/*/ReservedValues` | same | the reservation operations |
| `TestSessionFreshness` | same | `IsFresh` and `RequireFreshSession` |
