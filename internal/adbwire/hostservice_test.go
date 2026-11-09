package adbwire

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGivenHostPrefixedRequestsWhenSplitThenSelectorAndServiceReturned(t *testing.T) {
	tests := []struct {
		name     string
		request  string
		selector AdbDeviceSelector
		service  string
	}{
		{
			name:     "bare host prefix selects any device",
			request:  "host:forward:tcp:1234;tcp:9090",
			selector: AdbDeviceSelector{Kind: AdbSelectorAny},
			service:  "forward:tcp:1234;tcp:9090",
		},
		{
			name:     "host-usb",
			request:  "host-usb:list-forward",
			selector: AdbDeviceSelector{Kind: AdbSelectorUSB},
			service:  "list-forward",
		},
		{
			name:     "host-local",
			request:  "host-local:killforward-all",
			selector: AdbDeviceSelector{Kind: AdbSelectorLocal},
			service:  "killforward-all",
		},
		{
			name:     "host-serial with a plain serial",
			request:  "host-serial:R3CN30XXXX:forward:tcp:1234;tcp:9090",
			selector: AdbDeviceSelector{Kind: AdbSelectorSerial, Serial: "R3CN30XXXX"},
			service:  "forward:tcp:1234;tcp:9090",
		},
		{
			name:     "host-serial merges a tcp serial's port segment",
			request:  "host-serial:192.168.0.1:5555:list-forward",
			selector: AdbDeviceSelector{Kind: AdbSelectorSerial, Serial: "192.168.0.1:5555"},
			service:  "list-forward",
		},
		{
			name:     "host-serial merges only one digit segment",
			request:  "host-serial:192.168.0.1:5555:6666:list-forward",
			selector: AdbDeviceSelector{Kind: AdbSelectorSerial, Serial: "192.168.0.1:5555"},
			service:  "6666:list-forward",
		},
		{
			name:     "host-serial does not merge a non-numeric segment",
			request:  "host-serial:R3CN30XXXX:killforward:tcp:1234",
			selector: AdbDeviceSelector{Kind: AdbSelectorSerial, Serial: "R3CN30XXXX"},
			service:  "killforward:tcp:1234",
		},
		{
			name:     "host-serial does not merge a trailing digit service",
			request:  "host-serial:R3CN30XXXX:1234",
			selector: AdbDeviceSelector{Kind: AdbSelectorSerial, Serial: "R3CN30XXXX"},
			service:  "1234",
		},
		{
			name:     "host-serial merges a digit segment into any serial",
			request:  "host-serial:R3CN30XXXX:1234:list-forward",
			selector: AdbDeviceSelector{Kind: AdbSelectorSerial, Serial: "R3CN30XXXX:1234"},
			service:  "list-forward",
		},
		{
			name:     "host-serial does not merge an empty segment",
			request:  "host-serial:R3CN30XXXX::list-forward",
			selector: AdbDeviceSelector{Kind: AdbSelectorSerial, Serial: "R3CN30XXXX"},
			service:  ":list-forward",
		},
		{
			name:     "host-transport-id",
			request:  "host-transport-id:7:forward:tcp:1;tcp:2",
			selector: AdbDeviceSelector{Kind: AdbSelectorTransportID, TransportID: 7},
			service:  "forward:tcp:1;tcp:2",
		},
		{
			name:     "host-transport-id with leading zeros",
			request:  "host-transport-id:007:list-forward",
			selector: AdbDeviceSelector{Kind: AdbSelectorTransportID, TransportID: 7},
			service:  "list-forward",
		},
		{
			name:     "host-transport-id zero is passed through",
			request:  "host-transport-id:0:list-forward",
			selector: AdbDeviceSelector{Kind: AdbSelectorTransportID, TransportID: 0},
			service:  "list-forward",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			selector, service, ok := SplitHostPrefix(testCase.request)

			require.True(t, ok)
			assert.Equal(t, testCase.selector, selector)
			assert.Equal(t, testCase.service, service)
		})
	}
}

