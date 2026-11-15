// Package native is the WebSocket protocol of RFC 6455 without its
// connection: bytes that arrive are given to a Protocol, which turns them
// into events and into the bytes to answer with, and messages to send are
// made into frames. The Alchem module reads and writes the socket.
package native

import (
	"bytes"
	"compress/flate"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// guid is the constant RFC 6455 joins to a client's key to prove that a
// server understood the handshake.
const guid = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// DefaultLimit bounds a message unless max_message_bytes: says otherwise.
const DefaultLimit = 16 << 20

// Compression is permessage-deflate, RFC 7692, with each message compressed
// on its own: neither end carries a window from one message to the next, so
// a connection holds no memory for it between messages.
const (
	deflateOffer = "permessage-deflate; server_no_context_takeover; client_no_context_takeover"
	// deflateFrom is the size from which a message is compressed; deflate
	// does little for a shorter one.
	deflateFrom = 256
	// deflateTail ends each compressed message: the sender leaves it off and
	// the receiver puts it back, followed by an empty last block.
	deflateTail = "\x00\x00\xff\xff"
)

// Event is what a socket gives its reader. Kind is text, binary, closed or
// error; Text is the message, the reason of a close or the failure, and Code
// the code of a close.
type Event struct {
	Kind string
	Text string
	Code int64
}

// failure is a broken rule of the protocol, with the close code that
// reports it.
type failure struct {
	code int
	text string
}

func (f failure) Error() string { return f.text }

// Protocol is one end of a WebSocket. It may be used by several goroutines
// at once.
type Protocol struct {
	mu      sync.Mutex
	client  bool
	deflate bool
	limit   int64
	// pending holds bytes that do not yet make a frame, and message the
	// frames so far of a message in several.
	pending []byte
	message []byte
	kind    byte
	packed  bool
	// replies are frames to write: pongs and the answer to a close. events
	// wait to be taken.
	replies []string
	events  []Event
	// closing is set once a close frame has been made to send, shutting once
	// a caller has taken on closing from this end, closed once it has, and
	// ended once the last event has been queued.
	closing  bool
	shutting bool
	closed   bool
	ended    bool
	// active counts the events still to be sent as messages: -1 for all of
	// them, 0 when the socket is passive. pumping is set while a process
	// sends them.
	active  int64
	pumping bool
}

// New makes the protocol of a connection whose handshake is done.
func New(client bool, limit int64, deflate bool) *Protocol {
	if limit <= 0 {
		limit = DefaultLimit
	}
	return &Protocol{client: client, limit: limit, deflate: deflate}
}

// Compressed reports whether the handshake agreed to compress messages.
func (p *Protocol) Compressed() bool { return p.deflate }

// Feed takes bytes read from the connection, and reports whether the last
// event has now been read.
func (p *Protocol) Feed(data string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ended {
		return true
	}
	p.pending = append(p.pending, data...)
	for !p.ended {
		fin, op, payload, used, err := p.frame()
		if err != nil {
			p.broken(err)
			return true
		}
		if used == 0 {
			return false
		}
		p.pending = p.pending[used:]
		if len(p.pending) == 0 {
			p.pending = nil
		}
		if err := p.accept(fin, op, payload); err != nil {
			p.broken(err)
		}
	}
	return true
}

// packedBit marks, in what frame gives as the opcode, a message that is
// compressed.
const packedBit = 0x40

// frame reads one frame from the pending bytes, unmasking its payload, and
// reports how many bytes it took: none when the frame has not all arrived.
func (p *Protocol) frame() (fin bool, op byte, payload []byte, used int, err error) {
	data := p.pending
	if len(data) < 2 {
		return
	}
	fin, op = data[0]&0x80 != 0, data[0]&0x0f
	packed := data[0]&0x40 != 0 && p.deflate && (op == 0x1 || op == 0x2)
	if data[0]&0x30 != 0 || data[0]&0x40 != 0 && !packed {
		return false, 0, nil, 0, failure{1002, "protocol error: reserved bits are set"}
	}
	masked := data[1]&0x80 != 0
	if masked == p.client {
		if p.client {
			return false, 0, nil, 0, failure{1002, "protocol error: the server masked a frame"}
		}
		return false, 0, nil, 0, failure{1002, "protocol error: the client sent an unmasked frame"}
	}
	size, at := uint64(data[1]&0x7f), 2
	if width := map[uint64]int{126: 2, 127: 8}[size]; width > 0 {
		if len(data) < at+width {
			return false, 0, nil, 0, nil
		}
		size = 0
		for _, b := range data[at : at+width] {
			size = size<<8 | uint64(b)
		}
		at += width
	}
	if op >= 8 && (!fin || size > 125) {
		return false, 0, nil, 0, failure{1002, "protocol error: a control frame is fragmented or too long"}
	}
	if size > uint64(p.limit) {
		return false, 0, nil, 0, failure{1009, "message too large"}
	}
	var mask []byte
	if masked {
		if len(data) < at+4 {
			return false, 0, nil, 0, nil
		}
		mask = data[at : at+4]
		at += 4
	}
	if uint64(len(data)-at) < size {
		return false, 0, nil, 0, nil
	}
	payload = make([]byte, size)
	copy(payload, data[at:])
	for i := range payload {
		if masked {
			payload[i] ^= mask[i%4]
		}
	}
	if packed {
		op |= packedBit
	}
	return fin, op, payload, at + int(size), nil
}

// accept acts on one frame: it answers a ping, ends on a close, and queues a
// message once its last frame has come.
func (p *Protocol) accept(fin bool, op byte, payload []byte) error {
	if op&packedBit != 0 {
		op, p.packed = op&^packedBit, p.kind == 0
	}
	switch op {
	case 0x8:
		return p.peerClosed(payload)
	case 0x9:
		p.replies = append(p.replies, p.encode(0xA, payload))
		return nil
	case 0xA:
		return nil
	case 0x1, 0x2:
		if p.kind != 0 {
			return failure{1002, "protocol error: a message began before the last one ended"}
		}
		p.kind, p.message = op, payload
	case 0x0:
		if p.kind == 0 {
			return failure{1002, "protocol error: a continuation frame without a message"}
		}
		p.message = append(p.message, payload...)
	default:
		return failure{1002, "protocol error: unknown opcode " + strconv.Itoa(int(op))}
	}
	if int64(len(p.message)) > p.limit {
		return failure{1009, "message too large"}
	}
	if !fin {
		return nil
	}
	message := p.message
	if p.packed {
		var err error
		if message, err = p.inflate(message); err != nil {
			return err
		}
		p.packed = false
	}
	if p.kind == 0x1 && !utf8.Valid(message) {
		return failure{1007, "a text message is not valid UTF-8"}
	}
	kind := "text"
	if p.kind == 0x2 {
		kind = "binary"
	}
	p.events = append(p.events, Event{Kind: kind, Text: string(message)})
	p.kind, p.message = 0, nil
	return nil
}

// peerClosed answers the other end's close frame and ends with its code and
// reason.
func (p *Protocol) peerClosed(payload []byte) error {
	code, reason := 1005, ""
	switch {
	case len(payload) == 1:
		return failure{1002, "protocol error: a close frame with a one-byte body"}
	case len(payload) >= 2:
		code, reason = int(binary.BigEndian.Uint16(payload)), string(payload[2:])
		valid := code >= 1000 && code < 5000 && code != 1004 && code != 1005 && code != 1006 && (code < 1012 || code >= 3000)
		if !valid || !utf8.ValidString(reason) {
			return failure{1002, "protocol error: an invalid close code or reason"}
		}
	}
	if !p.closing {
		p.closing = true
		echo := []byte{}
		if code != 1005 {
			echo = binary.BigEndian.AppendUint16(echo, uint16(code))
		}
		p.replies = append(p.replies, p.encode(0x8, echo))
	}
	p.finish(Event{Kind: "closed", Text: reason, Code: int64(code)})
	return nil
}

// broken ends after a broken rule, which is reported to the other end with
// its close code.
func (p *Protocol) broken(err error) {
	var broke failure
	if !errors.As(err, &broke) {
		broke = failure{1002, err.Error()}
	}
	if !p.closing {
		p.closing = true
		p.replies = append(p.replies, p.encode(0x8, append(binary.BigEndian.AppendUint16(nil, uint16(broke.code)), broke.text...)))
	}
	p.finish(Event{Kind: "error", Text: broke.text})
}

// finish queues the last event.
func (p *Protocol) finish(last Event) {
	p.events = append(p.events, last)
	p.ended, p.pending, p.message = true, nil, nil
}

// Lost is told that the connection ended, or failed with a problem, without
// a closing handshake. It reports whether that was news.
func (p *Protocol) Lost(problem string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ended {
		return false
	}
	if p.closed || problem == "" || problem == "closed" {
		p.finish(Event{Kind: "closed", Code: 1006})
		return true
	}
	p.finish(Event{Kind: "error", Text: problem})
	return true
}

// Reply takes the next frame to write in answer to what was read.
func (p *Protocol) Reply() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.replies) == 0 {
		return "", false
	}
	reply := p.replies[0]
	p.replies = p.replies[1:]
	return reply, true
}

