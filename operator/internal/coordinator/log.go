// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"fmt"
	"regexp"

	"github.com/go-logr/logr"
)

// kvSubjectRE matches a key-value subject, which holds a key and, for a
// barrier, its name.
var kvSubjectRE = regexp.MustCompile(`\$KV\.[^\s"']*`)

// natsLogger routes the embedded server's log into the coordinator's, with
// every key-value subject redacted: the server names the subject of a refused
// publish.
type natsLogger struct{ log logr.Logger }

func (l natsLogger) message(format string, v []any) string {
	return kvSubjectRE.ReplaceAllString(fmt.Sprintf(format, v...), "$$KV.[redacted]")
}

// Noticef implements server.Logger.
func (l natsLogger) Noticef(format string, v ...any) { l.log.Info(l.message(format, v)) }

// Warnf implements server.Logger.
func (l natsLogger) Warnf(format string, v ...any) {
	l.log.Info(l.message(format, v), "level", "warning")
}

// Errorf implements server.Logger.
func (l natsLogger) Errorf(format string, v ...any) { l.log.Error(nil, l.message(format, v)) }

// Fatalf implements server.Logger.
func (l natsLogger) Fatalf(format string, v ...any) {
	l.log.Error(nil, l.message(format, v), "level", "fatal")
}

// Debugf implements server.Logger.
func (l natsLogger) Debugf(format string, v ...any) { l.log.V(1).Info(l.message(format, v)) }

// Tracef implements server.Logger.
func (l natsLogger) Tracef(format string, v ...any) { l.log.V(2).Info(l.message(format, v)) }
