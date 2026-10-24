# Username

The `username` plugin gives users a unique username and lets them sign in with it and their password. It builds on the [email and password](emailpassword.md) plugin: the password, the sign-up route, and the email sign-in stay there, and a user can sign in either way.

## Setup

Build the email/password plugin first and hand it to `username.New`. Pass both to `Prepare`, in any order.

```go
import (
	"github.com/go-behemoth/behemoth/plugins/emailpassword"
	"github.com/go-behemoth/behemoth/plugins/username"
)

passwords := emailpassword.New(emailpassword.Options{})
names := username.New(passwords, username.Options{})

app, err := engine.Prepare([]core.Plugin{passwords, names}, engine.PrepareConfig{})
// ...
ac, err := engine.Boot(ctx, app, db, engine.BootConfig{ /* ... */ })
```

The setup has these requirements:

- The plugin's name is `username` (`username.PluginName`). It depends on `emailpassword`, so `Prepare` fails when that plugin is missing.
- It adds two columns to `users`, both nullable strings of up to 64 characters: `username`, which is unique, and `display_username`. Generate and apply a migration after adding the plugin. On MongoDB, run `EnsureIndexes`. See [MongoDB](mongodb.md).
- Build the database adapter with `app.Resolver`, as for any plugin that adds a column to a core table. Without it the adapter does not read the columns.
- On SQLite, open the database with `_txlock=immediate`. The plugin checks a username inside the write's transaction, and without that setting two writes at the same moment can fail with "database is locked".

## Options

The zero value works.

