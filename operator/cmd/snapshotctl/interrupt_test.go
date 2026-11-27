//go:build unix

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"

	nvidiacomv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// The tests in this file run "snapshotctl checkpoint" in a child process: this test binary,
// started again, calls the real main against a stand-in for the Kubernetes API that it serves
// itself. The signals are real, and the parent reads how the process ended. Only a process of
// its own can show that the command ends by the signal.
const (
	interruptChildEnv    = "SNAPSHOT_TEST_INTERRUPT"
	interruptChildDirEnv = "SNAPSHOT_TEST_INTERRUPT_DIR"
)

// The calls of a checkpoint, as the stand-in for the API names them on the child's stdout.
const (
	callCreateJob   = "create-job"
	callListPods    = "list-pods"
	callGetSnapshot = "get-podsnapshot"
	callDeleteJob   = "delete-job"
	// callCompleted is not a call: it is the moment after the command printed status=completed.
	callCompleted = "completed"
)

// interruptBehavior is what the stand-in for the Kubernetes API does in one child process.
type interruptBehavior struct {
	snapshot     string // the PodSnapshot's state: "ready", "failed", or "" for one that stays pending
	deleteStatus int    // the answer to the Job delete: 0 deletes the Job
	signalAt     string // the call at which the child sends itself signal
	signal       syscall.Signal
	secondAt     string // the call at which the child sends itself second, after the time in which it would count as a repeat
	second       syscall.Signal
	repeat       syscall.Signal // a signal that the child sends itself with no gap after signal
	holdAt       string         // the call that gets its answer only after the parent releases it
}

var interruptBehaviors = map[string]interruptBehavior{
	"wait":                   {signalAt: callListPods, signal: syscall.SIGINT},
	"create-held":            {signalAt: callCreateJob, signal: syscall.SIGTERM, holdAt: callCreateJob},
	"manifest-pipe":          {},
	"failed-cleanup":         {snapshot: "failed", signalAt: callDeleteJob, signal: syscall.SIGINT, holdAt: callDeleteJob},
	"delete-refused":         {signalAt: callListPods, signal: syscall.SIGINT, deleteStatus: http.StatusForbidden},
	"second-signal":          {signalAt: callListPods, signal: syscall.SIGINT, deleteStatus: http.StatusServiceUnavailable, secondAt: callDeleteJob, second: syscall.SIGTERM},
	"two-signals-one-action": {signalAt: callListPods, signal: syscall.SIGINT, repeat: syscall.SIGTERM},
	"completed":              {snapshot: "ready", signalAt: callCompleted, signal: syscall.SIGTERM},
	"sigint-ignored":         {snapshot: "ready", signalAt: callListPods, signal: syscall.SIGINT},
	"pending":                {},
	"failed-no-signal":       {snapshot: "failed"},
}

// apiStandIn stands in for the Kubernetes API in the child process, over HTTP.
type apiStandIn struct {
	interruptBehavior
	out      io.Writer
	released chan struct{}
	once     map[string]*sync.Once
	mu       sync.Mutex
	snap     *nvidiacomv1alpha1.PodSnapshot
}

func sendSelf(sig syscall.Signal) {
	if err := syscall.Kill(os.Getpid(), sig); err != nil {
		panic(err)
	}
}

// call announces a call on the child's stdout, and sends the signals and holds the answer as the
// case says, the first time the call arrives.
func (a *apiStandIn) call(name, detail string) {
	_, _ = fmt.Fprintln(a.out, strings.TrimSpace("api: "+name+" "+detail))
	a.once[name].Do(func() {
		if a.signalAt == name {
			sendSelf(a.signal)
			if a.repeat != 0 {
				sendSelf(a.repeat)
			}
		}
		if a.secondAt == name {
			time.Sleep(interruptRepeatWindow + 50*time.Millisecond)
			sendSelf(a.second)
		}
		if a.holdAt == name {
			<-a.released
		}
	})
}

