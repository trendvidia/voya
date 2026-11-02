// The generated bindings are their own module (#36) because they are the one
// thing the root module and client/ both need: the daemon serves these
// schemas, and every fleet service's client speaks them. With proto in the
// root module, importing client meant requiring the daemon's whole go.mod —
// chameleon included. As a leaf,
// this module's requirements are what generated code needs and nothing more.
//
// buf's tree stays whole: proto/buf.yaml, the schemas and the bindings all
// live under this one directory, so `make proto-check` and `proto-breaking`
// are untouched by the module boundary around them.
module github.com/trendvidia/voya/proto

go 1.26.10

require (
	connectrpc.com/connect v1.20.0
	google.golang.org/protobuf v1.36.12
)
