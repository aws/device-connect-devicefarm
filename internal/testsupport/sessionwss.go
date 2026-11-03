package testsupport

import (
	"fmt"
	"regexp"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// sessionUUIDLen is the length of the reconnect signal, a bare UUIDv4 as the first client
// frame. Duplicated from the wire contract like the frame builders.
const sessionUUIDLen = 36

// Websocket close codes the server and the relay send, duplicated from the wire contract like
// the frame builders.
const (
	// SessionEndedCloseCode closes a session's current websocket once the server ends the
	// session; nothing can resume it.
	SessionEndedCloseCode = 4000
	// InternalErrorCloseCode is the relay's close when its leg to the server fails. A server
	// that does not send SessionEndedCloseCode ends sessions with it too.
	InternalErrorCloseCode = 1011
)

// Reconnect signal: a first client frame that is exactly a canonical UUIDv4.
var uuidV4Pattern = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-4[0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

// SessionMockConfig configures a session-model-aware mock /adb endpoint.
type SessionMockConfig struct {
	// Grace bounds how long a session survives with no attached websocket. Zero means
	// sessions never expire.
	Grace time.Duration

	// RejectReattach answers every reconnect with an ERROR frame. Server: session gone.
	// Catches: a client that fails the connection instead of opening a fresh session.
	RejectReattach bool

	// SwallowReattachAck takes the session over but never acks. Server: listener
	// already switched, client never told. Catches: a client that returns the poisoned
	// predecessor to service, whose outbound direction is then a silent black hole.
	SwallowReattachAck bool

	// SwallowFirstReattachAcks bounds SwallowReattachAck to the first N attempts, so a later
	// attempt can reach a different outcome. Pair with a short Grace for the mixed
	// indeterminate-then-refused sequence.
	SwallowFirstReattachAcks int

	// DropFirstReattaches fails the first N reconnects transiently -- socket closed, no
	// control frame, session intact -- as a host that dropped the connection does.
	// Catches: a client with no retry, which loses a session still there for the taking.
	DropFirstReattaches int

	// CleanCutDelay holds a superseded websocket open-but-ignored for this long after the
	// takeover (zero closes it at once), so input there vanishes with no error. It does not
	// reorder the ack; see ReattachAckDelay.
	CleanCutDelay time.Duration

	// ReattachAckDelay holds the ack until after the predecessor is cut, as when the
	// close overtakes the ack. Zero keeps the server's own order: ack, swap, close.
	ReattachAckDelay time.Duration

	// ReattachAckArm replaces the ack with an empty-bodied frame of that arm, so a test can
	// drive the client's fail-closed arm for a frame /adb never carries, or its decoder with
	// a key no arm owns. Empty keeps reattach.
	ReattachAckArm string

	// OnMessage handles raw ADB bytes from the client; reply goes to whichever websocket
	// is currently attached. Nil echoes.
	OnMessage func(session *MockSession, data []byte) (reply []byte)
}

// MockSession is one durable server-side session: it outlives any single websocket,
// as the real server's session does.
type MockSession struct {
	UUID string

	mu       sync.Mutex
	attached *websocket.Conn
	// inbound accumulates client ADB bytes across every websocket of this session, in
	// arrival order, so a test can assert the outbound direction is gap- and dupe-free.
	inbound []byte
	// sent counts bytes delivered, letting a sequence server resume at the exact next
	// index across a reattach.
	sent int
	// dropped accumulates bytes that landed on a superseded websocket. A correct client
	// never lets this grow.
	dropped    []byte
	closed     bool
	graceTimer *time.Timer
}

// Inbound returns a copy of the client ADB bytes this session received, in order.
func (s *MockSession) Inbound() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.inbound...)
}

// Send delivers data to the attached websocket, reporting whether it went anywhere.
func (s *MockSession) Send(data []byte) bool {
	s.mu.Lock()
	target := s.attached
	s.mu.Unlock()
	if target == nil {
		return false
	}
	return websocket.Message.Send(target, data) == nil
}

// recordDropped notes bytes discarded from a superseded websocket.
func (s *MockSession) recordDropped(data []byte) {
	s.mu.Lock()
	s.dropped = append(s.dropped, data...)
	s.mu.Unlock()
}

// Dropped returns bytes silently discarded from an already-superseded websocket. Must
// be empty for a client that fences outbound across the whole reattach round trip.
func (s *MockSession) Dropped() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.dropped...)
}

// SentCount returns how many bytes have been delivered to the client.
func (s *MockSession) SentCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent
}

