module github.com/ai-dynamo/snapshot/snapshotctl

go 1.26

require (
	github.com/ai-dynamo/snapshot/api v0.0.0
	k8s.io/apimachinery v0.31.0
	k8s.io/client-go v0.31.0
)

replace github.com/ai-dynamo/snapshot/api => ../api
