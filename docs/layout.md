# Repository layout

```mermaid
graph TD
    root["snapshot/ · go.work"]
    root --> api["api/ · v1alpha1<br/>PodSnapshot, PodSnapshotContent"]
    root --> agent["agent/ · node DaemonSet<br/>cmd: agent, nsrestore"]
    root --> operator["operator/ · controller manager"]
    root --> snapshotctl["snapshotctl/ · CLI"]
    root --> charts["charts/snapshot · Helm chart"]
    root --> hack["hack/ · tools.mk, boilerplate"]
    root --> gh[".github/workflows · ci, push-artifacts"]

    operator -->|imports| api
    charts -. installs .-> operator
    charts -. installs .-> agent
    charts -. bundles .-> api
```

Each directory under the root is its own Go module, coordinated by `go.work`. `operator` is
the only module that imports `api`; `agent` and `snapshotctl` are standalone. The Helm chart
deploys the operator and agent and ships the CRDs generated from `api`.