// Take takes the next event.
func (p *Protocol) Take() (Event, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.events) == 0 {
		return Event{}, false
	}
	event := p.events[0]
	p.events = p.events[1:]
	return event, true
}

// Ended reports that the last event has been read from the connection,
// whether or not it has been taken.
func (p *Protocol) Ended() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ended
}

// Drained reports that the last event has been taken: there is nothing more
// to receive.
func (p *Protocol) Drained() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ended && len(p.events) == 0
}

// encode makes one frame, masked when this end is the client. An op with
// packedBit set carries a compressed message.
func (p *Protocol) encode(op byte, payload []byte) string {
	header := []byte{0x80 | op}
	mask := byte(0)
	if p.client {
		mask = 0x80
	}
	switch size := len(payload); {
	case size < 126:
		header = append(header, mask|byte(size))
	case size < 1<<16:
		header = binary.BigEndian.AppendUint16(append(header, mask|126), uint16(size))
	default:
		header = binary.BigEndian.AppendUint64(append(header, mask|127), uint64(size))
	}
	body := payload
	if p.client {
		key := make([]byte, 4)
		rand.Read(key)
		header = append(header, key...)
		body = make([]byte, len(payload))
		for i, b := range payload {
			body[i] = b ^ key[i%4]
		}
	}
	return string(append(header, body...))
}

