module cpa-devin-plugin

go 1.26.0

require (
	connectrpc.com/connect v1.20.0
	github.com/google/uuid v1.6.0
	github.com/router-for-me/CLIProxyAPI/v7 v7.0.0-00010101000000-000000000000
	google.golang.org/protobuf v1.36.11
	gopkg.in/yaml.v3 v3.0.1
)

replace github.com/router-for-me/CLIProxyAPI/v7 => ../CLIProxyAPI
