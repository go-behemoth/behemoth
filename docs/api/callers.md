# Callers and permissions

Every call of a Behemoth flow runs for a caller: the user, service, or code it acts for, and what the transport knows about where it came from. This page covers how to read the caller, how your own code names itself, how a transport other than HTTP authenticates its calls, and how flows require permissions.

## The caller

A caller is an `identity.Caller` on the call's `context.Context`. The router authenticates every request before its route runs and puts the caller on the request's context. A flow reads it from the context it is given, so a flow called from a route, a job, or another transport gets the same checks.

A caller holds these values:

| Method | Returns |
| --- | --- |
| `Principal()` | who the call acts for: a `user`, a `service`, or the `system`, with an id. It is zero for an anonymous caller. |
| `Delegate()` | who acts for the principal, when it isn't the principal itself: the user who impersonates it, or the system code that runs a flow for a user. It is zero otherwise. |
| `Session()` | the session the caller signed in with, as an `identity.SessionRef`: its id, its user, its state, its expiry, and when the user last proved a credential |
| `Peer()` | what the transport knows: the protocol (`"http"`), the client's address, resolved with `RouterConfig.TrustedProxies`, and the user agent |
| `IsAnonymous()` | whether the caller has no principal |
| `AuthError()` | why an anonymous caller is anonymous when it presented a credential that failed, such as a session error with the code `session_expired` |

A context without a caller holds the zero `Caller`: anonymous, with nothing known about it, and holding no permission.

## Read the caller

In a route handler, read the caller from the request's context:

```go
func me(rctx *httpapi.RequestContext) error {
	session, ok := identity.CallerFrom(rctx.Ctx).Session()
	if !ok {
		return errs.NewSessionError("me", errs.CodeSessionMissing, nil)
	}
	return rctx.Response.JSON(http.StatusOK, behemoth.M{"user_id": session.UserID})
}
```