// AdvanceSent records that n more bytes have been delivered.
func (s *MockSession) AdvanceSent(n int) {
	s.mu.Lock()
	s.sent += n
	s.mu.Unlock()
}

// SessionMockWSS stands in for the server's /adb endpoint: control frame first on every
// connection, a 36-byte UUIDv4 first client frame means reconnect, and a reconnect
// takes over the existing session under the clean-cut rule.
type SessionMockWSS struct {
	URL string

	cfg SessionMockConfig

	mu             sync.Mutex
	sessions       map[string]*MockSession
	sessionsOpened int
	reattaches     int
	nextUUID       int
	reattachDrops  int
	acksSwallowed  int
	// connectionsAccepted counts every websocket served, including ones refused before a
	// session was resolved. Only counter that can measure attempts which never reached
	// a session.
	connectionsAccepted int
	// routesDialed is the ?path= value of every websocket served, in arrival order.
	routesDialed []string
	// droppedSockets marks websockets whose reattach was dropped transiently, so serve
	// closes them silently instead of announcing a refusal.
	droppedSockets map[*websocket.Conn]struct{}
}

// NewSessionMockWSS starts a session-aware mock /adb endpoint, torn down via t.Cleanup.
func NewSessionMockWSS(t *testing.T, cfg SessionMockConfig) *SessionMockWSS {
	t.Helper()
	mock := newSessionMock(cfg)
	mock.URL = NewMockWSS(t, mock.serve)
	return mock
}

// newSessionMock is a session mock with no endpoint yet.
func newSessionMock(cfg SessionMockConfig) *SessionMockWSS {
	return &SessionMockWSS{
		cfg:            cfg,
		sessions:       make(map[string]*MockSession),
		droppedSockets: make(map[*websocket.Conn]struct{}),
	}
}

// ConnectionsAccepted returns how many websockets were served, refusals included.
func (m *SessionMockWSS) ConnectionsAccepted() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connectionsAccepted
}

// RoutesDialed returns the ?path= value each served websocket was dialed with, in
// arrival order, refusals included.
func (m *SessionMockWSS) RoutesDialed() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.routesDialed...)
}

// tookTransientDrop reports whether ws's reattach was dropped rather than refused,
// consuming the mark.
func (m *SessionMockWSS) tookTransientDrop(ws *websocket.Conn) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, dropped := m.droppedSockets[ws]
	delete(m.droppedSockets, ws)
	return dropped
}

// NewSessionEchoWSS starts a session-aware mock that echoes raw ADB bytes back.
func NewSessionEchoWSS(t *testing.T) *SessionMockWSS {
	t.Helper()
	return NewSessionMockWSS(t, SessionMockConfig{})
}

// SessionsOpened returns how many new sessions were created.
func (m *SessionMockWSS) SessionsOpened() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessionsOpened
}

// Reattaches returns how many reconnects have been acked.
func (m *SessionMockWSS) Reattaches() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reattaches
}

// Sessions returns a snapshot of every live session.
func (m *SessionMockWSS) Sessions() []*MockSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	live := make([]*MockSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		live = append(live, session)
	}
	return live
}

// serve handles one websocket for its whole lifetime.
func (m *SessionMockWSS) serve(ws *websocket.Conn) {
	ws.PayloadType = websocket.BinaryFrame

	m.mu.Lock()
	m.connectionsAccepted++
	m.routesDialed = append(m.routesDialed, ws.Request().URL.Query().Get("path"))
	m.mu.Unlock()

	first, err := receive(ws)
	if err != nil {
		ws.Close()
		return
	}

	session, isReconnect := m.selectSession(ws, first)
	if session == nil {
		if m.tookTransientDrop(ws) {
			// Drop, not refusal: no control frame, so the client sees the socket die
			// rather than learning the session is gone.
			ws.Close()
			return
		}
		// Refusal: announce, then close, as the server does.
		websocket.Message.Send(ws, FrameError("unknown or expired session"))
		ws.Close()
		return
	}

	if !isReconnect {
		// New session's first frame is raw ADB bytes, forwarded after the ack.
		m.handleData(session, first)
	}

	for {
		data, err := receive(ws)
		if err != nil {
			break
		}
		if !session.isAttached(ws) {
			// Silent drop, mirroring the server's early return for a non-current
			// listener: a client writing here loses data with NO error.
			// Recorded so a test can assert the loss is zero.
			session.recordDropped(data)
			continue
		}
		m.handleData(session, data)
	}

	session.detach(ws, m)
	ws.Close()
}

