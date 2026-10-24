// Package httpapi serves Behemoth over HTTP: the router and its request
// pipeline, the request context a handler receives, the token carrier that
// moves the session token, the origin check, the route rate-limit rules,
// and the routes of core's own features, the session token exchange and the
// key set. A handler decodes a request, calls one flow of a plugin or of
// core, and encodes the result; the rules and protections live in the
// flow, so that another transport can call it with the same guarantees.
// FrameworkDriver connects the routes to an HTTP framework.
package httpapi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/core"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/telemetry"
)

// RouterConfig configures the HTTP side of behemoth: where the routes are
// mounted, how errors are answered, which origins and proxies are trusted,
// and how the session token travels. It is BootConfig.Router.
type RouterConfig struct {
	BasePath    string      // default "/api/auth"
	ErrorMapper ErrorMapper // default DefaultErrorMapper{}
	// BaseURL is the origin a browser reaches behemoth at, such as
	// "https://auth.example.com": a scheme, a host and an optional port,
	// without BasePath. The origin check (OriginCheck) accepts a
	// state-changing request from it. Boot fails on a value that is not an
	// origin. Empty = the check compares a request's Origin with its Host
	// header instead, which fails behind a proxy that rewrites Host; set
	// it in production.
	BaseURL string
	// TrustedOrigins are the origins a browser may be redirected to, such as
	// "https://app.example.com": a scheme, a host and an optional port,
	// matched exactly. Boot builds AuthContext.Origins from them and fails
	// on an entry that is not an origin. The origin check also accepts a
	// state-changing request from one of them, so list a front end that is
	// served from another origin than behemoth. Empty = no origin is
	// trusted: a redirect may only be a path on the application's own
	// site, and only behemoth's own origin may send such a request.
	TrustedOrigins []string

	// TrustedProxies are the CIDR blocks (load balancers, reverse proxies)
	// allowed to set ClientIPHeader. Empty = the direct peer is the client.
	TrustedProxies []string
	ClientIPHeader string // default "X-Forwarded-For"

	// RequestIDHeader is the header a request's ID is read from and echoed
	// in. A request without a usable value gets a generated ID. Default
	// "X-Request-ID".
	RequestIDHeader string

	// Carrier is where the session token travels over HTTP, and the
	// attributes of the session cookie. The router builds its TokenCarrier
	// from it and hands it to every handler as RequestContext.TokenCarrier.
	// The zero value is an HttpOnly, Secure, SameSite=Lax cookie named
	// DefaultSessionCookieName.
	Carrier TokenCarrierConfig
}

// Router is behemoth's route table. Boot mounts the plugins' routes and
// core's own, applies authentication and the route rate limits, and Build
// wraps every route in the request pipeline and hands the table to a
// FrameworkDriver.
type Router struct {
	basePath        string
	routes          []mountedRoute
	errorMapper     ErrorMapper
	routeOwners     map[routeKey]string
	auth            *core.AuthContext
	carrier         *TokenCarrier
	requestIDHeader string
	log             telemetry.Logger
	tel             *telemetry.Telemetry // for request metrics; never nil
}

// The route method and full path must be unique (eg. "GET" - /api/auth/health )
type routeKey struct{ method, path string }

// mountedRoute is a route in the table plus the path route rate-limit rules
// are matched against. Rules are declared relative to BasePath (core cannot
// know the developer's BasePath), so a mounted route matches on its path
// below BasePath — MountPath + Route.Path — while an absolute route, which
// lives outside BasePath, matches on its full path.
type mountedRoute struct {
	Route
	matchPath string
}

// NewRouter returns an empty route table. auth is handed to every handler
// as RequestContext.Auth, and the carrier of cfg.Carrier as
// RequestContext.TokenCarrier, so adapters never need to know about either.
// cfg.Carrier is not validated here: Boot calls its Validate first.
func NewRouter(cfg RouterConfig, auth *core.AuthContext) *Router {
	if cfg.BasePath == "" {
		cfg.BasePath = "/api/auth"
	}
	if cfg.ErrorMapper == nil {
		cfg.ErrorMapper = &DefaultErrorMapper{}
	}
	if cfg.RequestIDHeader == "" {
		cfg.RequestIDHeader = telemetry.DefaultRequestIDHeader
	}
	var tel *telemetry.Telemetry
	if auth != nil {
		tel = auth.Telemetry
	}
	tel = telemetry.OrDefault(tel).Named("router")
	return &Router{
		log:             tel.Logger,
		tel:             tel,
		basePath:        cfg.BasePath,
		errorMapper:     cfg.ErrorMapper,
		routeOwners:     map[routeKey]string{},
		auth:            auth,
		carrier:         NewTokenCarrier(cfg.Carrier),
		requestIDHeader: cfg.RequestIDHeader,
	}
}

