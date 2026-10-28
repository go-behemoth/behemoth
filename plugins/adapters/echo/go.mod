module github.com/go-behemoth/behemoth/plugins/adapters/echo

go 1.26.0

require (
	github.com/go-behemoth/behemoth v0.2.0
	github.com/labstack/echo/v5 v5.4.2
)

require (
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
)

replace github.com/go-behemoth/behemoth => ../../../
