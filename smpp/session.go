package smpp

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrTimeout = errors.New("smpp: timeout waiting for response")
	ErrClosed  = errors.New("smpp: session closed")
)

// TLV is an SMPP optional parameter.
type TLV struct {
	Tag   uint16
	Value []byte
}

// SubmitParams describes a single submit_sm.
type SubmitParams struct {
	ServiceType          string
	SrcTON, SrcNPI       byte
	SrcAddr              string
	DstTON, DstNPI       byte
	DstAddr              string
	ESMClass             byte
	ProtocolID           byte
	PriorityFlag         byte
	ScheduleDeliveryTime string
	ValidityPeriod       string
	RegisteredDelivery   byte
	ReplaceIfPresent     byte
	DataCoding           byte
	ShortMessage         []byte // payload incl. UDH; leave nil when using message_payload TLV
	TLVs                 []TLV
}

// SubmitResult carries the SMSC response for one submit_sm.
type SubmitResult struct {
	MessageID string
	Status    uint32
	Err       error
}

// MOHandler is called for inbound deliver_sm (mobile-originated / delivery
// receipts). src/dst are the addresses; text is the best-effort decoded body.
type MOHandler func(src, dst string, dataCoding byte, body []byte)

// Session is a single bound SMPP connection.
type Session struct {
	conn        net.Conn
	br          *bufio.Reader
	writeMu     sync.Mutex
	seq         uint32
	mu          sync.Mutex
	pending     map[uint32]chan pdu
	respTimeout time.Duration
	closed      chan struct{}
	closeOnce   sync.Once
	bound       BindMode

	OnMO  MOHandler
	Debug func(dir, summary string, raw []byte) // optional wire logger
}

// Dial opens a TCP connection to the SMSC.
func Dial(addr string, connectTimeout, respTimeout time.Duration) (*Session, error) {
	conn, err := net.DialTimeout("tcp", addr, connectTimeout)
	if err != nil {
		return nil, err
	}
	s := &Session{
		conn:        conn,
		br:          bufio.NewReaderSize(conn, 64*1024),
		pending:     make(map[uint32]chan pdu),
		respTimeout: respTimeout,
		closed:      make(chan struct{}),
	}
	go s.readLoop()
	return s, nil
}

func (s *Session) nextSeq() uint32 {
	for {
		v := atomic.AddUint32(&s.seq, 1) & 0x7FFFFFFF
		if v != 0 {
			return v
		}
	}
}

func (s *Session) writePDU(p pdu) error {
	raw := p.marshal()
	s.writeMu.Lock()
	err := func() error {
		if s.respTimeout > 0 {
			_ = s.conn.SetWriteDeadline(time.Now().Add(s.respTimeout))
		}
		_, e := s.conn.Write(raw)
		return e
	}()
	s.writeMu.Unlock()
	if s.Debug != nil {
		s.Debug("send", fmt.Sprintf("id=0x%08X status=0x%08X seq=%d len=%d", p.id, p.status, p.seq, len(raw)), raw)
	}
	return err
}

func (s *Session) readPDU() (pdu, error) {
	var lenb [4]byte
	if _, err := io.ReadFull(s.br, lenb[:]); err != nil {
		return pdu{}, err
	}
	total := binary.BigEndian.Uint32(lenb[:])
	if total < 16 || total > maxPDULen {
		return pdu{}, fmt.Errorf("smpp: invalid command_length %d", total)
	}
	rest := make([]byte, total-4)
	if _, err := io.ReadFull(s.br, rest); err != nil {
		return pdu{}, err
	}
	p := pdu{
		id:     binary.BigEndian.Uint32(rest[0:]),
		status: binary.BigEndian.Uint32(rest[4:]),
		seq:    binary.BigEndian.Uint32(rest[8:]),
		body:   rest[12:],
	}
	if s.Debug != nil {
		s.Debug("recv", fmt.Sprintf("id=0x%08X status=0x%08X seq=%d len=%d", p.id, p.status, p.seq, total), nil)
	}
	return p, nil
}

func (s *Session) readLoop() {
	for {
		p, err := s.readPDU()
		if err != nil {
			s.Close()
			return
		}
		switch {
		case p.id&0x80000000 != 0: // any *_resp or generic_nack
			s.route(p)
		case p.id == enquireLink:
			_ = s.writePDU(pdu{id: enquireLinkResp, seq: p.seq})
		case p.id == deliverSM:
			s.handleDeliver(p)
			_ = s.writePDU(pdu{id: deliverSMResp, seq: p.seq, body: []byte{0}})
		case p.id == unbind:
			_ = s.writePDU(pdu{id: unbindResp, seq: p.seq})
			s.Close()
			return
		default:
			_ = s.writePDU(pdu{id: genericNack, status: 0x00000003, seq: p.seq})
		}
	}
}

func (s *Session) route(p pdu) {
	s.mu.Lock()
	ch, ok := s.pending[p.seq]
	if ok {
		delete(s.pending, p.seq)
	}
	s.mu.Unlock()
	if ok {
		ch <- p // ch is buffered (cap 1)
	}
}

func (s *Session) clearPending(seq uint32) {
	s.mu.Lock()
	delete(s.pending, seq)
	s.mu.Unlock()
}

func (s *Session) register(seq uint32) chan pdu {
	ch := make(chan pdu, 1)
	s.mu.Lock()
	s.pending[seq] = ch
	s.mu.Unlock()
	return ch
}

// request sends a PDU and blocks for the matching response.
func (s *Session) request(id uint32, body []byte) (pdu, error) {
	seq := s.nextSeq()
	ch := s.register(seq)
	if err := s.writePDU(pdu{id: id, seq: seq, body: body}); err != nil {
		s.clearPending(seq)
		return pdu{}, err
	}
	select {
	case p := <-ch:
		return p, nil
	case <-time.After(s.respTimeout):
		s.clearPending(seq)
		return pdu{}, ErrTimeout
	case <-s.closed:
		s.clearPending(seq)
		return pdu{}, ErrClosed
	}
}