func TestGivenTransportSelectServicesWhenParsedThenSelectorAndIDPayloadFlag(t *testing.T) {
	tests := []struct {
		name      string
		service   string
		selector  AdbDeviceSelector
		carriesID bool
		ok        bool
	}{
		{name: "tport any", service: "tport:any", selector: AdbDeviceSelector{Kind: AdbSelectorAny}, carriesID: true, ok: true},
		{name: "tport usb", service: "tport:usb", selector: AdbDeviceSelector{Kind: AdbSelectorUSB}, carriesID: true, ok: true},
		{name: "tport local", service: "tport:local", selector: AdbDeviceSelector{Kind: AdbSelectorLocal}, carriesID: true, ok: true},
		{
			name:      "tport serial",
			service:   "tport:serial:R3CN30XXXX",
			selector:  AdbDeviceSelector{Kind: AdbSelectorSerial, Serial: "R3CN30XXXX"},
			carriesID: true,
			ok:        true,
		},
		{
			name:      "tport serial keeps a network serial whole",
			service:   "tport:serial:192.168.0.1:5555",
			selector:  AdbDeviceSelector{Kind: AdbSelectorSerial, Serial: "192.168.0.1:5555"},
			carriesID: true,
			ok:        true,
		},
		{
			name:     "transport id",
			service:  "transport-id:7",
			selector: AdbDeviceSelector{Kind: AdbSelectorTransportID, TransportID: 7},
			ok:       true,
		},
		{
			// adb reads 0 as unset, but the id is the caller's to judge.
			name:     "transport id zero",
			service:  "transport-id:0",
			selector: AdbDeviceSelector{Kind: AdbSelectorTransportID},
			ok:       true,
		},
		{
			name:     "the widest id a client can hand back",
			service:  "transport-id:18446744073709551615",
			selector: AdbDeviceSelector{Kind: AdbSelectorTransportID, TransportID: 18446744073709551615},
			ok:       true,
		},
		// The older spellings Android Studio sends: a bare OKAY, no id.
		{name: "the older any spelling", service: "transport-any", selector: AdbDeviceSelector{Kind: AdbSelectorAny}, ok: true},
		{name: "the older usb spelling", service: "transport-usb", selector: AdbDeviceSelector{Kind: AdbSelectorUSB}, ok: true},
		{name: "the older local spelling", service: "transport-local", selector: AdbDeviceSelector{Kind: AdbSelectorLocal}, ok: true},
		{
			name:     "the older serial spelling",
			service:  "transport:R3CN30XXXX",
			selector: AdbDeviceSelector{Kind: AdbSelectorSerial, Serial: "R3CN30XXXX"},
			ok:       true,
		},
		{
			name:     "the older serial spelling keeps a network serial whole",
			service:  "transport:192.168.0.1:5555",
			selector: AdbDeviceSelector{Kind: AdbSelectorSerial, Serial: "192.168.0.1:5555"},
			ok:       true,
		},
		// Not a select: the byte-blind path owns every row below.
		{name: "no service at all", service: ""},
		{name: "forward service", service: "forward:tcp:1;tcp:2"},
		{name: "features probe", service: "features"},
		{name: "devices", service: "devices"},
		{name: "version", service: "version"},
		{name: "list-forward", service: "list-forward"},
		{name: "no separator after tport", service: "tportsomething"},
		{name: "no separator after transport", service: "transportsomething"},
		{name: "the older serial spelling with no serial", service: "transport:"},
		{name: "the older serial spelling in a refused grammar", service: "transport:usb:1-1.2"},
		{name: "tport with no target", service: "tport:"},
		{name: "tport serial with no serial", service: "tport:serial:"},
		{name: "transport id not a number", service: "transport-id:abc"},
		{name: "transport id empty", service: "transport-id:"},
		{name: "transport id in hex", service: "transport-id:0x7"},
		{name: "transport id over uint64", service: "transport-id:18446744073709551616"},
		{name: "serial grammar this parser refuses", service: "tport:serial:usb:1-1.2"},
		{name: "the host prefix is the caller's to strip", service: "host:tport:any"},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			selector, carriesID, ok := ParseTransportSelect(testCase.service)

			assert.Equal(t, testCase.ok, ok)
			assert.Equal(t, testCase.selector, selector)
			assert.Equal(t, testCase.carriesID, carriesID)
		})
	}
}

func TestGivenNonHostRequestsWhenSplitThenNotHandled(t *testing.T) {
	// These stay on the byte-blind path untouched.
	requests := []string{
		"",
		"shell:echo hi",
		"sync:",
		"reverse:forward:tcp:1;tcp:2",
		"hostile:not-a-prefix",
		"host",
	}

	for _, request := range requests {
		_, _, ok := SplitHostPrefix(request)
		assert.False(t, ok, "request %q", request)
	}
}

func TestGivenMalformedHostPrefixesWhenSplitThenNotHandled(t *testing.T) {
	// The byte-blind path owns these too, so the server answers them.
	requests := []string{
		"host:",                            // empty service
		"host-usb:",                        // empty service
		"host-serial:",                     // nothing after the prefix
		"host-serial::forward:tcp:1;tcp:2", // empty serial (adb: any device; refused here)
		"host-serial:R3CN30XXXX",           // serial with no service
		"host-serial:R3CN30XXXX:",          // empty service
		"host-serial:192.168.0.1:5555:",    // port merge leaves no service
		// Serial grammars this parser refuses whole.
		"host-serial:usb:1-1.2:list-forward",
		"host-serial:product:foo:list-forward",
		"host-serial:model:foo:list-forward",
		"host-serial:device:foo:list-forward",
		"host-serial:localfilesystem:foo:list-forward",
		"host-serial:tcp:192.168.0.1:5555:list-forward",
		"host-serial:udp:192.168.0.1:5555:list-forward",
		"host-serial:vsock:3:5555:list-forward",
		"host-serial:[::1]:5555:list-forward",
		// Transport ids: decimal digits only, as adb's from_chars.
		"host-transport-id:abc:list-forward",
		"host-transport-id:+7:list-forward",
		"host-transport-id:-7:list-forward",
		"host-transport-id: 7:list-forward",
		"host-transport-id:0x7:list-forward",
		"host-transport-id:1_000:list-forward",
		"host-transport-id:12abc:list-forward",
		"host-transport-id:18446744073709551616:list-forward", // over uint64
		"host-transport-id::list-forward",                     // empty id
		"host-transport-id:12",                                // id with no service
		"host-transport-id:12:",                               // empty service
	}

	for _, request := range requests {
		_, _, ok := SplitHostPrefix(request)
		assert.False(t, ok, "request %q", request)
	}
}
