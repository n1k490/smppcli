// Command mocksmsc is a tiny, stdlib-only SMPP v3.4 server used to test smppcli
// locally. It is NOT a real SMSC: it accepts any bind, acknowledges every
// submit_sm with a generated message_id, decodes and prints the received text
// (so Georgian/UCS2 round-trips can be eyeballed), answers enquire_link, and
// optionally returns a delivery receipt as a deliver_sm when the client asked
// for one and is bound as receiver/transceiver.
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nika/smppcli/smpp"
)

// Command IDs (kept local so the mock is independent of smpp internals).
const (
	cmdBindReceiver        = 0x00000001
	cmdBindTransmitter     = 0x00000002
	cmdSubmitSM            = 0x00000004
	cmdDeliverSM           = 0x00000005
	cmdUnbind              = 0x00000006
	cmdBindTransceiver     = 0x00000009
	cmdEnquireLink         = 0x00000015
	cmdGenericNack         = 0x80000000
	cmdBindReceiverResp    = 0x80000001
	cmdBindTransmitterResp = 0x80000002
	cmdSubmitSMResp        = 0x80000004
	cmdDeliverSMResp       = 0x80000005
	cmdUnbindResp          = 0x80000006
	cmdBindTransceiverResp = 0x80000009
	cmdEnquireLinkResp     = 0x80000015
)

const tagMessagePayload = 0x0424

var (
	msgCounter uint64
	verbose    bool
)

func main() {
	addr := flag.String("addr", "127.0.0.1:2775", "listen address host:port")
	dlr := flag.Bool("dlr", false, "send a delivery receipt for every registered submit (RX/TRX binds only)")
	dlrDelay := flag.Duration("dlr-delay", 500*time.Millisecond, "delay before sending a delivery receipt")
	flag.BoolVar(&verbose, "verbose", false, "log raw PDU hex dumps")
	flag.Parse()

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen %s: %v", *addr, err)
	}
	log.Printf("mock SMSC listening on %s (dlr=%v)", *addr, *dlr)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go handle(conn, *dlr, *dlrDelay)
	}
}

// pdu is the decoded form of one SMPP message.
type pdu struct {
	id     uint32
	status uint32
	seq    uint32
	body   []byte
}

type session struct {
	conn    net.Conn
	w       *bufio.Writer
	mu      sync.Mutex // serialises writes (server may push DLRs concurrently)
	bound   bool
	canRecv bool // true for receiver/transceiver binds (eligible for DLRs)
	remote  string
}

func handle(conn net.Conn, sendDLR bool, dlrDelay time.Duration) {
	defer conn.Close()
	s := &session{conn: conn, w: bufio.NewWriter(conn), remote: conn.RemoteAddr().String()}
	r := bufio.NewReader(conn)
	log.Printf("[%s] connected", s.remote)

	for {
		p, err := readPDU(r)
		if err != nil {
			if err != io.EOF {
				log.Printf("[%s] read: %v", s.remote, err)
			}
			log.Printf("[%s] disconnected", s.remote)
			return
		}
		if verbose {
			log.Printf("[%s] <- id=0x%08X seq=%d body=%s", s.remote, p.id, p.seq, hex.EncodeToString(p.body))
		}
		if err := s.dispatch(p, sendDLR, dlrDelay); err != nil {
			log.Printf("[%s] dispatch: %v", s.remote, err)
			return
		}
	}
}

func (s *session) dispatch(p pdu, sendDLR bool, dlrDelay time.Duration) error {
	switch p.id {
	case cmdBindTransmitter, cmdBindReceiver, cmdBindTransceiver:
		sysID, _, _ := readCString(p.body, 0)
		s.bound = true
		s.canRecv = p.id == cmdBindReceiver || p.id == cmdBindTransceiver
		mode := map[uint32]string{
			cmdBindTransmitter: "transmitter",
			cmdBindReceiver:    "receiver",
			cmdBindTransceiver: "transceiver",
		}[p.id]
		log.Printf("[%s] bind %s system_id=%q", s.remote, mode, sysID)
		respID := map[uint32]uint32{
			cmdBindTransmitter: cmdBindTransmitterResp,
			cmdBindReceiver:    cmdBindReceiverResp,
			cmdBindTransceiver: cmdBindTransceiverResp,
		}[p.id]
		// Body: system_id C-Octet String + sc_interface_version TLV (0x0210).
		body := append([]byte("MockSMSC"), 0)
		body = append(body, 0x02, 0x10, 0x00, 0x01, 0x34)
		return s.write(respID, 0, p.seq, body)

	case cmdSubmitSM:
		mid, err := s.handleSubmit(p, sendDLR, dlrDelay)
		if err != nil {
			log.Printf("[%s] submit parse error: %v", s.remote, err)
			return s.write(cmdSubmitSMResp, 0x00000045 /*ESME_RSUBMITFAIL*/, p.seq, []byte{0})
		}
		// submit_sm_resp body: message_id C-Octet String.
		body := append([]byte(mid), 0)
		return s.write(cmdSubmitSMResp, 0, p.seq, body)

	case cmdEnquireLink:
		return s.write(cmdEnquireLinkResp, 0, p.seq, nil)

	case cmdUnbind:
		log.Printf("[%s] unbind", s.remote)
		if err := s.write(cmdUnbindResp, 0, p.seq, nil); err != nil {
			return err
		}
		return io.EOF // close after unbind

	case cmdDeliverSM + 0x80000000: // deliver_sm_resp from client (to our DLR)
		return nil

	default:
		log.Printf("[%s] unhandled command 0x%08X", s.remote, p.id)
		return s.write(cmdGenericNack, 0x00000003 /*ESME_RINVCMDID*/, p.seq, nil)
	}
}

