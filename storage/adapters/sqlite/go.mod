module github.com/go-behemoth/behemoth/storage/adapters/sqlite

go 1.26.0

require (
	github.com/go-behemoth/behemoth v0.2.0
	github.com/mattn/go-sqlite3 v1.14.52
)

require github.com/google/uuid v1.6.0 // indirect

replace github.com/go-behemoth/behemoth => ../../../
