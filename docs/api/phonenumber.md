# Phone number

The `phonenumber` plugin lets a user confirm a phone number with a one-time code sent by text message, and then sign in with the number and a code, or with the number and their password. It can also reset a password with a code.

It builds on the [email and password](emailpassword.md) plugin, and it does not send text messages itself: it hands each code to the [SMS sender](sms.md) you give Behemoth at `Boot`.

## How a user gets a number

A user signs up by email and adds a number while signed in. Sign-up with a number alone is not supported.

| Step | Who | What happens |
| --- | --- | --- |
| 1 | signed-in client | posts the number to `POST /phone-number/send-otp` |
| 2 | plugin | issues a code and hands it to your SMS sender |
| 3 | you | send the text message |
| 4 | signed-in client | posts the number and the code to `POST /phone-number/verify` |
| 5 | plugin | writes the number to the user |

The number is written at step 5 and not before, so a number on a user is always one that was confirmed. Changing a number is the same five steps with the new number.

## Setup

```go
import (
	"github.com/go-behemoth/behemoth/plugins/emailpassword"
	"github.com/go-behemoth/behemoth/plugins/phonenumber"
)

passwords := emailpassword.New(emailpassword.Options{})
phones := phonenumber.New(passwords, phonenumber.Options{})

app, err := engine.Prepare([]core.Plugin{passwords, phones}, engine.PrepareConfig{})
// ...
ac, err := engine.Boot(ctx, app, db, engine.BootConfig{
	SMS: core.SMSConfig{
		Sender: core.SMSSenderFunc(func(ctx context.Context, msg core.SMSMessage) error {
			return provider.Send(ctx, msg.To, "Your code is "+msg.Code)
		}),
	},
	// ...
})
```