// Message makes the frame of a text (op 1) or binary (op 2) message, or is
// an error once the socket is closing.
func (p *Protocol) Message(op int64, data string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closing {
		return "", errors.New("closed")
	}
	code, payload := byte(op), []byte(data)
	if p.deflate && len(payload) >= deflateFrom {
		// A message that does not shrink goes as it is.
		if small := deflate(payload); len(small) < len(payload) {
			code, payload = code|packedBit, small
		}
	}
	return p.encode(code, payload), nil
}

// Shut takes on closing the socket from this end. Of several callers at
// once the first is told to go on, with the close frame to send when none
// has been sent, and the rest are told that it is closed already.
func (p *Protocol) Shut(code int64, reason string) (Closing, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.shutting {
		return Closing{}, false
	}
	p.shutting = true
	if p.closing {
		return Closing{}, true
	}
	p.closing = true
	return Closing{Frame: p.encode(0x8, append(binary.BigEndian.AppendUint16(nil, uint16(code)), reason...)), Send: true}, true
}

// Closing is what the caller that closes a socket does first.
type Closing struct {
	Frame string
	Send  bool
}

// Closed is told that the connection has been closed from this end, and
// reports whether that was news.
func (p *Protocol) Closed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	news := !p.closed
	p.closed = true
	return news
}

// Activate sets how many events are sent as messages, and reports whether
// a process must be started to send them.
func (p *Protocol) Activate(count int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active = count
	if count == 0 || p.pumping {
		return false
	}
	p.pumping = true
	return true
}

// Active reports whether events are being sent as messages, in which case
// they are not there to be received.
func (p *Protocol) Active() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active != 0 || p.pumping
}

// Pumping is asked by the process sending events whether to go on.
func (p *Protocol) Pumping() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active == 0 {
		p.pumping = false
	}
	return p.pumping
}

// Sending settles the count before an event is delivered, so that a
// receiver given the last one can read passively at once, and reports
// whether it was the last.
func (p *Protocol) Sending(final bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active > 0 {
		p.active--
	}
	if final {
		p.active = 0
	}
	last := p.active == 0
	if last {
		p.pumping = false
	}
	return last
}

