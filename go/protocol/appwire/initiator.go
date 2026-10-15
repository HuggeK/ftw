package appwire

import (
	"fmt"
	"sync"
)

// Noise_IK_25519_ChaChaPoly_SHA256, initiator half.
//
// The box is always a responder, so Core never runs this. It exists for the
// other side of FTW Cloud's app session, where the service is the responder
// and a phone the initiator: the service's tests and any Go client speak to
// it with this. Verified against the same Cacophony vector as the responder.

// InitiatorOptions configures the initiator.
type InitiatorOptions struct {
	// StaticKey is the initiator's long-term key.
	StaticKey KeyPair
	// RemoteStatic is the responder's static key, known before the first
	// byte: IK's pre-message.
	RemoteStatic []byte
	// Prologue is mixed in before anything else. Both ends must agree on it
	// byte for byte.
	Prologue []byte
	// Ephemeral fixes the ephemeral key. Test vectors only.
	Ephemeral *KeyPair
}

type initStep int

const (
	initWrite1 initStep = iota
	initRead2
	initDone
	initSplit
)

// InitiatorState runs the initiator side of IK. Drive it with WriteMessage,
// then ReadMessage, then Split.
type InitiatorState struct {
	mu   sync.Mutex
	sym  *symmetricState
	s    KeyPair
	e    KeyPair
	rs   []byte
	step initStep
}

// NewInitiator starts a handshake toward a responder whose static key is
// already known.
func NewInitiator(opts InitiatorOptions) (*InitiatorState, error) {
	if opts.StaticKey.priv == nil {
		return nil, fmt.Errorf("%w: initiator needs a static key", ErrNoiseKey)
	}
	if len(opts.RemoteStatic) != DHBytes {
		return nil, fmt.Errorf("%w: responder key is %d bytes, need %d", ErrNoiseKey, len(opts.RemoteStatic), DHBytes)
	}
	h := &InitiatorState{sym: newSymmetricState(), s: opts.StaticKey, rs: append([]byte{}, opts.RemoteStatic...)}
	if opts.Ephemeral != nil {
		h.e = *opts.Ephemeral
	}
	h.sym.mixHash(opts.Prologue)
	h.sym.mixHash(h.rs)
	return h, nil
}

// WriteMessage produces `-> e, es, s, ss` carrying payload.
func (h *InitiatorState) WriteMessage(payload []byte) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.step != initWrite1 {
		return nil, fmt.Errorf("%w: cannot write now", ErrNoiseState)
	}
	if h.e.priv == nil {
		e, err := GenerateKeyPair()
		if err != nil {
			return nil, err
		}
		h.e = e
	}
	out := h.e.Public()
	h.sym.mixHash(out)

	es, err := h.e.dh(h.rs)
	if err != nil {
		return nil, err
	}
	if err := h.sym.mixKey(es); err != nil {
		return nil, err
	}
	encStatic, err := h.sym.encryptAndHash(h.s.Public())
	if err != nil {
		return nil, err
	}
	ss, err := h.s.dh(h.rs)
	if err != nil {
		return nil, err
	}
	if err := h.sym.mixKey(ss); err != nil {
		return nil, err
	}
	encPayload, err := h.sym.encryptAndHash(payload)
	if err != nil {
		return nil, err
	}
	h.step = initRead2
	out = append(out, encStatic...)
	return append(out, encPayload...), nil
}

// ReadMessage consumes `<- e, ee, se` and returns its payload.
func (h *InitiatorState) ReadMessage(message []byte) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.step != initRead2 {
		return nil, fmt.Errorf("%w: cannot read now", ErrNoiseState)
	}
	if len(message) < DHBytes+TagBytes {
		return nil, fmt.Errorf("%w: handshake message 2 is %d bytes", ErrNoiseMessage, len(message))
	}
	re := append([]byte{}, message[:DHBytes]...)
	h.sym.mixHash(re)
	ee, err := h.e.dh(re)
	if err != nil {
		return nil, err
	}
	if err := h.sym.mixKey(ee); err != nil {
		return nil, err
	}
	se, err := h.s.dh(re)
	if err != nil {
		return nil, err
	}
	if err := h.sym.mixKey(se); err != nil {
		return nil, err
	}
	payload, err := h.sym.decryptAndHash(message[DHBytes:])
	if err != nil {
		return nil, err
	}
	h.step = initDone
	return payload, nil
}

// Split ends the handshake and hands over the transport keys. Like the
// responder's, it works once.
func (h *InitiatorState) Split() (*Session, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.step == initSplit {
		return nil, fmt.Errorf("%w: handshake already split", ErrNoiseState)
	}
	if h.step != initDone {
		return nil, fmt.Errorf("%w: handshake is not finished", ErrNoiseState)
	}
	keys, err := h.sym.split()
	if err != nil {
		return nil, err
	}
	h.step = initSplit
	return &Session{
		Send:          keys[0],
		Recv:          keys[1],
		HandshakeHash: append([]byte{}, h.sym.h...),
		RemoteStatic:  append([]byte{}, h.rs...),
	}, nil
}
