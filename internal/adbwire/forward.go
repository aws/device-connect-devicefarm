package adbwire

import (
	"fmt"
	"strings"
)

// AdbForwardAction is which member of the forward family a service names. adb's
// model: the services adb.cpp handle_forward_request answers. The Go enum is
// the daemon's own.
type AdbForwardAction int

const (
	// AdbActionForward installs a rule: "forward:[norebind:]<local>;<remote>".
	AdbActionForward AdbForwardAction = iota
	// AdbActionKillForward removes one rule: "killforward:<local>".
	AdbActionKillForward
	// AdbActionKillForwardAll removes every rule: "killforward-all".
	AdbActionKillForwardAll
	// AdbActionListForward renders the rule table: "list-forward".
	AdbActionListForward
)

// AdbForwardRequest is a parsed forward-family service. adb's model: the pieces
// adb.cpp handle_forward_request reads; the struct is the daemon's own. Local
// and Remote are the raw spec texts: adb validates specs at listener-install
// time, not at request parse time, and the failure texts differ, so the
// responder owns spec validation.
type AdbForwardRequest struct {
	Action   AdbForwardAction
	NoRebind bool
	Local    string // AdbActionForward and AdbActionKillForward
	Remote   string // AdbActionForward only
}

// ParseForwardService parses the service text that follows a host prefix.
// found is false when the service is not part of the forward family; such
// requests stay on the byte-blind path. A non-nil error means the service IS
// forward-family but malformed, and the error text is the exact FAIL reason
// adb sends (adb.cpp handle_forward_request). For forward: and killforward:
// adb acquires the transport before it reads the pieces, so a device miss
// outranks a malformed request. This daemon keeps that order. list-forward and
// killforward-all never acquire one.
func ParseForwardService(service string) (AdbForwardRequest, bool, error) {
	switch service {
	case "list-forward":
		return AdbForwardRequest{Action: AdbActionListForward}, true, nil
	case "killforward-all":
		return AdbForwardRequest{Action: AdbActionKillForwardAll}, true, nil
	}

	if rest, found := strings.CutPrefix(service, "killforward:"); found {
		// adb: exactly one non-empty piece.
		if rest == "" || strings.Contains(rest, ";") {
			return AdbForwardRequest{}, true, fmt.Errorf("bad killforward: %s", rest)
		}
		return AdbForwardRequest{Action: AdbActionKillForward, Local: rest}, true, nil
	}

	if rest, found := strings.CutPrefix(service, "forward:"); found {
		noRebind := false
		if trimmed, found := strings.CutPrefix(rest, "norebind:"); found {
			noRebind = true
			rest = trimmed
		}
		// adb: exactly two non-empty pieces, and the remote must not name a
		// smart-socket listener (leading '*').
		pieces := strings.Split(rest, ";")
		if len(pieces) != 2 || pieces[0] == "" || pieces[1] == "" || pieces[1][0] == '*' {
			return AdbForwardRequest{}, true, fmt.Errorf("bad forward: %s", rest)
		}
		return AdbForwardRequest{Action: AdbActionForward, NoRebind: noRebind, Local: pieces[0], Remote: pieces[1]}, true, nil
	}

	return AdbForwardRequest{}, false, nil
}