- The plugin's name is `phonenumber` (`phonenumber.PluginName`). It depends on `emailpassword`, so `Prepare` fails when that plugin is missing.
- `Boot` fails without an SMS sender.
- It adds one column to `users`: `phone_number`, a unique, nullable string. It also uses the core `verification_codes` table. Generate and apply a migration after adding the plugin. On MongoDB, run `EnsureIndexes`.
- Build the database adapter with `app.Resolver`, as for any plugin that adds a column to a core table.
- On SQLite, open the database with `_txlock=immediate`. See [Username](username.md#setup) for the reason.

Your sender gets three kinds of message, as a `core.SMSMessage` with the code in `Code`:

| `Kind` | When |
| --- | --- |
| `core.SMSPhoneVerification` | a signed-in user is adding a number |
| `core.SMSSignIn` | a user is signing in by code |
| `core.SMSPasswordReset` | a user asked to reset their password |

## Options

The zero value works.

| Field | Default | Meaning |
| --- | --- | --- |
| `CodeLength` | 6 | Digits of a code, 4 to 12. |
| `CodeTTL` | 5 minutes | How long a code works. |
| `MaxAttempts` | 5 | How many times one code may be tried, the right guess included. |
| `SendLimit` | 3 per 15 minutes | Codes sent per number, for every purpose together. |
| `WaitForSend` | false | false: the request returns once the message is queued, and a failed send is logged. true: the request waits for your sender, and a failed send revokes the code. |
| `PasswordReset` | false | Turn on [password reset by code](#password-reset) and mount its two routes. |

## Phone numbers

A number is accepted and stored in E.164 form: `+`, the country code and the rest, 8 to 15 digits, such as `+14155550123`. Surrounding space is dropped. A national number (`0911 234567`) or one with spaces or dashes inside is refused with `phone_number_invalid_input`, because turning it into E.164 needs the user's country. Format the number on the client before sending it. `phonenumber.Parse` runs the same check.

## Routes

Body keys are snake case.

| Route | Body | Result |
| --- | --- | --- |
| `POST /phone-number/send-otp` | `phone_number`, and optionally `metadata` (an object passed to your sender) | `200` with `{"status": "ok"}` |
| `POST /phone-number/verify` | `phone_number`, `code` | With a session: `200` with `{"user": {...}}`, the user now holding the number. Without one: the sign-in response, as `POST /sign-in/email` gives it. |
| `POST /sign-in/phone-number` | `phone_number`, `password`. Other fields reach handlers on `auth.signIn.before`. | The sign-in response. |
| `POST /phone-number/request-password-reset` | `phone_number`, optionally `metadata` | `200` with `{"status": "ok"}`. Mounted with `PasswordReset`. |
| `POST /phone-number/reset-password` | `phone_number`, `code`, `password` | `200` with `{"status": "password_reset"}`. No session token. Mounted with `PasswordReset`. |

**`send-otp` and `verify` do one of two things, decided by the session token on the request:**

- **With a session**, they add the number to the caller. The code is tied to that user and that number.
- **Without one**, they sign in the user who holds the number.
- A code sent for one purpose does not work for the other.
- A request that carries a session token that is expired or revoked is refused with `401`. It is not treated as a request without a session.

### Error responses

| Route | When | Status | Code |
| --- | --- | --- | --- |
| any | the body is not valid JSON | `400` | `request_validation_error` |
| any | `phone_number` is not a string, or not in E.164 form (sign-in by password answers `401` for the second) | `400` | `phone_number_invalid_input` |
| `send-otp`, `request-password-reset` | more codes for this number than `SendLimit` allows | `429` | `rate_limited` |
| `verify`, `reset-password` | the code is wrong, expired, used, or out of attempts, was sent for another number or purpose, or nobody holds the number | `401` | `invalid_phone_code` |
| `verify`, with a session | another user took the number since the code was sent | `409` | `phone_number_taken` |
| `sign-in/phone-number` | nobody holds the number, wrong password, or a user without a password | `401` | `invalid_credentials` |
| `reset-password` | the password fails the email/password plugin's rules | `400` | `password_invalid_input`. The code keeps its attempts. |
| any | more than 10 requests a minute from one client address | `429` | `rate_limited` |
| `sign-in/phone-number`, `reset-password` | too many passwords are being hashed at once; see [Bound the password hashes that run at once](emailpassword.md#bound-the-password-hashes-that-run-at-once) | `503` with `Retry-After` | `server_busy` |

**What the responses do not show:**

- `send-otp` and `request-password-reset` answer `{"status": "ok"}` whether or not a code went out. Whether nobody holds the number, another user holds it, the caller holds it already, or the sender failed, the answer is the same.
- `verify` and `reset-password` give one error for a wrong code and for a number nobody holds.
- By default the code is sent in the background, so your provider's speed does not show in the response time either.

## What a code does

A code has these properties:

- **It works once**, and for five tries. A wrong guess costs a try, also when several arrive at the same moment. After the last try the code is dead and the user asks for another.
- **A new code replaces the old one** for the same user and purpose.
- **Only a keyed hash is stored**, in `verification_codes`.
- **It is tied to the number it was sent to.** A code sent to one number does not confirm another.
- **A user's codes are deleted with the user.**

## Direct writes are refused

A number is set by confirming a code. A write that sets `phone_number` any other way is refused with a `403` and the code `phone_number_unconfirmed_change`:

```go
_, err := ac.Store.UpdateUser(ctx, id, behemoth.M{phonenumber.Column.Name: "+14155550123"}) // refused
_, err = ac.Store.UpdateUser(ctx, id, behemoth.M{phonenumber.Column.Name: nil})             // removes the number
```

The refusal behaves as follows:

- This holds for a route of yours that updates a user from a request body, for `Store.CreateUser` with a number on the row, and for another plugin.
- Removing the number passes.
- When your own code vouches for a number (an admin screen, an import of numbers confirmed elsewhere), call `phones.SetPhoneNumber(ctx, userID, number)` with a system caller on `ctx`; see below.

## Password reset

Password reset is off until you set `Options.PasswordReset`. A text message is a weaker proof than a link in a mailbox: a number can be moved to another SIM. Turn it on when your users have no usable email, or accept that trade. The flow has three steps:

1. The client posts the number to `POST /phone-number/request-password-reset`.
2. The user receives a code (`core.SMSPasswordReset`).
3. The client posts the number, the code, and the new password to `POST /phone-number/reset-password`.

The reset itself is the email/password plugin's: the same password rules, every session of the user ends, no session is created, and the user gets the `core.MailPasswordChanged` notice when a mail sender is configured. The password is checked before the code, so a refused password does not cost a try.

## Call the flows from code

```go
forUser := identity.WithCaller(ctx, identity.System("support-tool").For(userID))
sent, err := phones.SendCode(forUser, phonenumber.SendRequest{PhoneNumber: number}) // add a number to the user
sent, err = phones.SendCode(ctx, phonenumber.SendRequest{PhoneNumber: number})      // sign in: an anonymous caller
// sent.Sent, sent.Reason, sent.ExpiresAt

admin := identity.WithCaller(ctx, identity.System("support-tool"))
user, err := phones.AddNumber(admin, userID, number, code) // admin: a user adds their own with Verify
result, err := phones.SignInWithCode(ctx, number, code)
result, err = phones.SignIn(ctx, phonenumber.Credentials{PhoneNumber: number, Password: password})

sent, err = phones.RequestPasswordReset(ctx, phonenumber.ResetRequest{PhoneNumber: number})
user, err = phones.ResetPassword(ctx, number, code, newPassword)

user, err = phones.SetPhoneNumber(admin, userID, number) // admin; no code; "" removes the number
number, ok, err := phonenumber.Column.Get(user)
```

The flows behave as follows:

- `SendCode` and `RequestPasswordReset` tell a caller in code what the routes hide. `sent.Sent` is false when no code went out, and `sent.Reason` is `phonenumber.ReasonNoAccount`, `ReasonNumberTaken`, `ReasonAlreadySet`, or `ReasonSendFailed`. Their only errors are a number that is not E.164, a rate limit, a hook's rejection, and a failure of the system.
- `SendCode` and `Verify` read the caller on `ctx`, as the routes do. For a caller that acts for a user they add a number to that user, and require `phonenumber.PermissionPhoneChange` (`phone:change`), which a signed-in user holds. For an anonymous caller they sign in. A caller whose session token failed gets the reason, and a system caller that names no user is a configuration error.
- `Verify(ctx, VerifyRequest{...})` is `AddNumber` for the caller's user, and `SignInWithCode` for an anonymous caller. The route calls it.
- `SignIn` and `SignInWithCode` return the raw session token and deliver nothing. From a route of your own, hand the result to `httpapi.WriteSignIn`.
- `AddNumber` and `SetPhoneNumber` are admin operations. They require `phonenumber.PermissionAddPhone` (`user:add_phone`) and `phonenumber.PermissionSetPhone` (`user:set_phone`), which only a system caller holds: a signed-in user gets `403` with the code `insufficient_scope`.
- Before `Boot`, the flows return a configuration error.

## Hooks

Signing in, by code or by password, fires core's `auth.signIn.*` points, with `Method` `"phonenumber"` in the result. The plugin declares these of its own:

| Point | Phase | Payload or result |
| --- | --- | --- |
| `auth.phoneNumber.beforeSend` (`phonenumber.HookSendBefore`) | before | `phone_number`, `purpose` (`verify`, `sign_in`, `password_reset`), and `userID` for a signed-in caller. Veto only. |
| `auth.phoneNumber.afterSend` (`phonenumber.HookSendAfter`) | after | a `*phonenumber.SendResult`. Not the code. |
| `auth.phoneNumber.sendFailed` (`phonenumber.HookSendFailed`) | failed | codes `no_account`, `number_taken`, `already_set`, `send_failed`, `rejected_by_hook` |
| `auth.phoneNumber.beforeVerify` (`phonenumber.HookVerifyBefore`) | before | `phone_number`, `userID`. Veto only. |
| `auth.phoneNumber.afterVerify` (`phonenumber.HookVerifyAfter`) | after | the `*models.User` |
| `auth.phoneNumber.verifyFailed` (`phonenumber.HookVerifyFailed`) | failed | codes `invalid_code`, `number_mismatch`, `number_taken`, `rejected_by_hook` |
| `auth.phoneNumber.beforePasswordReset` (`phonenumber.HookResetBefore`) | before | `phone_number`, `password`. Veto only. Declared with `PasswordReset`. |
| `auth.phoneNumber.afterPasswordReset` (`phonenumber.HookResetAfter`) | after | the `*models.User` |
| `auth.phoneNumber.passwordResetFailed` (`phonenumber.HookResetFailed`) | failed | codes `no_account`, `invalid_code`, `rejected_by_hook` |

The after and failed points are audited. On `auth.signIn.before`, a sign-in by code has `method` and `phone_number`, and one by password has `phone_number`, `password`, and the request's other fields. `auth.signIn.failed` has the code `invalid_code` for a refused code and `rejected_by_hook` for a sign-in a before handler refused.

Every flow publishes the number under `phonenumber.HookValuePhoneNumber` in `Values`, for rate-limit rules.

## Rate limits

| Rule | Limits | Default |
| --- | --- | --- |
| `phonenumber.RuleSendNumber` | codes per number, from any caller and for any purpose | `SendLimit`, 3 per 15 minutes |
| `phonenumber.RuleSendAddress` | codes asked for with `SendCode` per client address | 10 a minute |
| `phonenumber.RuleVerifyAddress` | verify attempts per client address | 10 a minute |
| `phonenumber.RuleResetRequestAddress`, `RuleResetAddress` | reset codes asked for, and reset attempts, per client address | 10 a minute each |

A sign-in, with a code or a password, is limited by core to 10 attempts a minute per client address and sign-in method (`engine.RuleSignInAddress`); the phone number method counts apart from the others. The rules per address are on the flows' before points, so they hold for the flows called from your own code or another transport. Change a limit, or turn a rule off, by its name with `RateLimitConfig`; see [Hooks](hooks.md#change-a-rule-by-its-name).

The rule per number is attached to `auth.phoneNumber.beforeSend`, so it holds for `SendCode` called from code. It counts requests for numbers nobody holds too, so a `429` does not show whether a number has an account. Each message costs money and reaches somebody's phone, which is why this limit is tighter than the one for email links.

Guessing a code is bounded by the code itself: 5 tries per code and 3 codes per 15 minutes.

## Limits

The plugin has these limits:

- **No sign-up with a phone number and no email.** `users.email` is required.
- **No national number formats.** Format the number as E.164 on the client.
- **No text message notice** when a number is changed or a password is reset.