// selectSession routes a first frame to a reconnect or a new session, sends the
// matching control frame, and attaches ws. Nil means refused.
func (m *SessionMockWSS) selectSession(ws *websocket.Conn, first []byte) (session *MockSession, isReconnect bool) {
	if len(first) == sessionUUIDLen && uuidV4Pattern.Match(first) {
		return m.reattach(ws, string(first)), true
	}
	return m.newSession(ws), false
}

// reattach hands ws the named session, or nil if it is gone or refusals are configured.
func (m *SessionMockWSS) reattach(ws *websocket.Conn, uuid string) *MockSession {
	if m.cfg.RejectReattach {
		return nil
	}

	m.mu.Lock()
	session := m.sessions[uuid]
	if session != nil {
		m.reattaches++
	}
	// A transient drop still counts as an attempt above, so a test can count attempts.
	transientDrop := session != nil && m.reattachDrops < m.cfg.DropFirstReattaches
	if transientDrop {
		m.reattachDrops++
		m.droppedSockets[ws] = struct{}{}
	}
	swallowAck := session != nil && (m.cfg.SwallowReattachAck ||
		m.acksSwallowed < m.cfg.SwallowFirstReattachAcks)
	if swallowAck {
		m.acksSwallowed++
	}
	m.mu.Unlock()
	if session == nil {
		return nil
	}

	// Vanish without a control frame, session intact: the transient failure worth one
	// retry. serve consults tookTransientDrop so it closes silently.
	if transientDrop {
		return nil
	}

	// Take over but never acknowledge: indistinguishable from a lost ack, so the client
	// must treat the predecessor as poisoned.
	if swallowAck {
		session.takeOver(ws, m.cfg.CleanCutDelay)
		return session
	}

	ack := FrameReattach(uuid)
	if m.cfg.ReattachAckArm != "" {
		ack = FrameEmptyArm(m.cfg.ReattachAckArm)
	}

	// No ack delay: announce BEFORE the swap, matching the server's own code order
	// (ack, switch the pointer, close the old socket).
	if m.cfg.ReattachAckDelay <= 0 {
		if websocket.Message.Send(ws, ack) != nil {
			return nil
		}
		session.takeOver(ws, m.cfg.CleanCutDelay)
		return session
	}

	// Swap first, then ack: makes the cut-before-ack ordering deterministic. The delay
	// stands in for the ack's flight time, not a change in server logic.
	session.takeOver(ws, m.cfg.CleanCutDelay)
	time.Sleep(m.cfg.ReattachAckDelay)
	if websocket.Message.Send(ws, ack) != nil {
		return nil
	}
	return session
}

// newSession mints a session, announces its UUID, and attaches ws.
func (m *SessionMockWSS) newSession(ws *websocket.Conn) *MockSession {
	m.mu.Lock()
	m.nextUUID++
	m.sessionsOpened++
	// Canonical UUIDv4 shape (version nibble 4, variant nibble 8) so the client's
	// anchored validation accepts it, with a counter tail to keep sessions distinct.
	uuid := fmt.Sprintf("11111111-2222-4333-8444-%012d", m.nextUUID)
	session := &MockSession{UUID: uuid}
	m.sessions[uuid] = session
	m.mu.Unlock()

	if websocket.Message.Send(ws, FrameNewSession(uuid)) != nil {
		return nil
	}
	session.takeOver(ws, m.cfg.CleanCutDelay)
	return session
}

// handleData records inbound ADB bytes and sends back what the handler produces
// (echo by default).
func (m *SessionMockWSS) handleData(session *MockSession, data []byte) {
	session.mu.Lock()
	session.inbound = append(session.inbound, data...)
	session.mu.Unlock()

	var reply []byte
	if m.cfg.OnMessage != nil {
		reply = m.cfg.OnMessage(session, data)
	} else {
		reply = data
	}
	if len(reply) > 0 {
		session.Send(reply)
		session.AdvanceSent(len(reply))
	}
}

// drop removes a session from the registry.
func (m *SessionMockWSS) drop(uuid string) {
	m.mu.Lock()
	delete(m.sessions, uuid)
	m.mu.Unlock()
}

// EndSession ends session as the server does once the device side of its adb stream closes:
// the attached websocket is closed with closeCode, and every later reattach is refused. A grace
// timer already armed finds the session closed and does nothing.
func (m *SessionMockWSS) EndSession(session *MockSession, closeCode int) {
	session.mu.Lock()
	attached := session.attached
	session.attached = nil
	session.closed = true
	session.mu.Unlock()
	m.drop(session.UUID)
	closeWithCode(attached, closeCode)
}

