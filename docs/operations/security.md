# Security

Snapshot's node agent needs elevated privileges to checkpoint and restore
processes. This page describes what it requires and why.

## Privileged node agent

The `snapshot-agent` runs as a privileged DaemonSet with `hostPID`, `hostIPC`, and
`hostNetwork` so it can invoke CRIU and `cuda-checkpoint` against live processes on
the node. Workloads do not need to be privileged — only the agent does.

Because of this, the agent's namespace must permit privileged pods. On clusters
that enforce [Pod Security Standards](https://kubernetes.io/docs/concepts/security/pod-security-standards/),
apply the `privileged` level (or an equivalent exception) to that namespace.

## Seccomp

CRIU requires a seccomp profile to perform checkpoint and restore. The Helm chart
installs the profile the agent needs.