// TokenCarrier returns the carrier the router hands to every handler, built
// from RouterConfig.Carrier with its defaults applied.
func (r *Router) TokenCarrier() *TokenCarrier { return r.carrier }

// Mount registers a plugin's endpoints under basePath + pluginMountPath +
// endpoint.Path, wrapping each in its declared Middlewares (Middlewares[0]
// executes first). Conflicts are reported for every endpoint at once.
func (r *Router) Mount(owner, pluginMountPath string, endpoints []Route) error {
	var duplicateErr []string
	for _, ep := range endpoints {
		fullPath := path.Join(r.basePath, pluginMountPath, ep.Path)
		if err := r.claim(owner, ep.Method, fullPath); err != "" {
			duplicateErr = append(duplicateErr, err)
			continue
		}

		h := ep.Handler
		for i := len(ep.Middlewares) - 1; i >= 0; i-- {
			h = ep.Middlewares[i](h)
		}
		r.routes = append(r.routes, mountedRoute{
			Route:     Route{Method: ep.Method, Path: fullPath, Handler: h},
			matchPath: path.Join("/", pluginMountPath, ep.Path),
		})
	}

	if len(duplicateErr) > 0 {
		return errs.NewConfigurationError("Router.Mount",
			fmt.Sprintf("route conflicts detected: \n  %s", strings.Join(duplicateErr, "\n  ")),
			nil,
		)
	}
	return nil
}

// MountAbsolute registers routes at their exact Path, bypassing basePath and
// plugin-mount joining, for standards-fixed routes (like JWKS) that
// must not live under the developer's configured auth API prefix.
func (r *Router) MountAbsolute(owner string, routes ...Route) error {
	var duplicateErr []string
	for _, rt := range routes {
		if err := r.claim(owner, rt.Method, rt.Path); err != "" {
			duplicateErr = append(duplicateErr, err)
			continue
		}
		r.routes = append(r.routes, mountedRoute{
			Route:     Route{Method: rt.Method, Path: rt.Path, Handler: rt.Handler},
			matchPath: rt.Path,
		})
	}
	if len(duplicateErr) > 0 {
		return errs.NewConfigurationError("Router.MountAbsolute",
			fmt.Sprintf("route conflicts detected: \n  %s", strings.Join(duplicateErr, "\n  ")),
			nil,
		)
	}
	return nil
}

// claim records owner for method+fullPath, or describes the conflict.
func (r *Router) claim(owner, method, fullPath string) string {
	key := routeKey{method: method, path: fullPath}
	if existingOwner, dup := r.routeOwners[key]; dup {
		return fmt.Sprintf("%s %s: claimed by both %q and %q", method, fullPath, existingOwner, owner)
	}
	r.routeOwners[key] = owner
	return ""
}

// Routes returns the resolved route table, for inspection and tests.
func (r *Router) Routes() []Route {
	routes := make([]Route, len(r.routes))
	for i, rt := range r.routes {
		routes[i] = rt.Route
	}
	return routes
}

// wrapWithErrorMapping turns an error returned by route's chain into a
// response, and logs it. This is the one place a request's failure is
// logged, so components below return their errors without logging them. A
// 5xx is logged at Error: behemoth failed at something it owed. An
// unavailable error, such as a busy password hasher, is logged at Warn: the
// process refused work it had no room for, and a burst of these must not
// bury real failures. Anything else is a rejection of the request (a wrong
// password, a rate limit) and is logged at Debug.
func (r *Router) wrapWithErrorMapping(route Route, next HandlerFunc) HandlerFunc {
	return func(rctx *RequestContext) error {
		if err := next(rctx); err != nil {
			status, body := r.errorMapper.Map(err)
			fields := telemetry.ErrorFields(err, behemoth.M{
				telemetry.FieldMethod: route.Method,
				telemetry.FieldRoute:  route.Path,
				telemetry.FieldStatus: status,
			})
			switch {
			case errs.IsCategory(err, errs.CategoryUnavailable):
				r.log.Warn(rctx.Ctx, "request refused: no capacity", fields)
			case status >= 500:
				telemetry.SpanFrom(rctx.Ctx).RecordError(err)
				r.log.Error(rctx.Ctx, "request failed", fields)
			default:
				r.log.Debug(rctx.Ctx, "request rejected", fields)
			}
			if de, ok := errors.AsType[*errs.DomainError](err); ok && de.RetryAfter > 0 {
				// Rounded up: a client that waits the truncated number of
				// seconds would come back before the window has ended.
				rctx.Response.Headers.Set("Retry-After", strconv.Itoa(int(math.Ceil(de.RetryAfter.Seconds()))))
			}
			return rctx.Response.JSON(status, body)
		}
		return nil
	}
}