// submitFields holds the parts of a submit_sm we care about for display.
type submitFields struct {
	src, dst   string
	esmClass   byte
	dataCoding byte
	registered byte
	shortMsg   []byte
	payload    []byte // message_payload TLV value, if present
}

func (s *session) handleSubmit(p pdu, sendDLR bool, dlrDelay time.Duration) (string, error) {
	f, err := parseSubmit(p.body)
	if err != nil {
		return "", err
	}

	// Pick the message bytes: short_message, or message_payload TLV if used.
	raw := f.shortMsg
	source := "short_message"
	if len(raw) == 0 && len(f.payload) > 0 {
		raw = f.payload
		source = "message_payload"
	}

	// Strip UDH if the esm_class user-data-header bit (0x40) is set.
	udh := []byte(nil)
	text := raw
	if f.esmClass&0x40 != 0 && len(raw) > 0 {
		udhl := int(raw[0])
		if 1+udhl <= len(raw) {
			udh = raw[:1+udhl]
			text = raw[1+udhl:]
		}
	}

	decoded := smpp.DecodeMessage(f.dataCoding, text)
	id := atomic.AddUint64(&msgCounter, 1)
	mid := fmt.Sprintf("%010d", id)

	udhDesc := "none"
	if len(udh) > 0 {
		udhDesc = hex.EncodeToString(udh)
	}
	log.Printf("[%s] submit_sm #%s  %s->%s  coding=%s reg=%d via=%s udh=%s\n        text=%q",
		s.remote, mid, f.src, f.dst, codingName(f.dataCoding), f.registered, source, udhDesc, decoded)

	// Optionally push a delivery receipt back.
	if sendDLR && s.canRecv && f.registered&0x01 != 0 {
		go s.sendReceipt(mid, f.src, f.dst, decoded, dlrDelay)
	}
	return mid, nil
}

// sendReceipt pushes a deliver_sm carrying a textbook delivery-receipt body.
func (s *session) sendReceipt(mid, src, dst, text string, delay time.Duration) {
	time.Sleep(delay)
	now := time.Now().Format("0601021504")
	sample := text
	if r := []rune(sample); len(r) > 20 {
		sample = string(r[:20])
	}
	receipt := fmt.Sprintf("id:%s sub:001 dlvrd:001 submit date:%s done date:%s stat:DELIVRD err:000 text:%s",
		mid, now, now, sample)

	var b []byte
	b = append(b, 0)                  // service_type
	b = append(b, 0, 0)               // src ton/npi
	b = append(b, []byte(dst)...)     // source = original dest
	b = append(b, 0)                  //   (C-Octet terminator)
	b = append(b, 1, 1)               // dest ton/npi
	b = append(b, []byte(src)...)     // dest = original source
	b = append(b, 0)                  //   (C-Octet terminator)
	b = append(b, 0x04)               // esm_class: delivery receipt
	b = append(b, 0, 0)               // protocol_id, priority
	b = append(b, 0)                  // schedule (empty C-Octet)
	b = append(b, 0)                  // validity (empty C-Octet)
	b = append(b, 0)                  // registered_delivery
	b = append(b, 0)                  // replace_if_present
	b = append(b, 0)                  // data_coding (default)
	b = append(b, 0)                  // sm_default_msg_id
	b = append(b, byte(len(receipt))) // sm_length
	b = append(b, []byte(receipt)...) // short_message

	seq := uint32(0xF0000000) | uint32(atomic.AddUint64(&msgCounter, 1)&0x0FFFFFFF)
	if err := s.write(cmdDeliverSM, 0, seq, b); err != nil {
		log.Printf("[%s] send receipt: %v", s.remote, err)
		return
	}
	log.Printf("[%s] -> deliver_sm DLR for #%s", s.remote, mid)
}