// DropAttached closes the attached websocket with closeCode and keeps the session, as a relay
// whose leg to the server failed does: a reattach still resumes it.
func (s *MockSession) DropAttached(closeCode int) {
	s.mu.Lock()
	attached := s.attached
	s.mu.Unlock()
	closeWithCode(attached, closeCode)
}

// closeWithCode sends a close frame carrying closeCode, then closes ws. x/net's Close sends a
// second close frame; a client stops reading at the first. Nil is a no-op.
func closeWithCode(ws *websocket.Conn, closeCode int) {
	if ws == nil {
		return
	}
	ws.WriteClose(closeCode)
	ws.Close()
}

// takeOver makes ws the attached websocket and closes the predecessor under the
// clean-cut rule. A positive delay closes it after this returns; zero closes it at
// once, before the caller has read the ack.
func (s *MockSession) takeOver(ws *websocket.Conn, cleanCutDelay time.Duration) {
	s.mu.Lock()
	if s.graceTimer != nil {
		s.graceTimer.Stop()
		s.graceTimer = nil
	}
	superseded := s.attached
	s.attached = ws
	s.mu.Unlock()

	if superseded == nil || superseded == ws {
		return
	}
	// The swap above already made the predecessor non-current; the close is a later event.
	// See CleanCutDelay.
	if cleanCutDelay > 0 {
		go func() {
			time.Sleep(cleanCutDelay)
			superseded.Close()
		}()
		return
	}
	superseded.Close()
}

// detach clears ws and arms the grace timer. A superseded websocket's detach is
// ignored, so it cannot disturb the successor that took over.
func (s *MockSession) detach(ws *websocket.Conn, m *SessionMockWSS) {
	s.mu.Lock()
	if s.attached != ws || s.closed {
		s.mu.Unlock()
		return
	}
	s.attached = nil
	grace := m.cfg.Grace
	if grace > 0 {
		s.graceTimer = time.AfterFunc(grace, func() {
			s.mu.Lock()
			expired := s.attached == nil && !s.closed
			if expired {
				s.closed = true
			}
			s.mu.Unlock()
			if expired {
				m.drop(s.UUID)
			}
		})
	}
	s.mu.Unlock()
}

// isAttached reports whether ws is the session's current websocket.
func (s *MockSession) isAttached(ws *websocket.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attached == ws
}

// SequenceConfig configures a monotonic byte-stream server.
type SequenceConfig struct {
	// ChunkSize is how many bytes each outbound message carries.
	ChunkSize int

	// Passed through to the underlying session mock.
	Grace            time.Duration
	RejectReattach   bool
	CleanCutDelay    time.Duration
	ReattachAckDelay time.Duration
}

// NewSequenceWSS starts a session-aware mock whose outbound stream is a monotonic sequence
// that resumes at the exact next index across a reattach, and records inbound bytes in
// order. One inbound message triggers one outbound chunk, so the test drives the stream.
func NewSequenceWSS(t *testing.T, cfg SequenceConfig) *SessionMockWSS {
	t.Helper()
	chunkSize := cfg.ChunkSize
	if chunkSize <= 0 {
		chunkSize = 64
	}

	return NewSessionMockWSS(t, SessionMockConfig{
		Grace:            cfg.Grace,
		RejectReattach:   cfg.RejectReattach,
		CleanCutDelay:    cfg.CleanCutDelay,
		ReattachAckDelay: cfg.ReattachAckDelay,
		OnMessage: func(session *MockSession, _ []byte) []byte {
			// SentCount is the resume point: it counts across every websocket of this
			// session, so the successor continues the sequence rather than restarting.
			start := session.SentCount()
			chunk := make([]byte, chunkSize)
			for i := range chunk {
				chunk[i] = byte((start + i) % 251)
			}
			return chunk
		},
	})
}

// ExpectedSequence returns the first n bytes NewSequenceWSS emits, to compare a
// received stream against.
func ExpectedSequence(n int) []byte {
	expected := make([]byte, n)
	for i := range expected {
		expected[i] = byte(i % 251)
	}
	return expected
}

// receive reads one whole binary message.
func receive(ws *websocket.Conn) ([]byte, error) {
	var message []byte
	err := websocket.Message.Receive(ws, &message)
	if err != nil {
		return nil, err
	}
	return message, nil
}
