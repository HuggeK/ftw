package appuplink

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// fakeRelay is the routing half of srcfl/ftw-webapp relay/src/server.ts: one
// box uplink and up to four browser streams meet under a handle, binary
// messages cross between them, and the relay says two words.
//
// It is a fake rather than the real server because the real one is TypeScript;
// the parts that matter to this package — the join path, the two control
// words and the close codes — are copied, not invented, and
// relay_contract_test.go pins them to the constants the app compiled against.
type fakeRelay struct {
	server *httptest.Server

	// wmu serialises writes. A websocket takes one writer at a time, and a
	// stream joining or leaving writes to the uplink beside its own reader.
	wmu sync.Mutex

	mu      sync.Mutex
	epoch   int64
	uplink  *websocket.Conn
	streams []*websocket.Conn
	handles []string
	// rotateTo, when set, closes the next accepted uplink with 4410 and this
	// epoch, which is what the relay does when the hour turns.
	rotateTo *int64
	// refuseEpoch, when set, closes with 4409 and this epoch, which is what
	// the relay does when a box guessed wrong.
	refuseEpoch *int64

	// The dead man's switch half of the contract: rows keyed on id, and
	// the claims heard on the uplink socket, in order.
	deadmanRows   map[string]deadmanRowWire
	deadmanClaims []string

	joined  chan struct{}
	deadman chan struct{}

	// The exchange half: the box's second socket, every answer it writes,
	// and how many times it has joined.
	exchange       *websocket.Conn
	answers        chan []byte
	exchangeJoins  int
	exchangeJoined chan struct{}
}

// deadmanRowWire is the POST /deadman body, as the contract writes it.
type deadmanRowWire struct {
	ID        string `json:"id"`
	Endpoint  string `json:"endpoint"`
	CT        string `json:"ct"`
	Auth      string `json:"auth"`
	DeadlineS int    `json:"deadline_s"`
}

func newFakeRelay(epoch int64) *fakeRelay {
	r := &fakeRelay{
		epoch:   epoch,
		joined:  make(chan struct{}, 16),
		deadman: make(chan struct{}, 64),
		answers: make(chan []byte, 64),

		exchangeJoined: make(chan struct{}, 16),
		deadmanRows:    map[string]deadmanRowWire{},
	}
	r.server = httptest.NewServer(http.HandlerFunc(r.handle))
	return r
}

func (r *fakeRelay) url() string {
	return "ws" + strings.TrimPrefix(r.server.URL, "http")
}

func (r *fakeRelay) close() { r.server.Close() }

func (r *fakeRelay) handle(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/deadman" || strings.HasPrefix(req.URL.Path, "/deadman/") {
		r.handleDeadman(w, req)
		return
	}
	parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "r" {
		http.Error(w, "join", http.StatusBadRequest)
		return
	}
	epoch, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		http.Error(w, "join", http.StatusBadRequest)
		return
	}
	handle, role := parts[2], parts[3]

	upgrader := websocket.Upgrader{}
	conn, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		return
	}

	r.mu.Lock()
	r.handles = append(r.handles, handle)
	refuse, rotate, want := r.refuseEpoch, r.rotateTo, r.epoch
	r.mu.Unlock()

	select {
	case r.joined <- struct{}{}:
	default:
	}

	switch {
	case refuse != nil:
		r.mu.Lock()
		r.refuseEpoch = nil
		r.mu.Unlock()
		closeWith(conn, CloseEpoch, strconv.FormatInt(*refuse, 10))
		return
	case rotate != nil:
		r.mu.Lock()
		r.rotateTo = nil
		r.epoch = *rotate
		r.mu.Unlock()
		closeWith(conn, CloseRotated, strconv.FormatInt(*rotate, 10))
		return
	case epoch != want:
		closeWith(conn, CloseEpoch, strconv.FormatInt(want, 10))
		return
	}

	switch role {
	case "box":
		r.serveUplink(conn)
	case "exchange":
		r.serveExchange(conn)
	default:
		r.serveStream(conn)
	}
}

func (r *fakeRelay) serveUplink(conn *websocket.Conn) {
	r.mu.Lock()
	r.uplink = conn
	streams := append([]*websocket.Conn(nil), r.streams...)
	r.mu.Unlock()

	// The relay's only two words. Both sides hear them, which is how a peer
	// knows there is anybody to talk to.
	for _, s := range streams {
		r.write(conn, websocket.TextMessage, []byte(CtrlReady))
		r.write(s, websocket.TextMessage, []byte(CtrlReady))
	}

	defer func() {
		r.mu.Lock()
		if r.uplink == conn {
			r.uplink = nil
		}
		r.mu.Unlock()
		_ = conn.Close()
	}()

	for {
		kind, message, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if kind != websocket.BinaryMessage {
			// The deadman-aware relay accepts exactly one text word from
			// the box: "deadman <id>" claims the id on this socket.
			// Anything else remains a protocol breach and drops the
			// connection, as before.
			word := string(message)
			if kind == websocket.TextMessage && strings.HasPrefix(word, CtrlDeadman+" ") {
				r.mu.Lock()
				r.deadmanClaims = append(r.deadmanClaims, strings.TrimPrefix(word, CtrlDeadman+" "))
				r.mu.Unlock()
				select {
				case r.deadman <- struct{}{}:
				default:
				}
				continue
			}
			return
		}
		// Broadcast, because the relay cannot tell which stream a ciphertext
		// belongs to without reading it.
		r.mu.Lock()
		streams := append([]*websocket.Conn(nil), r.streams...)
		r.mu.Unlock()
		for _, s := range streams {
			r.write(s, websocket.BinaryMessage, message)
		}
	}
}

