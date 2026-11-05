module github.com/latebit-io/demarkus/tools

go 1.27.0

toolchain go1.27.3

replace github.com/latebit-io/demarkus/protocol => ../protocol

replace github.com/latebit-io/demarkus/client => ../client

require (
	github.com/latebit-io/demarkus/client v0.0.0-00010101000000-000000000000
	github.com/latebit-io/demarkus/protocol v0.0.0
	github.com/mark3labs/mcp-go v1.5.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/google/jsonschema-go v0.4.2 // indirect
	github.com/quic-go/quic-go v0.65.0 // indirect
	github.com/spf13/cast v1.7.1 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	github.com/yuin/goldmark v1.8.7 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
)