// withTracing runs route inside a span, the parent of everything behemoth
// does for the request. It is started from the request's context, so it is
// a child of the span the application's HTTP middleware started, if any:
// behemoth reads no trace headers itself.
//
// The span is not marked as failed here. Error mapping, further in, knows
// the error and whether it is the server's (see wrapWithErrorMapping).
func (r *Router) withTracing(route Route, next HandlerFunc) HandlerFunc {
	if !r.tel.TracingEnabled() {
		return next
	}
	return func(rctx *RequestContext) error {
		ctx, span := r.tel.StartSpan(rctx.Ctx, telemetry.SpanRequest, behemoth.M{
			telemetry.AttrMethod:    route.Method,
			telemetry.AttrRoute:     route.Path,
			telemetry.AttrRequestID: telemetry.RequestIDFrom(rctx.Ctx),
		})
		rctx.Ctx = ctx
		err := next(rctx)
		status := http.StatusInternalServerError
		if err == nil && rctx.Response != nil {
			status = rctx.Response.Code
		}
		span.SetAttributes(behemoth.M{telemetry.AttrStatus: status})
		span.RecordError(err) // nil unless the response could not be built
		span.End()
		return err
	}
}

// withMetrics counts each request to route and measures how long it took.
// It sits outside error mapping, so the status it reports is the one the
// client gets, mapped errors included. The route attribute is the route's
// pattern, which keeps the number of series fixed by the route table.
func (r *Router) withMetrics(route Route, next HandlerFunc) HandlerFunc {
	if !r.tel.MetricsEnabled() {
		return next
	}
	return func(rctx *RequestContext) error {
		start := time.Now()
		err := next(rctx)
		status := http.StatusInternalServerError // the adapter's answer when the response could not be built
		if err == nil && rctx.Response != nil {
			status = rctx.Response.Code
		}
		attrs := behemoth.M{telemetry.AttrMethod: route.Method, telemetry.AttrRoute: route.Path, telemetry.AttrStatus: status}
		r.tel.Count(rctx.Ctx, telemetry.MetricHTTPRequests, attrs)
		r.tel.ObserveSince(rctx.Ctx, telemetry.MetricHTTPDuration, start, attrs)
		return err
	}
}

// withRequestScope prepares every request before anything else sees it: it
// sets rctx.Auth and rctx.TokenCarrier, and puts rctx on rctx.Ctx
// (ContextWithRequest) so code that only receives a context.Context, such
// as a hook handler, still knows the request. Adapters always set Ctx; the fallbacks keep a bare RequestContext
// (tests, custom drivers) safe.
//
// It also gives the request its ID (see requestID), puts it on rctx.Ctx for
// logs and audit events, and sets it on the response header so a client can
// quote it when reporting a problem.
func (r *Router) withRequestScope(next HandlerFunc) HandlerFunc {
	return func(rctx *RequestContext) error {
		rctx.Auth = r.auth
		rctx.TokenCarrier = r.carrier
		ctx := rctx.Ctx
		if ctx == nil && rctx.Request != nil {
			ctx = rctx.Request.Context()
		}
		if ctx == nil {
			ctx = context.Background()
		}
		id := r.requestID(ctx, rctx)
		ctx = telemetry.ContextWithRequestID(ctx, id)
		if rctx.Response != nil {
			rctx.Response.Headers.Set(r.requestIDHeader, id)
		}
		rctx.Ctx = ContextWithRequest(ctx, rctx)
		return next(rctx)
	}
}

// requestID picks the ID for a request: the one already on ctx (set by the
// application's own middleware), else the request header's value when it is
// usable (telemetry.ValidRequestID), else a generated one.
func (r *Router) requestID(ctx context.Context, rctx *RequestContext) string {
	if id := telemetry.RequestIDFrom(ctx); id != "" {
		return id
	}
	if rctx.Request != nil {
		if id := rctx.Request.Header.Get(r.requestIDHeader); telemetry.ValidRequestID(id) {
			return id
		}
	}
	return telemetry.NewRequestID()
}

