module github.com/snowmerak/llm-provider/experiments/siwc-probe

go 1.26.5

require (
	github.com/coreos/go-oidc/v3 v3.21.0
	github.com/snowmerak/llm-provider v0.0.0
)

require (
	github.com/go-jose/go-jose/v4 v4.1.4 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
)

replace github.com/snowmerak/llm-provider => ../..
