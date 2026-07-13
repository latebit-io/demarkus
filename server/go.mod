module github.com/latebit-io/demarkus/server

go 1.26.0

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/fsnotify/fsnotify v1.10.1
	github.com/jackc/pgerrcode v0.0.0-20250907135507-afb5586c32a6
	github.com/jackc/pgx/v5 v5.10.0
	github.com/latebit-io/demarkus/protocol v0.0.0
	github.com/quic-go/quic-go v0.59.0
	golang.org/x/time v0.14.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/kr/text v0.2.0 // indirect
	golang.org/x/crypto v0.47.0 // indirect
	golang.org/x/net v0.48.0 // indirect
	golang.org/x/sync v0.19.0 // indirect
	golang.org/x/sys v0.40.0 // indirect
	golang.org/x/text v0.33.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/latebit-io/demarkus/protocol => ../protocol