func (r *fakeRelay) serveStream(conn *websocket.Conn) {
	r.mu.Lock()
	r.streams = append(r.streams, conn)
	uplink := r.uplink
	r.mu.Unlock()

	if uplink != nil {
		r.write(uplink, websocket.TextMessage, []byte(CtrlReady))
		r.write(conn, websocket.TextMessage, []byte(CtrlReady))
	}

	defer func() {
		r.mu.Lock()
		for i, s := range r.streams {
			if s == conn {
				r.streams = append(r.streams[:i], r.streams[i+1:]...)
				break
			}
		}
		empty := len(r.streams) == 0
		up := r.uplink
		r.mu.Unlock()
		if empty && up != nil {
			r.write(up, websocket.TextMessage, []byte(CtrlGone))
		}
		_ = conn.Close()
	}()

	for {
		kind, message, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if kind != websocket.BinaryMessage {
			return
		}
		r.mu.Lock()
		up := r.uplink
		r.mu.Unlock()
		if up != nil {
			r.write(up, websocket.BinaryMessage, message)
		}
	}
}

// serveExchange holds the box's exchange socket and keeps every answer it
// writes. A test plays the watch's side with post.
func (r *fakeRelay) serveExchange(conn *websocket.Conn) {
	r.mu.Lock()
	r.exchange = conn
	r.exchangeJoins++
	r.mu.Unlock()
	select {
	case r.exchangeJoined <- struct{}{}:
	default:
	}
	defer func() {
		r.mu.Lock()
		if r.exchange == conn {
			r.exchange = nil
		}
		r.mu.Unlock()
		_ = conn.Close()
	}()
	for {
		kind, message, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if kind == websocket.BinaryMessage {
			r.answers <- message
		}
	}
}

// post is the relay's /x/ route in one call: an id of its own in front of the
// body, down the exchange socket, and the answer that comes back behind the
// same id. Nil when the box stays silent.
func (r *fakeRelay) post(id byte, body []byte, wait time.Duration) []byte {
	r.mu.Lock()
	conn := r.exchange
	r.mu.Unlock()
	if conn == nil {
		return nil
	}
	tag := bytes.Repeat([]byte{id}, 16)
	r.write(conn, websocket.BinaryMessage, append(append([]byte(nil), tag...), body...))
	deadline := time.After(wait)
	for {
		select {
		case answer := <-r.answers:
			if bytes.Equal(answer[:16], tag) {
				return answer[16:]
			}
		case <-deadline:
			return nil
		}
	}
}

// dropExchange closes the box's exchange socket from the relay's side.
func (r *fakeRelay) dropExchange() {
	r.mu.Lock()
	conn := r.exchange
	r.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (r *fakeRelay) write(conn *websocket.Conn, kind int, message []byte) {
	r.wmu.Lock()
	defer r.wmu.Unlock()
	_ = conn.WriteMessage(kind, message)
}

func (r *fakeRelay) seenHandles() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.handles...)
}

// handleDeadman is the relay's HTTP half of the dead man's switch contract:
// POST /deadman upserts a row on id, DELETE /deadman/<id> forgets one,
// idempotently. 204 both ways. A row without a VAPID auth header is refused
// the way the real relay refuses it: the subscription is bound to the box's
// key, so a row the relay could never deliver must not arm the switch.
func (r *fakeRelay) handleDeadman(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case http.MethodPost:
		var row deadmanRowWire
		if err := json.NewDecoder(req.Body).Decode(&row); err != nil ||
			len(row.ID) != 32 || row.Endpoint == "" || row.CT == "" ||
			!strings.HasPrefix(row.Auth, "vapid t=") ||
			row.DeadlineS < 60 || row.DeadlineS > 86400 {
			http.Error(w, "bad row", http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		r.deadmanRows[row.ID] = row
		r.mu.Unlock()
		select {
		case r.deadman <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		id := strings.TrimPrefix(req.URL.Path, "/deadman/")
		r.mu.Lock()
		delete(r.deadmanRows, id)
		r.mu.Unlock()
		select {
		case r.deadman <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method", http.StatusMethodNotAllowed)
	}
}

func (r *fakeRelay) deadmanState() (rows map[string]deadmanRowWire, claims []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rows = make(map[string]deadmanRowWire, len(r.deadmanRows))
	for id, row := range r.deadmanRows {
		rows[id] = row
	}
	return rows, append([]string(nil), r.deadmanClaims...)
}

func closeWith(conn *websocket.Conn, code int, reason string) {
	_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason))
	_ = conn.Close()
}