var deflaters = sync.Pool{New: func() any {
	writer, _ := flate.NewWriter(io.Discard, flate.BestSpeed)
	return writer
}}

// coders only shorten the codes of the bytes a message uses, which is what
// gains on text that does not repeat itself, such as a token.
var coders = sync.Pool{New: func() any {
	writer, _ := flate.NewWriter(io.Discard, flate.HuffmanOnly)
	return writer
}}

var inflaters = sync.Pool{New: func() any { return flate.NewReader(strings.NewReader("")) }}

// deflate compresses one message: by its repetitions, or when it has none
// worth having, by its bytes alone.
func deflate(payload []byte) []byte {
	pack := func(pool *sync.Pool) []byte {
		var out bytes.Buffer
		out.Grow(len(payload)/4 + 64)
		writer := pool.Get().(*flate.Writer)
		defer pool.Put(writer)
		writer.Reset(&out)
		writer.Write(payload)
		writer.Flush()
		return bytes.TrimSuffix(out.Bytes(), []byte(deflateTail))
	}
	packed := pack(&deflaters)
	if len(packed) >= len(payload) {
		packed = pack(&coders)
	}
	return packed
}

// inflate gives back a compressed message, which may not grow past the
// limit of a message.
func (p *Protocol) inflate(message []byte) ([]byte, error) {
	source := io.MultiReader(bytes.NewReader(message), strings.NewReader(deflateTail+"\x01"+deflateTail))
	reader := inflaters.Get().(io.ReadCloser)
	defer inflaters.Put(reader)
	reader.(flate.Resetter).Reset(source, nil)
	out, err := io.ReadAll(io.LimitReader(reader, p.limit+1))
	if err != nil {
		return nil, failure{1007, "a compressed message could not be read"}
	}
	if int64(len(out)) > p.limit {
		return nil, failure{1009, "message too large"}
	}
	return out, nil
}

// headers is a list of header lines by lower-case name.
type headers map[string][]string

func headersOf(names, values []string) headers {
	out := headers{}
	for i, name := range names {
		if i < len(values) {
			out[strings.ToLower(name)] = append(out[strings.ToLower(name)], values[i])
		}
	}
	return out
}

func (h headers) get(name string) string {
	if values := h[name]; len(values) > 0 {
		return values[0]
	}
	return ""
}