func (a *apiStandIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	answer := func(code int, obj any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(obj)
	}
	status := func(code int, reason metav1.StatusReason) {
		answer(code, &metav1.Status{
			TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Status: metav1.StatusFailure,
			Reason: reason, Code: int32(code), Message: r.Method + " " + r.URL.Path + ": " + string(reason),
		})
	}
	// The clientset sends protobuf, the controller-runtime client JSON: decode reads both.
	decode := func(into runtime.Object) {
		body, _ := io.ReadAll(r.Body)
		_, _, _ = scheme.Codecs.UniversalDeserializer().Decode(body, nil, into)
	}
	path := r.URL.Path
	group := metav1.GroupVersionForDiscovery{GroupVersion: nvidiacomv1alpha1.GroupVersion.String(), Version: nvidiacomv1alpha1.GroupVersion.Version}
	switch {
	case r.Method == http.MethodGet && path == "/api":
		answer(http.StatusOK, &metav1.APIVersions{TypeMeta: metav1.TypeMeta{Kind: "APIVersions"}, Versions: []string{"v1"}})
	case r.Method == http.MethodGet && path == "/apis":
		answer(http.StatusOK, &metav1.APIGroupList{
			TypeMeta: metav1.TypeMeta{Kind: "APIGroupList", APIVersion: "v1"},
			Groups: []metav1.APIGroup{{
				Name: nvidiacomv1alpha1.GroupVersion.Group, Versions: []metav1.GroupVersionForDiscovery{group}, PreferredVersion: group,
			}},
		})
	case r.Method == http.MethodGet && path == "/apis/nvidia.com/v1alpha1":
		answer(http.StatusOK, &metav1.APIResourceList{
			TypeMeta:     metav1.TypeMeta{Kind: "APIResourceList", APIVersion: "v1"},
			GroupVersion: nvidiacomv1alpha1.GroupVersion.String(),
			APIResources: []metav1.APIResource{{Name: "podsnapshots", Namespaced: true, Kind: nvidiacomv1alpha1.KindPodSnapshot}},
		})
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/jobs"):
		job := &batchv1.Job{}
		decode(job)
		job.UID = "job-uid"
		a.call(callCreateJob, "name="+job.Name)
		answer(http.StatusCreated, job)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/pods"):
		a.call(callListPods, "")
		answer(http.StatusOK, &corev1.PodList{
			TypeMeta: metav1.TypeMeta{Kind: "PodList", APIVersion: "v1"},
			Items: []corev1.Pod{{
				ObjectMeta: metav1.ObjectMeta{
					Name: "source-pod", Namespace: "default", UID: "pod-uid",
					OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: "job", UID: "job-uid", Controller: ptr.To(true)}},
				},
				Spec: corev1.PodSpec{NodeName: "node-a"},
			}},
		})
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/podsnapshots"):
		snap := &nvidiacomv1alpha1.PodSnapshot{}
		_ = json.NewDecoder(r.Body).Decode(snap)
		snap.UID = "snapshot-uid"
		a.mu.Lock()
		a.snap = snap
		a.mu.Unlock()
		answer(http.StatusCreated, snap)
	case r.Method == http.MethodGet && strings.Contains(path, "/podsnapshots/"):
		a.call(callGetSnapshot, "")
		a.mu.Lock()
		snap := a.snap.DeepCopy()
		a.mu.Unlock()
		switch a.snapshot {
		case "ready":
			meta.SetStatusCondition(&snap.Status.Conditions, metav1.Condition{Type: nvidiacomv1alpha1.PodSnapshotConditionReady, Status: metav1.ConditionTrue, Reason: "Captured"})
		case "failed":
			meta.SetStatusCondition(&snap.Status.Conditions, metav1.Condition{
				Type: nvidiacomv1alpha1.PodSnapshotConditionFailed, Status: metav1.ConditionTrue, Reason: "CheckpointFailed", Message: "the stand-in failed it",
			})
		}
		answer(http.StatusOK, snap)
	case r.Method == http.MethodDelete && strings.Contains(path, "/jobs/"):
		opts := &metav1.DeleteOptions{}
		decode(opts)
		a.call(callDeleteJob, "propagation="+string(ptr.Deref(opts.PropagationPolicy, "")))
		switch a.deleteStatus {
		case 0:
			answer(http.StatusOK, &metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Status: metav1.StatusSuccess})
		case http.StatusForbidden:
			status(a.deleteStatus, metav1.StatusReasonForbidden)
		default:
			status(a.deleteStatus, metav1.StatusReasonServiceUnavailable)
		}
	default:
		_, _ = fmt.Fprintln(a.out, "api: unexpected "+r.Method+" "+path)
		status(http.StatusNotFound, metav1.StatusReasonNotFound)
	}
}