| Field | Default | Meaning |
| --- | --- | --- |
| `MinLength` | 3 | Shortest username accepted, in characters. |
| `MaxLength` | 30 | Longest username accepted. At most 64 (`username.MaxColumnLength`). |
| `Reserved` | none | Usernames nobody may take, such as `admin`. `Admin` in the list also covers `admin`. |
| `RequireAtSignUp` | false | Refuse a sign-up that names no username. By default a username is optional. |
| `AvailabilityRoute` | false | Mount `POST /username/available`. See [Check availability](#check-availability). |

A minimum above the maximum, a negative limit, a maximum above 64, and a nil email/password plugin are configuration errors from `Boot`.

## The rules

A username follows these rules:

- A username holds the letters `a` to `z`, digits, `_`, and `.`, and starts with a letter or a digit.
- It is stored trimmed and lowercased. `Ada` and `ada` are one username. The form the user typed is kept next to it. See [The display username](#the-display-username).
- It can't contain `@`, so a username is never an email address.
- It is unique. A user without a username has no value in the column, and any number of users can go without one.
- The length limits apply where a username is set, not at sign-in. Lowering `MaxLength` later does not lock out a user whose username is longer.

The rules are applied to every write of a user: a sign-up, `Plugin.SetUsername`, and a `Store.UpdateUser` or `Store.CreateUser` of your own. `names.Validate(username)` runs them without writing, and returns the stored form.

## The display username

`display_username` holds the username as the user typed it, for showing: `Ada_L` for the username `ada_l`. Sign-in and uniqueness use `username` only. The display username follows these rules:

- You don't have to send it. It is set from the username whenever a username is set.
- When you do send it, at sign-up or in an update, it has to spell the username: `ADA_L` passes for `ada_l`, and `Grace` does not. This keeps a user from showing another user's name.
- An update that sets `display_username` alone changes the spelling and leaves the username. An empty one puts the lowercase form back.
- A user without a username has no display username. Removing the username removes both.
- It is a public column, so a user in a response has `display_username` next to `username`.

## Sign up

There is no extra route. Send `username` with the other sign-up fields:

```
POST /sign-up/email
{"email": "ada@example.com", "password": "correct horse", "username": "Ada"}
```

The response is the email/password plugin's `201` with `{"user": {...}}`, and the user now has `"username": "ada"` and `"display_username": "Ada"`. `display_username` can be sent too. Both are checked before the password is hashed.

| When | Status | Body |
| --- | --- | --- |
| the username breaks a rule, is reserved, or is not a string | `400` | `{"error": "<which rule>", "code": "username_invalid_input"}` |
| `RequireAtSignUp` is set and the body has no username | `400` | `{"error": "a username is required", "code": "username_invalid_input"}` |
| `display_username` is not a string, does not spell the username, or comes without one | `400` | `{"error": "<which rule>", "code": "display_username_invalid_input"}` |
| another user holds the username | `409` | `{"error": "this username is taken", "code": "username_taken"}` |

A `409` shows that a username exists, as the email/password plugin's `email_taken` does for an address. Two sign-ups that ask for the same new username at the same moment are both checked before either is written. The database then refuses the second, which also gets `409` with `username_taken`. Its message is "username is already taken".

## Sign in

| Route | Body | Result |
| --- | --- | --- |
| `POST /sign-in/username` | `username`, `password`. Other fields are not used by sign-in, but handlers on `auth.signIn.before` see them. | The same as `POST /sign-in/email`: `200` with `{"user": {...}}`, or `{"status": "requires_second_factor"}`, and the session token by the configured delivery. See [Sessions](sessions.md). |

| When | Status | Body |
| --- | --- | --- |
| unknown username, wrong password, or a user who has no password | `401` | `{"error": "invalid username or password", "code": "invalid_credentials"}` |
| `username` or `password` is not a string | `400` | `username_invalid_input`, `password_invalid_input` |
| the body is not valid JSON | `400` | `request_validation_error` |
| more than 10 attempts a minute from one client address | `429` with `Retry-After` | `rate_limited` |
| too many passwords are being hashed at once; see [Bound the password hashes that run at once](emailpassword.md#bound-the-password-hashes-that-run-at-once) | `503` with `Retry-After` | `server_busy` |

The `401` is the same for the three refusals, so the response does not show which usernames exist. Its code is `emailpassword.ErrorCodeInvalidCredentials`, the code of a refused email sign-in. The username is matched in any casing. An email address in the `username` field finds no user: use `POST /sign-in/email` for that.

The user in a response has the `username` and `display_username` keys next to the core columns. They are `null`, or absent, for a user without a username.

## Check availability

A sign-up form can ask whether a username is free before the user submits. The route is off until you set `Options.AvailabilityRoute`, because it tells anyone who asks which usernames exist. Sign-up's `409` tells the same, one account-creating request at a time. This route makes it cheap, so it has a rate limit of its own.

| Route | Body | Result |
| --- | --- | --- |
| `POST /username/available` | `username` | `200` with `{"available": true}` or `{"available": false}` |

| When | Status | Body |
| --- | --- | --- |
| the username is taken or reserved | `200` | `{"available": false}` |
| the username is missing, not a string, or breaks a rule of length or characters | `400` | `{"error": "<which rule>", "code": "username_invalid_input"}` |
| more than 30 checks a minute from one client address | `429` with `Retry-After` | `rate_limited` |

`true` is not a reservation. Another user can take the name before the sign-up arrives, and the sign-up checks again.

## Call the flows from code

```go
result, err := names.SignIn(ctx, username.Credentials{
	Username: "ada", Password: "correct horse",
	Extra:    behemoth.M{"captchaToken": token},
})
// result.User, result.Session, result.RawToken; result.Method is "username"

admin := identity.WithCaller(ctx, identity.System("support-tool"))
user, err := names.SetUsername(admin, userID, "Ada.L") // stores "ada.l", shows "Ada.L"; "" removes both

free, err := names.Available(ctx, "ada.l") // works with the route on or off

name, ok, err := username.Column.Get(user)         // ok is false for a user without one
shown, ok, err := username.DisplayColumn.Get(user) // "Ada.L"
```

The flows behave as follows:

- `SignIn` is the flow behind the route. It returns the raw session token and delivers nothing. From a route of your own, hand the result to `httpapi.WriteSignIn`.
- `SetUsername` applies the rules and returns the same typed errors as sign-up (`username.ErrorCodeInvalidUsername`, `username.ErrorCodeUsernameTaken`). It is an admin operation: it requires `username.PermissionSetUsername` (`user:set_username`), which only a system caller holds, so your code names itself with `identity.System(name)`. A signed-in user gets `403` with the code `insufficient_scope`. The old username is free for another user at once.
- To give usernames to users you had before adding the plugin, call `SetUsername` for each of them from a job, and handle the two errors for names that break the rules or collide.
- `Available` returns false for a taken or reserved username, and a validation error for one that breaks a rule of length or characters.
- `username.Column` and `username.DisplayColumn` are the typed columns (`schema.Field[string]`). `Store.UpdateUser(ctx, id, behemoth.M{username.Column.Name: "ada"})` goes through the same checks. To change only the spelling, update `username.DisplayColumn.Name`.
- Before `Boot`, the flows return a configuration error.

## Hooks

The plugin declares no points. A sign-in by username fires the `auth.signIn.*` points, with the same failure codes as an email sign-in:

| Point | What a handler sees |
| --- | --- |
| `auth.signIn.before` | `username` in its stored form, the plaintext `password`, and the other fields of the request. The returned payload becomes the sign-in's input. |
| `auth.signIn.credentialsVerified` | the user's id, and the factor `password`, as for an email sign-in. A second-factor plugin and `emailverification`'s `RequireVerified` apply here. |
| `auth.signIn.after` | a `*core.SignInResult` with `Method` `"username"` |
| `auth.signIn.failed` | codes `user_not_found` (with the username in the audit event's metadata), `no_credential_account`, `invalid_credentials`, `rejected_by_hook` |

It registers three handlers: on `auth.signUp.before`, `data.user.beforeCreate`, and `data.user.beforeUpdate`. A refused username at sign-up fires `auth.signUp.failed` with `rejected_by_hook`.

## Rate limits

| Rule | Limits | Default |
| --- | --- | --- |
| `username.RuleAvailableRoute` | requests to `POST /username/available` per client address, declared with `AvailabilityRoute` | 30 a minute |

A sign-in by username is limited by core to 10 attempts a minute per client address and sign-in method (`engine.RuleSignInAddress`). The `username` method counts apart from sign-in by email, and the limit holds for `SignIn` called from your own code too. `Available` fires no hook point, so its limit is a route rule and holds for the route only. Change a limit, or turn a rule off, by its name with `RateLimitConfig`; see [Hooks](hooks.md#change-a-rule-by-its-name).

A sign-in by username publishes the username under `username.HookValueUsername` and publishes no email. A rule of yours on `auth.signIn.before` that counts per email does not count these sign-ins. To limit attempts per account, add a rule per username next to it:

```go
ic.RateLimits.DeclareHookRateLimitRule(core.HookRateLimitRule{
	Name: "app.signin.username", Point: hooks.HookSignInBefore,
	KeyFunc: core.KeyByValues(username.HookValueUsername),
	Limit:   core.Limit{Max: 20, Window: 15 * time.Minute},
})
```

The plugin declares no such rule itself: a limit per username lets anyone keep a user out by failing on purpose.

## Limits

The plugin has these limits:

- **No route to change a username or its spelling.** A route of your own that lets a user change their own checks the caller, then calls `SetUsername` with a system caller for that user's id.
- **No holding period for released usernames.** Another user can take a released username at once.
