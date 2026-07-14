module github.com/ai-dynamo/snapshot/agent

go 1.26

require (
	github.com/ai-dynamo/snapshot/api v0.0.0
	google.golang.org/grpc v1.65.0
	k8s.io/apimachinery v0.31.0
	k8s.io/client-go v0.31.0
)

replace github.com/ai-dynamo/snapshot/api => ../api
