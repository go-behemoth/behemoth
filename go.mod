module github.com/go-behemoth/behemoth

go 1.26.0

require (
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/google/uuid v1.6.0
	github.com/stretchr/testify v1.12.1
	golang.org/x/crypto v0.58.0
	golang.org/x/sync v0.24.0
)

require golang.org/x/sys v0.49.0 // indirect

require go.yaml.in/yaml/v3 v3.0.5 // indirect

// These versions declare the old module path, github.com/MastewalB/behemoth, and don't build at this one.
retract [v0.1.0, v0.1.3]
