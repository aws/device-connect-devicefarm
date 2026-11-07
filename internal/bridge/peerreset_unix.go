// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package bridge

import "syscall"

// errPeerReset is the error a read or write returns after the peer reset the connection.
const errPeerReset syscall.Errno = syscall.ECONNRESET
