// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCustomStorageBrokerOutsidePIDNamespace(t *testing.T) {
	if os.Getenv("SNAPSHOT_TEST_BROKER_NAMESPACE") == "child" {
		if err := unix.PidfdSendSignal(3, 0, nil, 0); !errors.Is(err, unix.EINVAL) {
			t.Fatalf("signal ancestor namespace broker: %v, want EINVAL", err)
		}
		input := customStorageFlags{
			brokerProcessFD: 3, pageBrokerSocketDirectoryFD: 4, hostProcFD: 4,
			executionFD: 5, cancelFD: 6,
			pageBrokerSocketName: "broker.sock", pageBrokerTransactionID: "restore",
			gpuContext: `{"captured_pids":[12],"visible_devices":["GPU-target"]}`,
		}
		execution, _, err := parseCustomStorageOptions(customStorageTestFlags(t, "--pagebroker-transaction=restore"), input)
		if err != nil {
			t.Fatal(err)
		}
		defer execution.Close()
		poll := []unix.PollFd{{Fd: 3, Events: unix.POLLIN}}
		if n, err := unix.Poll(poll, 0); err != nil || n != 0 {
			t.Fatalf("cannot observe live broker outside namespace: %v, %v", poll, err)
		}
		return
	}
	pidfd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		t.Fatal(err)
	}
	broker := os.NewFile(uintptr(pidfd), "broker")
	defer broker.Close()
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	socketFD, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	socket := os.NewFile(uintptr(socketFD), "execution")
	defer socket.Close()
	cancelRead, cancelWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer cancelRead.Close()
	defer cancelWrite.Close()
	child := exec.Command(os.Args[0], "-test.run=^TestCustomStorageBrokerOutsidePIDNamespace$")
	child.Env = append(os.Environ(), "SNAPSHOT_TEST_BROKER_NAMESPACE=child")
	child.ExtraFiles = []*os.File{broker, directory, socket, cancelRead}
	child.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	output, err := child.CombinedOutput()
	if errors.Is(err, syscall.EPERM) {
		t.Skip("user/PID namespace creation is unavailable")
	}
	if err != nil {
		t.Fatalf("inherited broker in child PID namespace: %v\n%s", err, output)
	}
}

var customStorageFlagNames = []string{
	"pagebroker-socket-directory-fd",
	"host-proc-fd",
	"pagebroker-execution-fd",
	"cancel-fd",
	"pagebroker-process-fd",
	"pagebroker-socket-name",
	"gpu-context",
	"pagebroker-transaction",
}

func customStorageTestFlags(t *testing.T, supplied ...string) *flag.FlagSet {
	t.Helper()
	flags := flag.NewFlagSet("nsrestore", flag.ContinueOnError)
	for _, name := range customStorageFlagNames {
		flags.String(name, "", "")
	}
	if err := flags.Parse(supplied); err != nil {
		t.Fatal(err)
	}
	return flags
}

func TestCustomStorageOptionsRequireAllFlags(t *testing.T) {
	input := customStorageFlags{pageBrokerSocketDirectoryFD: -1, hostProcFD: -1}
	execution, hostProc, err := parseCustomStorageOptions(customStorageTestFlags(t), input)
	if err != nil || execution != nil || hostProc != nil {
		t.Fatalf("omitted CustomStorage options = %v, %v, %v", execution, hostProc, err)
	}
	for _, name := range customStorageFlagNames {
		t.Run(name, func(t *testing.T) {
			flags := customStorageTestFlags(t, "--"+name+"=")
			if _, _, err := parseCustomStorageOptions(flags, input); err == nil || !strings.Contains(err.Error(), "CustomStorage requires") {
				t.Fatalf("incomplete options with --%s: %v", name, err)
			}
		})
	}
}

func TestCustomStorageOptionsValidateDescriptors(t *testing.T) {
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	file, err := os.Create(filepath.Join(t.TempDir(), "file"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, kind := range []string{"directory", "closed", "socket-file", "host-proc-file", "execution-file", "cancel-file", "broker-file", "malformed-context"} {
		t.Run(kind, func(t *testing.T) {
			socketSource, procSource := directory, directory
			if kind == "socket-file" {
				socketSource = file
			}
			if kind == "host-proc-file" {
				procSource = file
			}
			socketFD, err := unix.Dup(int(socketSource.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			procFD, err := unix.Dup(int(procSource.Fd()))
			if err != nil {
				unix.Close(socketFD)
				t.Fatal(err)
			}
			executionFD, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
			if err != nil {
				t.Fatal(err)
			}
			cancelRead, cancelWrite, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer cancelRead.Close()
			defer cancelWrite.Close()
			brokerFD, err := unix.PidfdOpen(os.Getpid(), 0)
			if err != nil {
				t.Fatal(err)
			}
			input := customStorageFlags{
				pageBrokerSocketDirectoryFD: socketFD,
				hostProcFD:                  procFD,
				executionFD:                 executionFD,
				cancelFD:                    int(cancelRead.Fd()),
				brokerProcessFD:             brokerFD,
				pageBrokerSocketName:        "broker.sock",
				pageBrokerTransactionID:     "restore",
				gpuContext:                  `{"captured_pids":[12],"visible_devices":["GPU-target"]}`,
			}
			if kind == "malformed-context" {
				input.gpuContext = "{"
			}
			if kind == "execution-file" {
				input.executionFD = int(file.Fd())
			}
			if kind == "broker-file" {
				input.brokerProcessFD = int(file.Fd())
			}
			if kind == "cancel-file" {
				input.cancelFD = int(file.Fd())
			}
			if kind == "closed" {
				unix.Close(socketFD)
			}
			flags := customStorageTestFlags(t, "--pagebroker-transaction=restore")
			execution, hostProc, err := parseCustomStorageOptions(flags, input)
			if err != nil {
				unix.Close(brokerFD)
				if kind != "closed" {
					unix.Close(socketFD)
				}
				unix.Close(procFD)
				unix.Close(executionFD)
				switch kind {
				case "closed":
					if !errors.Is(err, unix.EBADF) {
						t.Fatalf("closed descriptor: %v, want EBADF", err)
					}
				case "socket-file", "host-proc-file", "execution-file", "cancel-file":
					if !strings.Contains(err.Error(), "descriptor type") {
						t.Fatalf("regular-file descriptor: %v", err)
					}
				case "broker-file":
					if !errors.Is(err, unix.EBADF) {
						t.Fatalf("broker descriptor: %v", err)
					}
				case "malformed-context":
					var syntaxErr *json.SyntaxError
					if !errors.As(err, &syntaxErr) {
						t.Fatalf("malformed GPU context: %v", err)
					}
				default:
					t.Fatal(err)
				}
				return
			}
			defer execution.Close()
			defer hostProc.Close()
			if kind != "directory" {
				t.Fatalf("accepted %s", kind)
			}
			for _, descriptor := range []int{socketFD, procFD, executionFD, int(cancelRead.Fd()), brokerFD} {
				flags, err := unix.FcntlInt(uintptr(descriptor), unix.F_GETFD, 0)
				if err != nil || flags&unix.FD_CLOEXEC == 0 {
					t.Fatalf("descriptor %d is not close-on-exec: %v", descriptor, err)
				}
			}
		})
	}
}
