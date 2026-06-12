module github.com/latebit-io/demarkus/server

go 1.26.0

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/fsnotify/fsnotify v1.10.1
	github.com/latebit-io/demarkus/protocol v0.0.0
	github.com/quic-go/quic-go v0.59.0
	golang.org/x/time v0.14.0
)

require (
	github.com/kr/text v0.2.0 // indirect
	golang.org/x/crypto v0.47.0 // indirect
	golang.org/x/net v0.48.0 // indirect
	golang.org/x/sys v0.40.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/latebit-io/demarkus/protocol => ../protocol