// Bind authenticates with the SMSC in the requested mode.
func (s *Session) Bind(mode BindMode, systemID, password, systemType string) error {
	bb := &bodyBuilder{}
	bb.cstr(systemID).cstr(password).cstr(systemType).
		u8(interfaceVersion34).
		u8(0).u8(0). // addr_ton, addr_npi
		cstr("")     // address_range
	resp, err := s.request(mode.commandID(), bb.bytes())
	if err != nil {
		return err
	}
	if resp.status != 0 {
		return fmt.Errorf("bind rejected: %s", statusName(resp.status))
	}
	s.bound = mode
	return nil
}

func buildSubmitBody(p SubmitParams) []byte {
	bb := &bodyBuilder{}
	bb.cstr(p.ServiceType).
		u8(p.SrcTON).u8(p.SrcNPI).cstr(p.SrcAddr).
		u8(p.DstTON).u8(p.DstNPI).cstr(p.DstAddr).
		u8(p.ESMClass).u8(p.ProtocolID).u8(p.PriorityFlag).
		cstr(p.ScheduleDeliveryTime).cstr(p.ValidityPeriod).
		u8(p.RegisteredDelivery).u8(p.ReplaceIfPresent).u8(p.DataCoding).
		u8(0). // sm_default_msg_id
		lenOctets(p.ShortMessage)
	for _, t := range p.TLVs {
		bb.tlv(t.Tag, t.Value)
	}
	return bb.bytes()
}

// Submit sends one submit_sm and blocks for its response.
func (s *Session) Submit(p SubmitParams) SubmitResult {
	resp, err := s.request(submitSM, buildSubmitBody(p))
	if err != nil {
		return SubmitResult{Err: err}
	}
	var id string
	if len(resp.body) > 0 {
		id, _, _ = readCString(resp.body, 0)
	}
	return SubmitResult{MessageID: id, Status: resp.status}
}

// SubmitAsync sends one submit_sm and returns a channel that yields the result.
// Use this with a window/semaphore for high-throughput sending.
func (s *Session) SubmitAsync(p SubmitParams) <-chan SubmitResult {
	out := make(chan SubmitResult, 1)
	seq := s.nextSeq()
	ch := s.register(seq)
	if err := s.writePDU(pdu{id: submitSM, seq: seq, body: buildSubmitBody(p)}); err != nil {
		s.clearPending(seq)
		out <- SubmitResult{Err: err}
		return out
	}
	go func() {
		select {
		case resp := <-ch:
			var id string
			if len(resp.body) > 0 {
				id, _, _ = readCString(resp.body, 0)
			}
			out <- SubmitResult{MessageID: id, Status: resp.status}
		case <-time.After(s.respTimeout):
			s.clearPending(seq)
			out <- SubmitResult{Err: ErrTimeout}
		case <-s.closed:
			s.clearPending(seq)
			out <- SubmitResult{Err: ErrClosed}
		}
	}()
	return out
}

func (s *Session) handleDeliver(p pdu) {
	if s.OnMO == nil {
		return
	}
	// Parse the deliver_sm body just enough to surface src/dst/body.
	off := 0
	var err error
	_, off, err = readCString(p.body, off) // service_type
	if err != nil || off+3 > len(p.body) {
		return
	}
	off += 2 // source_addr_ton, source_addr_npi
	var src string
	if src, off, err = readCString(p.body, off); err != nil {
		return
	}
	if off+3 > len(p.body) {
		return
	}
	off += 2 // dest_addr_ton, dest_addr_npi
	var dst string
	if dst, off, err = readCString(p.body, off); err != nil {
		return
	}
	// esm_class, protocol_id, priority_flag, sched(C), valid(C),
	// reg_delivery, replace, data_coding, sm_default_msg_id, sm_length
	if off+1 > len(p.body) {
		return
	}
	off++                                                   // esm_class
	off += 2                                                // protocol_id, priority_flag
	if _, off, err = readCString(p.body, off); err != nil { // schedule
		return
	}
	if _, off, err = readCString(p.body, off); err != nil { // validity
		return
	}
	if off+3 > len(p.body) {
		return
	}
	off += 2 // reg_delivery, replace_if_present
	dc := p.body[off]
	off++
	off++ // sm_default_msg_id
	if off >= len(p.body) {
		return
	}
	smLen := int(p.body[off])
	off++
	if off+smLen > len(p.body) {
		smLen = len(p.body) - off
	}
	body := p.body[off : off+smLen]
	s.OnMO(src, dst, dc, body)
}

// EnquireLink sends a keepalive and waits for the response.
func (s *Session) EnquireLink() error {
	resp, err := s.request(enquireLink, nil)
	if err != nil {
		return err
	}
	if resp.status != 0 {
		return fmt.Errorf("enquire_link: %s", statusName(resp.status))
	}
	return nil
}

// StartKeepAlive periodically sends enquire_link until the session closes.
func (s *Session) StartKeepAlive(interval time.Duration) {
	if interval <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if err := s.EnquireLink(); err != nil {
					return
				}
			case <-s.closed:
				return
			}
		}
	}()
}

// Unbind gracefully unbinds and closes the session.
func (s *Session) Unbind() error {
	_, err := s.request(unbind, nil)
	s.Close()
	return err
}

// Close terminates the connection. Safe to call multiple times.
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		close(s.closed)
		_ = s.conn.Close()
	})
}

// Done returns a channel closed when the session ends.
func (s *Session) Done() <-chan struct{} { return s.closed }
