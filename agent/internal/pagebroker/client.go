// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package pagebroker

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"

	"github.com/google/uuid"
)

type Transaction struct {
	socket, id, staging, scratch string
	provider                     *os.File
	control                      *os.File
}

func (t *Transaction) StagingPath() string { return t.staging }
func (t *Transaction) ID() string          { return t.id }

func Stage(ctx context.Context, socket, checkpoint string) (*Transaction, error) {
	if socket == "" {
		socket = "/run/pagebroker/pagebroker.sock"
	}
	id := "tx-" + uuid.NewString()
	manifestRef, _, err := EnsureManifest(checkpoint)
	if err != nil {
		return nil, err
	}
	r, provider, err := submit(ctx, socket, id, checkpoint, manifestRef)
	if err != nil {
		return nil, err
	}
	if !r.ok {
		return nil, fmt.Errorf("submit rejected: %s", r.err)
	}
	// Metadata stays in the mounted checkpoint tree. Page payloads are supplied
	// through the transaction-owned memfds described by the manifest.
	if _, err := os.Stat(checkpoint); err != nil {
		_ = provider.Close()
		_, _ = call(context.Background(), socket, 5, id, "")
		return nil, fmt.Errorf("checkpoint is unavailable after submit: %w", err)
	}
	control, err := connectFile(ctx, socket)
	if err != nil {
		_ = provider.Close()
		_, _ = call(context.Background(), socket, 5, id, "")
		return nil, fmt.Errorf("open PageBroker readiness connection: %w", err)
	}
	return &Transaction{socket: socket, id: id, staging: checkpoint, scratch: r.scratch, provider: provider, control: control}, nil
}
func PrepareCheckpoint(ctx context.Context, socket, checkpoint string) (*Transaction, error) {
	if socket == "" {
		socket = "/run/pagebroker/pagebroker.sock"
	}
	id := "tx-" + uuid.NewString()
	r, err := call(ctx, socket, 3, id, checkpoint)
	if err != nil {
		return nil, err
	}
	if !r.ok {
		return nil, fmt.Errorf("checkpoint prepare rejected: %s", r.err)
	}
	return &Transaction{socket: socket, id: id, staging: r.staging, scratch: r.scratch}, nil
}
func (t *Transaction) Files() ([]*os.File, error) {
	if t.provider == nil || t.control == nil {
		return nil, fmt.Errorf("PageBroker restore connections are unavailable")
	}
	image, err := os.Open(t.staging)
	if err != nil {
		return nil, fmt.Errorf("open staged checkpoint: %w", err)
	}
	scratch := filepath.Clean(t.scratch)
	if err := os.MkdirAll(scratch, 0755); err != nil {
		image.Close()
		return nil, err
	}
	work, err := os.Open(scratch)
	if err != nil {
		image.Close()
		return nil, fmt.Errorf("open PageBroker scratch: %w", err)
	}
	provider := t.provider
	control := t.control
	t.provider = nil
	t.control = nil
	return []*os.File{image, work, provider, control}, nil
}
func (t *Transaction) Commit() error {
	t.closeProvider()
	r, err := call(context.Background(), t.socket, 4, t.id, "")
	if err != nil {
		return err
	}
	if !r.ok {
		return fmt.Errorf("commit rejected: %s", r.err)
	}
	return nil
}
func (t *Transaction) Abort() error {
	t.closeProvider()
	r, err := call(context.Background(), t.socket, 5, t.id, "")
	if err != nil {
		return err
	}
	if !r.ok {
		return fmt.Errorf("abort rejected: %s", r.err)
	}
	return nil
}

func (t *Transaction) closeProvider() {
	if t.provider != nil {
		_ = t.provider.Close()
		t.provider = nil
	}
	if t.control != nil {
		_ = t.control.Close()
		t.control = nil
	}
}

type response struct {
	ok                    bool
	staging, scratch, err string
}

func varint(v uint64) []byte {
	var b []byte
	for v >= 128 {
		b = append(b, byte(v)|128)
		v >>= 7
	}
	return append(b, byte(v))
}
func field(n int, v []byte) []byte {
	return append(append(varint(uint64(n*8+2)), varint(uint64(len(v)))...), v...)
}
func varintField(n int, v uint64) []byte {
	return append(varint(uint64(n*8)), varint(v)...)
}
func request(op int, id, path string) []byte {
	b := append(varint(8), varint(uint64(op))...)
	if id != "" {
		b = append(b, field(2, []byte(id))...)
	}
	if path != "" {
		b = append(b, field(3, []byte(path))...)
	}
	return b
}

