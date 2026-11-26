// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storageprobe

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// ParseSharedCredentials reads the [default] profile of a shared-credentials
// file.
func ParseSharedCredentials(data []byte) (aws.Credentials, error) {
	var credentials aws.Credentials
	inDefault := false
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
			continue
		case strings.HasPrefix(line, "["):
			inDefault = strings.TrimSpace(strings.Trim(line, "[]")) == "default"
			continue
		case !inDefault:
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = propertyValue(value)
		switch strings.TrimSpace(key) {
		case "aws_access_key_id":
			credentials.AccessKeyID = value
		case "aws_secret_access_key":
			credentials.SecretAccessKey = value
		case "aws_session_token":
			credentials.SessionToken = value
		}
	}
	if !credentials.HasKeys() {
		return aws.Credentials{}, fmt.Errorf("the credentials file has no access key in its [default] profile")
	}
	return credentials, nil
}

// propertyValue drops an inline comment and surrounding quotes, as the SDK's
// shared-credentials parser does.
func propertyValue(value string) string {
	for _, comment := range []string{" #", " ;", "\t#", "\t;"} {
		value, _, _ = strings.Cut(value, comment)
	}
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		first, last := value[0], value[len(value)-1]
		if first == last && (first == '"' || first == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}
