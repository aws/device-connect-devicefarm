// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import "syscall"

// errPeerReset is the error a read or write returns after the peer reset the connection.
// Winsock reports WSAECONNRESET; syscall.ECONNRESET is a placeholder on Windows that
// no socket call returns.
const errPeerReset syscall.Errno = syscall.WSAECONNRESET