const interruptManifest = `apiVersion: v1
kind: Pod
metadata:
  name: worker
spec:
  containers:
  - name: main
    image: registry.example/worker:1
`

// runInterruptChild is the child process: it serves the stand-in for the API, points a kubeconfig
// at it, and runs the real main. It does not return.
func runInterruptChild(name string) {
	behavior, ok := interruptBehaviors[name]
	if !ok {
		panic("unknown case " + name)
	}
	stdout := os.Stdout
	api := &apiStandIn{interruptBehavior: behavior, out: stdout, released: make(chan struct{}), once: map[string]*sync.Once{}}
	for _, call := range []string{callCreateJob, callListPods, callGetSnapshot, callDeleteJob} {
		api.once[call] = &sync.Once{}
	}
	go func() {
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		close(api.released)
	}()
	server := httptest.NewServer(api)

	dir := os.Getenv(interruptChildDirEnv)
	kubeconfig := filepath.Join(dir, "kubeconfig")
	config := fmt.Sprintf("apiVersion: v1\nkind: Config\ncurrent-context: test\nclusters:\n- name: test\n  cluster: {server: %q}\n"+
		"contexts:\n- name: test\n  context: {cluster: test, user: test, namespace: default}\nusers:\n- name: test\n  user: {}\n", server.URL)
	if err := os.WriteFile(kubeconfig, []byte(config), 0o600); err != nil {
		panic(err)
	}
	if err := os.Setenv("KUBECONFIG", kubeconfig); err != nil {
		panic(err)
	}
	manifest := filepath.Join(dir, "pod.yaml")
	if _, err := os.Lstat(manifest); err != nil { // the parent made it a named pipe in one case
		if err := os.WriteFile(manifest, []byte(interruptManifest), 0o600); err != nil {
			panic(err)
		}
	}
	os.Args = []string{"snapshotctl", "checkpoint", "--manifest", manifest, "--snapshot", "my-snapshot", "--container", "main", "--timeout", "1m"}

	if api.signalAt != callCompleted {
		main()
		os.Exit(0)
	}
	// The signal arrives after the command printed the first line of its result. The rest of the
	// result reaches the parent only after the signal had time to end the process, if it can.
	reader, writer, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	os.Stdout = writer
	forwarded := make(chan struct{})
	go func() {
		defer close(forwarded)
		lines := bufio.NewScanner(reader)
		for lines.Scan() {
			_, _ = fmt.Fprintln(stdout, lines.Text())
			if lines.Text() == "status=completed" {
				sendSelf(api.signal)
				time.Sleep(300 * time.Millisecond)
			}
		}
	}()
	main()
	_ = writer.Close()
	<-forwarded
	os.Exit(0)
}

// interruptChild is the parent's side of one child process.
type interruptChild struct {
	t      *testing.T
	cmd    *exec.Cmd
	dir    string
	stdin  io.WriteCloser
	errOut io.ReadCloser
	ended  chan struct{}

	mu     sync.Mutex
	stdout []string
	stderr []string
}

func startInterruptChild(t *testing.T, name string, prepare ...func(c *interruptChild)) *interruptChild {
	t.Helper()
	c := &interruptChild{t: t, dir: t.TempDir(), ended: make(chan struct{})}
	c.cmd = exec.Command(os.Args[0], "-test.run=^TestCheckpointInterrupt$")
	c.cmd.Env = append(os.Environ(), interruptChildEnv+"="+name, interruptChildDirEnv+"="+c.dir)
	for _, fn := range prepare {
		fn(c)
	}
	var err error
	c.stdin, err = c.cmd.StdinPipe()
	require.NoError(t, err)
	out, err := c.cmd.StdoutPipe()
	require.NoError(t, err)
	c.errOut, err = c.cmd.StderrPipe()
	require.NoError(t, err)
	// The child must start with the default action for SIGINT, also when this test binary was
	// started with SIGINT ignored. A signal that this process handles is at its default action
	// in a process that it starts.
	handled := make(chan os.Signal, 1)
	signal.Notify(handled, syscall.SIGINT)
	err = c.cmd.Start()
	signal.Stop(handled)
	require.NoError(t, err)

	var readers sync.WaitGroup
	read := func(from io.Reader, into *[]string) {
		defer readers.Done()
		lines := bufio.NewScanner(from)
		lines.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for lines.Scan() {
			c.mu.Lock()
			*into = append(*into, lines.Text())
			c.mu.Unlock()
		}
	}
	readers.Add(2)
	go read(out, &c.stdout)
	go read(c.errOut, &c.stderr)
	go func() {
		readers.Wait()
		_ = c.cmd.Wait()
		close(c.ended)
	}()
	t.Cleanup(func() {
		_ = c.cmd.Process.Kill()
		<-c.ended
	})
	return c
}

