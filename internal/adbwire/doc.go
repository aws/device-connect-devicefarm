// Package adbwire implements the pieces of the ADB smart-socket host protocol
// the daemon speaks itself. Pure protocol logic: no I/O beyond an io.Writer,
// no internal imports.
//
//   - Request framing: a 4-hex-digit payload length, then the payload, for the
//     requests an adb client writes and the ones the daemon fabricates
//     (sockets.cpp smart_socket_enqueue).
//   - Host-prefix parsing: splits a request into its device selector and the
//     bare service that follows (sockets.cpp parse_host_service), and reads
//     that service half as a transport select, the request a modern client
//     sends before its forward or reverse service (client/adb_client.cpp
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
//     OKAY and the 8 raw little-endian bytes of a uint64 transport id, or a
//     bare OKAY for the older spellings (adb.cpp handle_host_request; adb.h
//     TransportId).
//
// Two markers run through adbwire, forward and bridge. "adb's model": adb
// defines it and the Go code mirrors it, under adb's name where it has one,
// with the adb source in its comment. "The daemon's own": the daemon defines
// it; where it stands in for an adb piece, the comment names that piece. In
// adbwire and forward an exported type whose values or fields hold adb's model
// carries the Adb prefix, and so does each of its constants, even one adb
// lacks; the Go type itself is the daemon's own. An exported type without the
// prefix is the daemon's own. Every other name here is adb's model unless its
// comment marks it the daemon's own. The device host runs adb 1.0.39
// (platform-tools 27) or 1.0.41 (platform-tools 37). adb pins a client to its
// server's version, so this daemon meets only those two. A model holds for both
// unless its comment says otherwise; the 1.0.39 claims were read against the
// android-8.1.0_r1 tag of platform/system/core.
//
// Pinned against the adb host server source at
// https://android.googlesource.com/platform/packages/modules/adb/+/refs/heads/main/
// fetched 2026-08-29 and re-read 2026-09-10; the transport select's reply also
// against the android-11.0.0_r1 tag of platform/system/core and real clients at
// the wire. The tests pin the exact bytes and texts.
package adbwire