A route that needs a signed-in user can put `httpapi.RequireSession()` in front of the handler instead; see [Sessions](sessions.md#protect-your-own-routes).

In a hook handler, `hctx.Caller()` returns the caller of the operation that fired the point. It is the same value as `identity.CallerFrom(hctx.Ctx)`.

## Name your code as the caller

Code in your process, such as a job, a command, or an admin screen, puts a system caller on the context. The audit log records its name:

```go
ctx = identity.WithCaller(ctx, identity.System("purge-inactive"))
```

A system caller holds every permission. It acts for nobody, so a flow that acts on the caller's own account, such as an email change, refuses it with a configuration error. To run such a flow for a user, name the user with `For`:

```go
ctx = identity.WithCaller(ctx, identity.System("support-tool").For(userID))
result, err := verification.RequestChange(ctx, emailverification.ChangeRequest{NewEmail: "ada@new.example.com"})
```

`For` returns a caller whose principal is the user and whose delegate is the system caller. The audit log records the user as the actor and the system caller as the delegate. It holds every self-service permission over that user's account, and no admin permission. Called on a caller that isn't a system caller, or with an empty user id, it returns an anonymous caller whose `AuthError` is `identity.ErrInvalidFor`.

Code that names no caller runs as the zero caller: anonymous. Public flows, such as sign-up and sign-in, work for it, and the audit log records it as the system.

## Authenticate calls from another transport

A transport other than HTTP, such as a gRPC server, authenticates each call through `AuthContext.Authenticator`, as the router does, and puts the caller on the context before it calls a flow:

```go
func callerContext(ctx context.Context, ac *core.AuthContext, token, address, agent string) context.Context {
	peer := identity.Peer{Transport: "grpc", IPAddress: address, UserAgent: agent}
	var cred *core.Credential
	if token != "" {
		cred = &core.Credential{Kind: core.CredentialSession, Value: token}
	}
	caller, _ := ac.Authenticator.Authenticate(ctx, cred, peer)
	return identity.WithCaller(ctx, caller)
}
```

A call without a credential gives an anonymous caller. A credential that fails gives an anonymous caller with the reason in `AuthError`, so a public flow still runs, and a flow that requires a user returns the reason. The second value, a `core.AuthOutcome`, says what the client may need back: `Extended` holds the session when rolling expiration extended it, and `Ended` is true when the session is gone for good. A client that keeps its token without a date can ignore both.

Rate limits per client address count calls from your transport by the address you put in the `Peer`. A caller without an address is not counted by them.

## What a caller may run

Flows fall into three groups:

| Caller | Public flows | Self-service flows | Admin operations |
| --- | --- | --- | --- |
| anonymous, no credential | yes | `401` with `session_missing` | `401` with `session_missing` |
| anonymous, a credential that failed | yes | `401` with the reason, such as `session_expired` or `session_pending` | `401` with the reason |
| a user signed in with a session | yes | yes, for their own account | `403` with `insufficient_scope` |
| `identity.System(name).For(userID)` | yes | yes, for that user | `403` with `insufficient_scope` |
| `identity.System(name)` | yes | a configuration error: name the user with `For` | yes |

- **Public flows** need no caller: sign-up, sign-in, the requests for links and codes, and every verify by token or code. A token or code grants its one action.
- **Self-service flows** act on the caller's own account, and take the account from the caller, never from their input.
- **Admin operations** act on an account named in their input. Only a system caller passes them.
- **A failed lookup** of the session, such as a database that is down, is returned unchanged and answered with `500`.

## Built-in permissions

Each flow that requires a caller requires a permission. A signed-in user holds every self-service permission, and a system caller every permission.

| Permission | Kind | Required by |
| --- | --- | --- |
| `session:sign_out` | self | `emailpassword.Plugin.SignOut` |
| `email:change` | self, with a fresh session | `emailverification.Plugin.RequestChange` |
| `session_token:issue` | self, with a session | `core.SessionTokens.Issue` |
| `phone:change` | self, when the caller acts for a user | `phonenumber.Plugin.SendCode`, `phonenumber.Plugin.Verify` |
| `oauth_account:read` | self | `oauth.Plugin.Accounts` |
| `oauth_account:manage` | self, with a fresh session to start a link and to unlink | `oauth.Plugin.BeginLink`, `oauth.Plugin.Unlink`, and `oauth.Plugin.Complete` for a link |
| `oauth_token:read` | self | `oauth.Plugin.AccessToken` |
| `two_factor:read` | self | `twofactor.Plugin.Status` |
| `two_factor:manage` | self, with a fresh session | `twofactor.Plugin.Enroll`, `Confirm`, `Disable`, and `RegenerateRecoveryCodes` |
| `user:set_password` | admin | `emailpassword.Plugin.SetPassword` |
| `user:set_username` | admin | `username.Plugin.SetUsername` |
| `user:set_phone` | admin | `phonenumber.Plugin.SetPhoneNumber` |
| `user:add_phone` | admin | `phonenumber.Plugin.AddNumber` |
| `user:reset_two_factor` | admin | `twofactor.Plugin.Reset` |

Each package exports its permissions as constants, such as `emailpassword.PermissionSignOut` and `core.PermissionSessionTokenIssue`.

## Require a permission in a plugin

A plugin declares the permissions its flows require in `Declare`, and its flows check them through `AuthContext.Access`:

```go
const PermissionInvite core.Permission = "team:invite"

func (p *Plugin) Declare(ic *core.PluginInitContext) error {
	return ic.Permissions.Declare(core.PermissionDef{
		Name: PermissionInvite, Kind: core.PermissionSelf,
		Description: "Invite people to your team",
	})
}

func (p *Plugin) Invite(ctx context.Context, email string) error {
	userID, err := p.ac.Access.RequireUser(ctx, PermissionInvite)
	if err != nil {
		return err
	}
	// The inviting user is userID, from the caller and not from the input.
	return p.invite(ctx, userID, email)
}
```

The catalog enforces these rules:

- **A name is `resource:action`** in lowercase snake case, such as `team:invite`.
- **A name has one owner.** Declaring a name another plugin declared fails `Prepare`.
- **Kind and description are required.** The kind is `core.PermissionSelf` for the caller's own account or `core.PermissionAdmin` for an account named in the input.
- **The catalog is frozen after `Prepare`.**

`Access` has one method per requirement:

| Method | Use it for | Returns |
| --- | --- | --- |
| `RequireUser(ctx, p)` | a self-service flow | the id of the user the caller acts for |
| `RequireFreshUser(ctx, p)` | a self-service flow that changes something sensitive | the same, and refuses a session older than `SessionConfig.FreshAge` with `session_not_fresh`. A system caller acting for a user passes. |
| `OptionalUser(ctx, p)` | a flow that acts for a user when there is one and runs as a public flow when there is none | the user's id and true, or false for an anonymous caller that presented no credential |
| `RequireAdmin(ctx, p)` | an admin operation | nil for a caller that holds the admin permission |

Each method checks that `p` was declared with the kind it takes. An undeclared permission, or one of the other kind, is a configuration error on the first call.
