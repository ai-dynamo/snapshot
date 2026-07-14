module github.com/ai-dynamo/snapshot/operator

go 1.26

require (
	github.com/ai-dynamo/snapshot/api v0.0.0
	k8s.io/apimachinery v0.31.0
	k8s.io/client-go v0.31.0
	sigs.k8s.io/controller-runtime v0.19.0
)

replace github.com/ai-dynamo/snapshot/api => ../api