func parseSubmit(b []byte) (submitFields, error) {
	var f submitFields
	pos := 0
	var ok bool

	_, pos, ok = readCString(b, pos) // service_type
	if !ok {
		return f, fmt.Errorf("service_type")
	}
	if pos+2 > len(b) {
		return f, fmt.Errorf("source ton/npi")
	}
	pos += 2 // source ton/npi
	f.src, pos, ok = readCString(b, pos)
	if !ok {
		return f, fmt.Errorf("source_addr")
	}
	if pos+2 > len(b) {
		return f, fmt.Errorf("dest ton/npi")
	}
	pos += 2 // dest ton/npi
	f.dst, pos, ok = readCString(b, pos)
	if !ok {
		return f, fmt.Errorf("destination_addr")
	}
	// esm_class, protocol_id, priority_flag
	if pos+3 > len(b) {
		return f, fmt.Errorf("esm/proto/prio")
	}
	f.esmClass = b[pos]
	pos += 3
	_, pos, ok = readCString(b, pos) // schedule_delivery_time
	if !ok {
		return f, fmt.Errorf("schedule")
	}
	_, pos, ok = readCString(b, pos) // validity_period
	if !ok {
		return f, fmt.Errorf("validity")
	}
	// registered_delivery, replace_if_present, data_coding, sm_default_msg_id
	if pos+4 > len(b) {
		return f, fmt.Errorf("reg/replace/coding/default")
	}
	f.registered = b[pos]
	f.dataCoding = b[pos+2]
	pos += 4
	if pos >= len(b) {
		return f, fmt.Errorf("sm_length")
	}
	smLen := int(b[pos])
	pos++
	if pos+smLen > len(b) {
		return f, fmt.Errorf("short_message truncated")
	}
	f.shortMsg = b[pos : pos+smLen]
	pos += smLen

	// Optional TLVs.
	for pos+4 <= len(b) {
		tag := binary.BigEndian.Uint16(b[pos : pos+2])
		ln := int(binary.BigEndian.Uint16(b[pos+2 : pos+4]))
		pos += 4
		if pos+ln > len(b) {
			break
		}
		val := b[pos : pos+ln]
		pos += ln
		if tag == tagMessagePayload {
			f.payload = val
		}
	}
	return f, nil
}

func (s *session) write(id, status, seq uint32, body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 16 + len(body)
	hdr := make([]byte, 16)
	binary.BigEndian.PutUint32(hdr[0:], uint32(total))
	binary.BigEndian.PutUint32(hdr[4:], id)
	binary.BigEndian.PutUint32(hdr[8:], status)
	binary.BigEndian.PutUint32(hdr[12:], seq)
	if _, err := s.w.Write(hdr); err != nil {
		return err
	}
	if len(body) > 0 {
		if _, err := s.w.Write(body); err != nil {
			return err
		}
	}
	if verbose {
		log.Printf("[%s] -> id=0x%08X status=%s seq=%d", s.remote, id, smpp.StatusName(status), seq)
	}
	return s.w.Flush()
}

func readPDU(r *bufio.Reader) (pdu, error) {
	hdr := make([]byte, 16)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return pdu{}, err
	}
	length := binary.BigEndian.Uint32(hdr[0:])
	if length < 16 || length > 1<<20 {
		return pdu{}, fmt.Errorf("bad pdu length %d", length)
	}
	body := make([]byte, length-16)
	if _, err := io.ReadFull(r, body); err != nil {
		return pdu{}, err
	}
	return pdu{
		id:     binary.BigEndian.Uint32(hdr[4:]),
		status: binary.BigEndian.Uint32(hdr[8:]),
		seq:    binary.BigEndian.Uint32(hdr[12:]),
		body:   body,
	}, nil
}

// readCString reads a NUL-terminated string starting at off, returning the
// string, the offset just past the NUL, and ok=false if no terminator found.
func readCString(b []byte, off int) (string, int, bool) {
	for i := off; i < len(b); i++ {
		if b[i] == 0 {
			return string(b[off:i]), i + 1, true
		}
	}
	return "", len(b), false
}

func codingName(c byte) string {
	switch c {
	case 0x00:
		return "GSM7(0)"
	case 0x01:
		return "IA5(1)"
	case 0x03:
		return "Latin1(3)"
	case 0x04:
		return "Binary(4)"
	case 0x08:
		return "UCS2(8)"
	default:
		return fmt.Sprintf("0x%02X", c)
	}
}
