module github.com/latebit/demarkus/tools

go 1.26

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/latebit/demarkus/protocol v0.0.0
)

require gopkg.in/yaml.v3 v3.0.1 // indirect

replace github.com/latebit/demarkus/protocol => ../protocol
