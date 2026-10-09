module github.com/rossbrigoli/skquad/tool-gateway

go 1.26.8
toolchain go1.26.9

require (
	github.com/rossbrigoli/skquad/shared v0.0.0
	github.com/stretchr/testify v1.11.1
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/rossbrigoli/skquad/shared => ../shared