func (c *interruptChild) output(lines *[]string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(*lines, "\n")
}

// await waits until the child's output has the text, and fails the test when it does not come.
func (c *interruptChild) await(lines *[]string, text string) {
	c.t.Helper()
	require.Eventually(c.t, func() bool { return strings.Contains(c.output(lines), text) }, 20*time.Second, 2*time.Millisecond,
		"the child never printed %q", text)
}

// secondSignal sends the child a signal that is late enough to count as a second one.
func (c *interruptChild) secondSignal(sig syscall.Signal) {
	c.t.Helper()
	time.Sleep(interruptRepeatWindow + 50*time.Millisecond)
	require.NoError(c.t, c.cmd.Process.Signal(sig))
}

func (c *interruptChild) release() {
	_, err := io.WriteString(c.stdin, "\n")
	require.NoError(c.t, err)
}

// end waits for the process to end and returns how it ended.
func (c *interruptChild) end() syscall.WaitStatus {
	c.t.Helper()
	select {
	case <-c.ended:
	case <-time.After(20 * time.Second):
		c.t.Fatalf("the command did not end\nstdout:\n%s\nstderr:\n%s", c.output(&c.stdout), c.output(&c.stderr))
	}
	c.t.Logf("stdout:\n%s\nstderr:\n%s", c.output(&c.stdout), c.output(&c.stderr))
	return c.cmd.ProcessState.Sys().(syscall.WaitStatus)
}

func requireEndedBy(t *testing.T, status syscall.WaitStatus, sig syscall.Signal) {
	t.Helper()
	require.True(t, status.Signaled(), "the command must end by the signal, as a command with no handler does; it exited with status %d", status.ExitStatus())
	require.Equal(t, sig, status.Signal())
}

// jobName returns the name of the Job that the child's command created.
func (c *interruptChild) jobName() string {
	c.t.Helper()
	for _, line := range strings.Split(c.output(&c.stdout), "\n") {
		if name, ok := strings.CutPrefix(line, "api: "+callCreateJob+" name="); ok {
			return name
		}
	}
	c.t.Fatal("the command created no Job")
	return ""
}

const (
	logInterruptedJob    = "Interrupted, deleting the checkpoint Job and its source Pod; interrupt again to stop at once"
	logInterruptedCreate = "Interrupted while the checkpoint Job is created; the command deletes the Job when the create is answered"
	logDeleted           = "Deleted the checkpoint Job and its source Pod"
)