// ApplyRateLimiting resolves, for each mounted route, its single most-specific matching RouteRateLimitRule once,
// and wraps that route's handler accordingly. Rules are matched against a
// mounted route's path below BasePath, and an absolute route's full path
// (see mountedRoute). rules are the route rules Prepare declared
// (DeclareRouteRules), whose counting part rl knows by name.
// It should run after every Mount/MountAbsolute call,
// ideally making it the last step before Build hands routes to a FrameworkDriver
func (r *Router) ApplyRateLimiting(rl core.RateLimiter, rules []RouteRateLimitRule, ipCfg *ClientIPConfig) {
	for i, rt := range r.routes {
		rule, ok := BestRouteMatch(rules, rt.Method, rt.matchPath)
		if !ok || rule.Disabled {
			continue
		}
		r.routes[i].Handler = rateLimitWrap(rt.Handler, rule, rl, ipCfg)
	}
}

// Build wraps every route in the request pipeline and hands the table to
// driver. From outermost to innermost, a request passes through:
//
//	request scope (Auth, request on Ctx) -> tracing -> metrics -> error mapping -> origin check -> global middlewares -> route rate limit -> route middlewares -> handler
//
// Error mapping sits outside everything that can fail, so an error from a
// middleware or the rate limiter becomes a response the same way a handler
// error does. Global middlewares wrap only behemoth's routes, never the rest
// of the application sharing the framework instance. The origin check is the
// first of them: Boot passes OriginCheck.Middleware ahead of the plugins'
// when the session token travels in a cookie.
func (r *Router) Build(driver FrameworkDriver, globalMiddleware ...Middleware) error {
	routes := make([]Route, len(r.routes))
	for i, mr := range r.routes {
		rt := mr.Route
		h := rt.Handler
		for j := len(globalMiddleware) - 1; j >= 0; j-- {
			h = globalMiddleware[j](h)
		}
		rt.Handler = r.withRequestScope(r.withTracing(rt, r.withMetrics(rt, r.wrapWithErrorMapping(rt, h))))
		routes[i] = rt
	}
	return driver.Mount(routes...)
}

// FrameworkDriver is the only seam between behemoth's plugin system and a
// concrete HTTP framework. Each framework (gin, echo, chi, fiber, ...) ships
// its own thin implementation, built around the application's own framework
// instance. Behemoth never imports gin or echo directly. The driver for the
// standard library's http.ServeMux needs no such import, so it is the one
// adapter in this module (plugins/adapters/nethttp).
//
// Mount tells the framework to mount the given Routes.
// How it does that is entirely up to the adapter:
//   - gin      - router.Handle(route.Method, route.Path, ginHandler)
//   - echo     - e.Add(route.Method, route.Path, echoHandler)
//   - chi      - r.Method(route.Method, route.Path, httpHandler)
//   - net/http - mux.HandleFunc(route.Method+" "+route.Path, httpHandler)
//   - fiber    - app.Add([]string{route.Method}, route.Path, fiberHandler)
//
// Inside every adapter's handler, the pattern is always the same:
//  1. Build a *RequestContext from the framework's native request. A
//     framework that is not built on net/http (fiber) has to build the
//     *http.Request too.
//  2. Call route.Handler(rc). Routes arrive fully wrapped: Auth is set and
//     errors are already mapped to responses, so a returned error means the
//     response itself could not be written.
//  3. Call rc.Response.Flush(underlying ResponseWriter).
type FrameworkDriver interface {
	// Mount registers handlers for every route. It returns an error, rather
	// than panicking, when the framework rejects a route (e.g. it conflicts
	// with one the application registered itself).
	Mount(routes ...Route) error
}

// rateLimitWrap counts each request to next against rule, per what rule's
// KeyFunc returns for it. A refusal is an *errs.DomainError of
// CategoryRateLimited, which error mapping answers with 429 and
// Retry-After before the body is read or a session looked up.
func rateLimitWrap(next HandlerFunc, rule RouteRateLimitRule, rl core.RateLimiter, ipCfg *ClientIPConfig) HandlerFunc {
	return func(rctx *RequestContext) error {
		ip := ClientIP(rctx.Request, ipCfg)
		if err := rl.CheckRule(rctx.Ctx, rule.Name, rule.KeyFunc(rctx.Request, ip), ip); err != nil {
			return err
		}
		return next(rctx)
	}
}