// has reports whether a comma-separated header holds a token.
func (h headers) has(name, token string) bool {
	for _, value := range h[name] {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// extensions parses Sec-WebSocket-Extensions: each extension with its
// parameters, in order.
func (h headers) extensions() []map[string]string {
	out := []map[string]string{}
	for _, value := range h["sec-websocket-extensions"] {
		for _, offer := range strings.Split(value, ",") {
			parts := strings.Split(offer, ";")
			extension := map[string]string{"": strings.ToLower(strings.TrimSpace(parts[0]))}
			for _, part := range parts[1:] {
				name, setting, _ := strings.Cut(strings.TrimSpace(part), "=")
				extension[strings.ToLower(strings.TrimSpace(name))] = strings.Trim(strings.TrimSpace(setting), "\"")
			}
			if extension[""] != "" {
				out = append(out, extension)
			}
		}
	}
	return out
}

// deflateOffered reports whether a client offered compression in a form
// this end can give: its own window cannot be made smaller than deflate's
// full one.
func (h headers) deflateOffered() bool {
offers:
	for _, offer := range h.extensions() {
		if offer[""] != "permessage-deflate" {
			continue
		}
		for name, setting := range offer {
			switch name {
			case "", "client_max_window_bits", "server_no_context_takeover", "client_no_context_takeover":
			case "server_max_window_bits":
				if setting != "15" {
					continue offers
				}
			default:
				continue offers
			}
		}
		return true
	}
	return false
}

// accept is the Sec-WebSocket-Accept value for a key.
func accept(key string) string {
	sum := sha1.Sum([]byte(key + guid))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// Answer is a server's side of the handshake: the response to write, and
// whether it agrees to compress.
type Answer struct {
	Response string
	Deflate  bool
}

// Accept checks a request for a WebSocket handshake and makes the response
// that accepts it.
func Accept(method string, names, values []string, compress bool) (Answer, error) {
	header := headersOf(names, values)
	key := header.get("sec-websocket-key")
	decoded, _ := base64.StdEncoding.DecodeString(key)
	switch {
	case method != "GET":
		return Answer{}, errors.New("not a WebSocket request: the method is " + method)
	case !header.has("connection", "upgrade") || !header.has("upgrade", "websocket"):
		return Answer{}, errors.New("not a WebSocket request: no upgrade to websocket")
	case header.get("sec-websocket-version") != "13":
		return Answer{}, errors.New("not a WebSocket request: the version must be 13")
	case len(decoded) != 16:
		return Answer{}, errors.New("not a WebSocket request: the key is missing or malformed")
	}
	deflate, extensions := compress && header.deflateOffered(), ""
	if deflate {
		extensions = "Sec-WebSocket-Extensions: " + deflateOffer + "\r\n"
	}
	return Answer{"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept(key) + "\r\n" + extensions + "\r\n", deflate}, nil
}

// Offer is a client's side of the handshake: where to connect, whether with
// TLS and to which name, the request to write and the key it carries.
type Offer struct {
	Address string
	Secure  bool
	Name    string
	Request string
	Key     string
}

// handshakeHeaders are the headers the handshake itself sets.
var handshakeHeaders = map[string]bool{"host": true, "upgrade": true, "connection": true, "sec-websocket-key": true, "sec-websocket-version": true, "sec-websocket-extensions": true}

// Ask makes the request that opens a WebSocket to a ws:// or wss:// address.
// The headers are sent with it, as a browser sends Cookie or a client
// Authorization.
func Ask(address, origin string, compress bool, names, values []string) (Offer, error) {
	extra := ""
	for i, name := range names {
		value := values[i]
		if name == "" || strings.ContainsAny(name, " \t\r\n:") || strings.ContainsAny(value, "\r\n") {
			return Offer{}, errors.New("invalid header " + strconv.Quote(name))
		}
		if handshakeHeaders[strings.ToLower(name)] {
			return Offer{}, errors.New("the header " + name + " is set by the WebSocket handshake")
		}
		extra += name + ": " + value + "\r\n"
	}
	target, err := url.Parse(address)
	if err != nil {
		return Offer{}, err
	}
	offer := Offer{Name: target.Hostname()}
	switch target.Scheme {
	case "ws":
	case "wss":
		offer.Secure = true
	default:
		return Offer{}, errors.New("a WebSocket address starts with ws:// or wss://")
	}
	offer.Address = target.Host
	if target.Port() == "" {
		offer.Address = net.JoinHostPort(target.Hostname(), map[bool]string{false: "80", true: "443"}[offer.Secure])
	}
	nonce := make([]byte, 16)
	rand.Read(nonce)
	offer.Key = base64.StdEncoding.EncodeToString(nonce)
	offer.Request = "GET " + target.RequestURI() + " HTTP/1.1\r\nHost: " + target.Host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: " + offer.Key + "\r\nSec-WebSocket-Version: 13\r\n"
	if origin != "" {
		offer.Request += "Origin: " + origin + "\r\n"
	}
	if compress {
		offer.Request += "Sec-WebSocket-Extensions: " + deflateOffer + "\r\n"
	}
	offer.Request += extra + "\r\n"
	return offer, nil
}

// Agreed checks a server's response to the handshake, given as its status
// line and header lines, and reports whether messages are compressed.
func Agreed(status string, lines []string, key string, compress bool) (bool, error) {
	var names, values []string
	for _, line := range lines {
		name, value, _ := strings.Cut(line, ":")
		names, values = append(names, strings.TrimSpace(name)), append(values, strings.TrimSpace(value))
	}
	header := headersOf(names, values)
	_, rest, _ := strings.Cut(status, " ")
	if !strings.HasPrefix(status, "HTTP/1.1 101") || !header.has("upgrade", "websocket") || header.get("sec-websocket-accept") != accept(key) {
		return false, errors.New("the server did not accept the WebSocket: " + rest)
	}
	// The server may use only what was offered: compression, with each of
	// its messages compressed on its own.
	deflate := false
	for _, extension := range header.extensions() {
		_, afresh := extension["server_no_context_takeover"]
		if extension[""] != "permessage-deflate" || !compress || deflate || !afresh {
			return false, errors.New("the server chose an extension that was not offered: " + extension[""])
		}
		deflate = true
	}
	return deflate, nil
}
