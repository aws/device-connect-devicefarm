// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package forward

// AdbInstallStatus classifies an Install outcome. adb's model: the
// adb_listeners.h InstallStatus values an install can hit. The Go enum is the
// daemon's own, as is each value adb lacks.
type AdbInstallStatus int

const (
	// AdbInstallOK is a fresh bind or an in-place rebind.
	AdbInstallOK AdbInstallStatus = iota
	// AdbInstallCannotBind is any failure to listen on the local spec.
	AdbInstallCannotBind
	// AdbInstallCannotRebind is norebind against an existing rule.
	AdbInstallCannotRebind
	// AdbInstallUnsupportedRemote is a remote spec that is not tcp:<port>, which
	// adb lacks: adb defers remote validation to connect time. Refusing at
	// install is deliberate, since every connection through such a rule would
	// die with zero bytes.
	AdbInstallUnsupportedRemote
	// AdbInstallTooManyRules is a new local at the rule limit, which adb
	// lacks. A rule costs a host listener, and its connections spend the
	// attachment's stream budget.
	AdbInstallTooManyRules
)

// AdbInstallResult is one Install outcome. adb's model: the status
// install_listener (adb_listeners.cpp) returns, and the port and error it fills
// through out-parameters; adb.cpp builds the FAIL
// text from them. The struct is the daemon's own. FailReason is the exact FAIL
// text for a non-OK status. ResolvedPort is the OS-assigned port for a tcp:0
// local, else zero; adb sends the extra protocol string only then.
type AdbInstallResult struct {
	Status       AdbInstallStatus
	FailReason   string
	ResolvedPort int
}