func TestCheckpointInterrupt(t *testing.T) {
	if name := os.Getenv(interruptChildEnv); name != "" {
		runInterruptChild(name)
	}

	t.Run("during the wait deletes the Job and ends by the signal", func(t *testing.T) {
		c := startInterruptChild(t, "wait")
		requireEndedBy(t, c.end(), syscall.SIGINT)

		stderr := c.output(&c.stderr)
		acknowledged := strings.Index(stderr, logInterruptedJob)
		deleted := strings.Index(stderr, logDeleted)
		failed := strings.Index(stderr, "snapshotctl failed")
		require.True(t, acknowledged >= 0 && deleted > acknowledged && failed > deleted,
			"the command must acknowledge the signal, then report the delete, then report the error")
		assert.Contains(t, stderr, `"signal": "interrupt"`)
		assert.Contains(t, stderr, `"job": "default/`+c.jobName()+`"`)
		assert.Contains(t, stderr, "checkpoint interrupted: interrupt signal received")
		assert.Contains(t, c.output(&c.stdout), "api: delete-job propagation=Background")
	})

	t.Run("while the Job create is in flight is acknowledged at once", func(t *testing.T) {
		c := startInterruptChild(t, "create-held")
		c.await(&c.stderr, logInterruptedCreate)
		assert.NotContains(t, c.output(&c.stdout), "api: delete-job", "the create has no answer yet")
		c.release()
		requireEndedBy(t, c.end(), syscall.SIGTERM)

		assert.Contains(t, c.output(&c.stdout), "api: delete-job propagation=Background", "the command must delete the Job once the create is answered")
		assert.Contains(t, c.output(&c.stderr), logDeleted)
		assert.Contains(t, c.output(&c.stderr), "checkpoint interrupted: terminated signal received")
	})

	t.Run("before the Job exists creates no Job", func(t *testing.T) {
		c := startInterruptChild(t, "manifest-pipe", func(c *interruptChild) {
			// The command blocks on the manifest: it has its signal handler and has sent nothing.
			require.NoError(t, syscall.Mkfifo(filepath.Join(c.dir, "pod.yaml"), 0o600))
		})
		c.await(&c.stderr, "Running checkpoint")
		require.NoError(t, c.cmd.Process.Signal(syscall.SIGINT))
		c.await(&c.stderr, "Interrupted, stopping; interrupt again to stop at once")
		require.NoError(t, os.WriteFile(filepath.Join(c.dir, "pod.yaml"), []byte(interruptManifest), 0o600))
		requireEndedBy(t, c.end(), syscall.SIGINT)

		assert.NotContains(t, c.output(&c.stdout), "api: create-job")
		assert.Contains(t, c.output(&c.stderr), "checkpoint interrupted: interrupt signal received")
	})

	t.Run("during the cleanup of a failed checkpoint keeps the checkpoint's error", func(t *testing.T) {
		c := startInterruptChild(t, "failed-cleanup")
		c.await(&c.stderr, logInterruptedJob)
		c.release()
		requireEndedBy(t, c.end(), syscall.SIGINT)

		stderr := c.output(&c.stderr)
		assert.Contains(t, stderr, logDeleted)
		assert.Contains(t, stderr, "PodSnapshot default/my-snapshot failed: CheckpointFailed")
		assert.NotContains(t, stderr, "checkpoint interrupted")
	})

	t.Run("with a delete that is refused names the Job and the command that deletes it", func(t *testing.T) {
		c := startInterruptChild(t, "delete-refused")
		requireEndedBy(t, c.end(), syscall.SIGINT)

		stderr := c.output(&c.stderr)
		assert.Contains(t, stderr, "checkpoint interrupted: interrupt signal received")
		assert.Contains(t, stderr, "kubectl delete job "+c.jobName()+" --namespace default")
		assert.NotContains(t, stderr, logDeleted)
	})

	t.Run("a second signal ends the command at once and says what it leaves", func(t *testing.T) {
		c := startInterruptChild(t, "second-signal")
		requireEndedBy(t, c.end(), syscall.SIGTERM)

		stderr := c.output(&c.stderr)
		assert.Contains(t, stderr, logInterruptedJob)
		assert.Contains(t, stderr, "Stopped by a second signal before the cleanup finished")
		assert.Contains(t, stderr, `"signal": "terminated"`)
		assert.Contains(t, stderr, "kubectl delete job "+c.jobName()+" --namespace default")
		assert.Equal(t, 1, strings.Count(c.output(&c.stdout), "api: delete-job"), "the command must not wait for the next attempt of the delete")
	})

	t.Run("two signals that arrive together count as one", func(t *testing.T) {
		// GNU timeout signals the command and then the command's process group.
		c := startInterruptChild(t, "two-signals-one-action")
		status := c.end()
		// The child sends itself SIGINT and then SIGTERM with no gap. Which of the two ends the
		// command is not fixed. Linux can deliver them on two threads, in either order. A signal
		// that is delivered only after the cleanup finds the default action back in place.
		require.True(t, status.Signaled(), "the command must end by a signal; it exited with status %d", status.ExitStatus())
		assert.Contains(t, []syscall.Signal{syscall.SIGINT, syscall.SIGTERM}, status.Signal())

		assert.Contains(t, c.output(&c.stderr), logDeleted, "the second signal must not end the cleanup")
		assert.NotContains(t, c.output(&c.stderr), "Stopped by a second signal")
	})

	t.Run("a second signal while the Job create is in flight names the Job", func(t *testing.T) {
		c := startInterruptChild(t, "create-held")
		c.await(&c.stderr, logInterruptedCreate)
		c.secondSignal(syscall.SIGINT)
		requireEndedBy(t, c.end(), syscall.SIGINT) // the create is never answered

		stderr := c.output(&c.stderr)
		assert.Contains(t, stderr, "Stopped by a second signal before the cleanup finished")
		assert.Contains(t, stderr, "kubectl delete job "+c.jobName()+" --namespace default")
	})

	t.Run("a second signal during the cleanup of a failed checkpoint reports the checkpoint's error", func(t *testing.T) {
		c := startInterruptChild(t, "failed-cleanup")
		c.await(&c.stderr, logInterruptedJob)
		c.secondSignal(syscall.SIGTERM)
		requireEndedBy(t, c.end(), syscall.SIGTERM) // the delete is never answered

		stderr := c.output(&c.stderr)
		assert.Contains(t, stderr, "Stopped by a second signal before the cleanup finished")
		assert.Contains(t, stderr, "PodSnapshot default/my-snapshot failed: CheckpointFailed", "nothing else prints why the checkpoint failed")
		assert.Contains(t, stderr, "kubectl delete job "+c.jobName()+" --namespace default")
	})

	t.Run("after the checkpoint completed changes nothing", func(t *testing.T) {
		c := startInterruptChild(t, "completed")
		status := c.end()

		require.False(t, status.Signaled(), "a checkpoint that completed must not end by signal %v", status.Signal())
		assert.Equal(t, 0, status.ExitStatus())
		assert.Contains(t, c.output(&c.stdout), "status=completed")
		assert.Contains(t, c.output(&c.stdout), "pod_snapshot=my-snapshot", "the signal must not cut off the result")
		assert.NotContains(t, c.output(&c.stderr), "Interrupted")
		assert.NotContains(t, c.output(&c.stdout), "api: delete-job")
	})

	t.Run("SIGINT that was ignored at the start stays ignored", func(t *testing.T) {
		c := startInterruptChild(t, "sigint-ignored", func(c *interruptChild) {
			// A shell starts a background command of a script in this way.
			c.cmd = exec.Command("/bin/sh", "-c", `trap "" INT; exec "$0" "$@"`, c.cmd.Path, c.cmd.Args[1])
			c.cmd.Env = append(os.Environ(), interruptChildEnv+"=sigint-ignored", interruptChildDirEnv+"="+c.dir)
		})
		status := c.end()

		require.False(t, status.Signaled(), "the command ended by signal %v", status.Signal())
		assert.Equal(t, 0, status.ExitStatus())
		assert.Contains(t, c.output(&c.stdout), "status=completed")
		assert.NotContains(t, c.output(&c.stderr), "Interrupted")
	})

	t.Run("with its output pipe closed still deletes the Job", func(t *testing.T) {
		// Ctrl+C on "snapshotctl checkpoint ... 2>&1 | tee log" also ends tee.
		c := startInterruptChild(t, "pending")
		c.await(&c.stdout, "api: "+callGetSnapshot)
		require.NoError(t, c.errOut.Close())
		require.NoError(t, c.cmd.Process.Signal(syscall.SIGINT))
		requireEndedBy(t, c.end(), syscall.SIGINT)

		assert.Contains(t, c.output(&c.stdout), "api: delete-job propagation=Background")
	})

	t.Run("a checkpoint that fails with no signal exits with status 1", func(t *testing.T) {
		c := startInterruptChild(t, "failed-no-signal")
		status := c.end()

		require.False(t, status.Signaled(), "the command ended by signal %v", status.Signal())
		assert.Equal(t, 1, status.ExitStatus())
		assert.Contains(t, c.output(&c.stderr), "PodSnapshot default/my-snapshot failed: CheckpointFailed")
		assert.NotContains(t, c.output(&c.stderr), logDeleted, "only an interrupted command reports the delete")
	})
}
