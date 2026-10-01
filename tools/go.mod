module github.com/latebit-io/demarkus/tools

go 1.26.0

replace github.com/latebit-io/demarkus/protocol => ../protocol

replace github.com/latebit-io/demarkus/client => ../client

require (
	github.com/latebit-io/demarkus/client v0.0.0-00010101000000-000000000000
	github.com/latebit-io/demarkus/protocol v0.0.0
	github.com/yuin/goldmark v1.7.17
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/google/jsonschema-go v0.4.2 // indirect
	github.com/mark3labs/mcp-go v1.1.1 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/quic-go/quic-go v0.59.1 // indirect
	github.com/spf13/cast v1.7.1 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
)