func manifestWireData(manifest *Manifest) []byte {
	b := request(1, "", "")
	b = append(b, varintField(5, manifest.ResidentBytes)...)
	imageNames := make([]string, 0, len(manifest.Images))
	for name := range manifest.Images {
		imageNames = append(imageNames, name)
	}
	sort.Strings(imageNames)
	for _, name := range imageNames {
		image := manifest.Images[name]
		encoded := field(1, []byte(name))
		encoded = append(encoded, field(2, []byte(image.URI))...)
		encoded = append(encoded, varintField(3, image.Size)...)
		b = append(b, field(6, encoded)...)
	}
	for _, object := range manifest.HostMemoryObjects {
		encoded := varintField(1, object.MemoryID)
		encoded = append(encoded, field(2, []byte(object.Name))...)
		encoded = append(encoded, varintField(3, uint64(object.PID))...)
		encoded = append(encoded, varintField(4, uint64(object.VMAID))...)
		encoded = append(encoded, varintField(5, object.Shmid)...)
		encoded = append(encoded, varintField(6, object.DstAddr)...)
		encoded = append(encoded, varintField(7, object.Length)...)
		encoded = append(encoded, field(8, []byte(object.Semantics))...)
		encoded = append(encoded, field(9, []byte(object.MapMode))...)
		for _, source := range object.SourceRange {
			rangeData := field(1, []byte(source.Object))
			rangeData = append(rangeData, varintField(2, source.SourceOffset)...)
			rangeData = append(rangeData, varintField(3, source.DstOffset)...)
			rangeData = append(rangeData, varintField(4, source.Length)...)
			encoded = append(encoded, field(10, rangeData)...)
		}
		b = append(b, field(7, encoded)...)
	}
	return b
}

func submitRequest(id, checkpoint, manifestRef string) []byte {
	b := request(1, id, checkpoint)
	return append(b, field(4, []byte(manifestRef))...)
}

func connectFile(ctx context.Context, socket string) (*os.File, error) {
	connection, err := (&net.Dialer{}).DialContext(ctx, "unixpacket", socket)
	if err != nil {
		return nil, err
	}
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		connection.Close()
		return nil, fmt.Errorf("PageBroker connection is %T, want UnixConn", connection)
	}
	file, err := unixConnection.File()
	connection.Close()
	if err != nil {
		return nil, err
	}
	return file, nil
}

func submit(ctx context.Context, socket, id, checkpoint, manifestRef string) (response, *os.File, error) {
	connection, err := (&net.Dialer{}).DialContext(ctx, "unixpacket", socket)
	if err != nil {
		return response{}, nil, err
	}
	defer connection.Close()
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return response{}, nil, fmt.Errorf("PageBroker connection is %T, want UnixConn", connection)
	}
	if _, err := connection.Write(submitRequest(id, checkpoint, manifestRef)); err != nil {
		return response{}, nil, err
	}
	buf := make([]byte, 65536)
	n, err := connection.Read(buf)
	if err != nil {
		return response{}, nil, err
	}
	r, err := parse(buf[:n])
	if err != nil || !r.ok {
		return r, nil, err
	}
	provider, err := unixConnection.File()
	if err != nil {
		return response{}, nil, fmt.Errorf("duplicate PageBroker provider connection: %w", err)
	}
	return r, provider, nil
}

// WaitReady uses the dedicated descriptor inherited by nsrestore. It blocks
// until every manifest range has completed or PageBroker reports an error.
func WaitReady(control *os.File, transactionID string) error {
	connection, err := net.FileConn(control)
	if err != nil {
		return err
	}
	defer connection.Close()
	if _, err := connection.Write(request(2, transactionID, "")); err != nil {
		return err
	}
	buffer := make([]byte, 65536)
	n, err := connection.Read(buffer)
	if err != nil {
		return err
	}
	result, err := parse(buffer[:n])
	if err != nil {
		return err
	}
	if !result.ok {
		return fmt.Errorf("PageBroker readiness failed: %s", result.err)
	}
	return nil
}

func call(ctx context.Context, socket string, op int, id, path string) (response, error) {
	c, err := (&net.Dialer{}).DialContext(ctx, "unixpacket", socket)
	if err != nil {
		return response{}, err
	}
	defer c.Close()
	if _, err := c.Write(request(op, id, path)); err != nil {
		return response{}, err
	}
	buf := make([]byte, 65536)
	n, err := c.Read(buf)
	if err != nil {
		return response{}, err
	}
	return parse(buf[:n])
}
func parse(b []byte) (response, error) {
	var r response
	for len(b) > 0 {
		tag, n := read(b)
		if n == 0 {
			return r, fmt.Errorf("invalid PageBroker response")
		}
		b = b[n:]
		f, w := int(tag>>3), tag&7
		if w == 0 {
			v, k := read(b)
			if k == 0 {
				return r, fmt.Errorf("invalid response varint")
			}
			b = b[k:]
			if f == 1 {
				r.ok = v != 0
			}
		} else if w == 2 {
			l, k := read(b)
			if k == 0 || l > uint64(len(b)-k) {
				return r, fmt.Errorf("invalid response string")
			}
			v := string(b[k : k+int(l)])
			b = b[k+int(l):]
			switch f {
			case 3:
				r.staging = v
			case 4:
				r.scratch = v
			case 5:
				r.err = v
			}
		} else {
			return r, fmt.Errorf("unsupported response wire type")
		}
	}
	return r, nil
}
func read(b []byte) (uint64, int) {
	var v uint64
	for i, c := range b {
		v |= uint64(c&127) << uint(7*i)
		if c < 128 {
			return v, i + 1
		}
		if i == 9 {
			return 0, 0
		}
	}
	return 0, 0
}
