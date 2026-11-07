// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package testsupport

// Presigned-shaped query markers on an endpoint nothing listens on: every dial fails while
// holding the URL, and an error that still carries it lands in the log.
const (
	SignatureMarker              = "SIGNATURE-MARKER-0123456789abcdef"
	SecurityTokenMarker          = "SECURITY-TOKEN-MARKER-fedcba9876543210"
	PresignedUnreachableEndpoint = "wss://127.0.0.1:1/?X-Amz-Signature=" + SignatureMarker +
		"&X-Amz-Security-Token=" + SecurityTokenMarker

	// ScrubbedDialText is how the dialer renders PresignedUnreachableEndpoint in its error:
	// scheme and host, no path or query. Its presence in a log proves the dial ran.
	ScrubbedDialText = "websocket dial wss://127.0.0.1:1 route "
)
