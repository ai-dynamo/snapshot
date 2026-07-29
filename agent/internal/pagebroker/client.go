// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package pagebroker

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

type Transaction struct{ socket, id, staging, scratch string }

func Stage(ctx context.Context, socket, checkpoint string) (*Transaction, error) {
	if socket == "" {
		socket = "/run/pagebroker/pagebroker.sock"
	}
	id := "tx-" + uuid.NewString()
	r, err := call(ctx, socket, 1, id, checkpoint)
	if err != nil {
		return nil, err
	}
	if !r.ok {
		return nil, fmt.Errorf("submit rejected: %s", r.err)
	}
	r, err = call(ctx, socket, 2, id, "")
	if err != nil {
		_, _ = call(context.Background(), socket, 4, id, "")
		return nil, err
	}
	if !r.ok {
		_, _ = call(context.Background(), socket, 4, id, "")
		return nil, fmt.Errorf("wait-ready rejected: %s", r.err)
	}
	return &Transaction{socket: socket, id: id, staging: r.staging, scratch: r.scratch}, nil
}
func (t *Transaction) Files() ([]*os.File, error) {
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
	return []*os.File{image, work}, nil
}
func (t *Transaction) Commit() error {
	r, err := call(context.Background(), t.socket, 3, t.id, "")
	if err != nil {
		return err
	}
	if !r.ok {
		return fmt.Errorf("commit rejected: %s", r.err)
	}
	return nil
}
func (t *Transaction) Abort() error {
	r, err := call(context.Background(), t.socket, 4, t.id, "")
	if err != nil {
		return err
	}
	if !r.ok {
		return fmt.Errorf("abort rejected: %s", r.err)
	}
	return nil
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
