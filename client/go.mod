module github.com/latebit/demarkus/client

go 1.26

require (
	github.com/latebit/demarkus/protocol v0.0.0
	github.com/quic-go/quic-go v0.59.0
)

require (
	github.com/kr/text v0.2.0 // indirect
	golang.org/x/crypto v0.41.0 // indirect
	golang.org/x/net v0.43.0 // indirect
	golang.org/x/sys v0.35.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/latebit/demarkus/protocol => ../protocol
