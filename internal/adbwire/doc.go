// Package adbwire implements the pieces of the ADB smart-socket host protocol
// the daemon or a probe speaks. Pure protocol logic: no I/O beyond an io.Writer,
// no internal imports.
//
//   - Request framing: a 4-hex-digit payload length, then the payload, for the
//     requests an adb client writes and the ones the daemon fabricates
//     (sockets.cpp smart_socket_enqueue).
//   - Host-prefix parsing: splits a request into its device selector and the
//     bare service that follows (sockets.cpp parse_host_service), and reads
//     that service half as a transport select, the request an adb client
//     sends before the service it came for (client/adb_client.cpp
//     switch_socket_transport).
//   - Forward spec and forward-family service parsing: kind:value endpoint
//     specs with adb's tcp port check, and the forward:, killforward:,
//     killforward-all and list-forward services with adb's exact FAIL texts
//     (adb.cpp handle_forward_request; socket_spec.cpp parse_tcp_socket_spec
//     and socket_spec_listen).
//   - Reply writers: the byte-exact OKAY, FAIL and length-prefixed protocol
//     strings the adb host server sends for the forward family, its
//     list-forward table and its failure texts (adb_io.cpp SendOkay, SendFail
//     and SendProtocolString; adb_listeners.cpp format_listeners;
//     transport.cpp acquire_one_transport), plus the transport select's reply:
//     OKAY and the 8 raw little-endian bytes of a uint64 transport id for
//     host:tport:*, a bare OKAY for every other spelling (adb.cpp
//     handle_host_request; adb.h TransportId).
//   - host:version: the framed request and the reply parser, for a caller that
//     asks an adb server its protocol version (adb.cpp handle_host_request).
//
// Two markers run through adbwire, forward and bridge. "adb's model": adb
// defines it and the Go code mirrors it, under adb's name where it has one,
// with the adb source in its comment. "The daemon's own": the daemon defines
// it; where it stands in for an adb piece, the comment names that piece. In
// adbwire and forward an exported type whose values or fields hold adb's model
// carries the Adb prefix, and so does each of its constants, even one adb
// lacks; the Go type itself is the daemon's own. An exported type without the
// prefix is the daemon's own. Every other name here is adb's model unless its
// comment marks it the daemon's own.
//
// This daemon is compatible with adb 1.0.41 and tested with platform-tools 37.
//
// The adb source files named above are at
// https://android.googlesource.com/platform/packages/modules/adb/
package adbwire
