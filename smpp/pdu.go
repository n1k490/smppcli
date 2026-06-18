package smpp

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Command IDs (SMPP v3.4).
const (
	bindReceiver        = 0x00000001
	bindTransmitter     = 0x00000002
	submitSM            = 0x00000004
	deliverSM           = 0x00000005
	unbind              = 0x00000006
	bindTransceiver     = 0x00000009
	enquireLink         = 0x00000015
	genericNack         = 0x80000000
	bindReceiverResp    = 0x80000001
	bindTransmitterResp = 0x80000002
	submitSMResp        = 0x80000004
	deliverSMResp       = 0x80000005
	unbindResp          = 0x80000006
	bindTransceiverResp = 0x80000009
	enquireLinkResp     = 0x80000015
)

const interfaceVersion34 = 0x34

// Optional-parameter (TLV) tags used here.
const (
	tagSarMsgRefNum    = 0x020C
	tagSarTotalSegs    = 0x020E
	tagSarSegmentSeq   = 0x020F
	tagMessagePayload  = 0x0424
	tagReceiptedMsgID  = 0x001E
	tagMessageStateTLV = 0x0427
)

// BindMode selects how the client authenticates with the SMSC.
type BindMode int

const (
	BindTX  BindMode = iota // transmitter (send only)
	BindRX                  // receiver (receive only)
	BindTRX                 // transceiver (send + receive)
)

func (m BindMode) commandID() uint32 {
	switch m {
	case BindRX:
		return bindReceiver
	case BindTRX:
		return bindTransceiver
	default:
		return bindTransmitter
	}
}

func (m BindMode) String() string {
	switch m {
	case BindRX:
		return "receiver"
	case BindTRX:
		return "transceiver"
	default:
		return "transmitter"
	}
}

// pdu is a decoded SMPP protocol data unit.
type pdu struct {
	id     uint32
	status uint32
	seq    uint32
	body   []byte
}

func (p pdu) marshal() []byte {
	length := 16 + len(p.body)
	out := make([]byte, length)
	binary.BigEndian.PutUint32(out[0:], uint32(length))
	binary.BigEndian.PutUint32(out[4:], p.id)
	binary.BigEndian.PutUint32(out[8:], p.status)
	binary.BigEndian.PutUint32(out[12:], p.seq)
	copy(out[16:], p.body)
	return out
}

const maxPDULen = 1 << 20 // 1 MiB sanity cap

// bodyBuilder assembles a PDU body with the C-octet-string / fixed-octet
// conventions SMPP uses.
type bodyBuilder struct {
	buf []byte
}

// cstr appends a C-Octet String (value + NUL terminator).
func (b *bodyBuilder) cstr(s string) *bodyBuilder {
	b.buf = append(b.buf, []byte(s)...)
	b.buf = append(b.buf, 0)
	return b
}

func (b *bodyBuilder) u8(v byte) *bodyBuilder {
	b.buf = append(b.buf, v)
	return b
}

// octets appends a length-prefixed octet field (used for short_message, where
// sm_length precedes the bytes).
func (b *bodyBuilder) lenOctets(v []byte) *bodyBuilder {
	b.buf = append(b.buf, byte(len(v)))
	b.buf = append(b.buf, v...)
	return b
}

// tlv appends an optional parameter (tag, length, value).
func (b *bodyBuilder) tlv(tag uint16, value []byte) *bodyBuilder {
	var hdr [4]byte
	binary.BigEndian.PutUint16(hdr[0:], tag)
	binary.BigEndian.PutUint16(hdr[2:], uint16(len(value)))
	b.buf = append(b.buf, hdr[:]...)
	b.buf = append(b.buf, value...)
	return b
}

func (b *bodyBuilder) bytes() []byte { return b.buf }

// readCString reads a NUL-terminated string from buf starting at off,
// returning the string and the offset just past the terminator.
func readCString(buf []byte, off int) (string, int, error) {
	for i := off; i < len(buf); i++ {
		if buf[i] == 0 {
			return string(buf[off:i]), i + 1, nil
		}
	}
	return "", off, errors.New("smpp: unterminated C-Octet string")
}

// statusName maps the most common command_status codes to their SMPP names.
func statusName(code uint32) string {
	switch code {
	case 0x00000000:
		return "ESME_ROK"
	case 0x00000001:
		return "ESME_RINVMSGLEN"
	case 0x00000002:
		return "ESME_RINVCMDLEN"
	case 0x00000003:
		return "ESME_RINVCMDID"
	case 0x00000004:
		return "ESME_RINVBNDSTS"
	case 0x00000005:
		return "ESME_RALYBND"
	case 0x0000000A:
		return "ESME_RINVSRCADR"
	case 0x0000000B:
		return "ESME_RINVDSTADR"
	case 0x0000000D:
		return "ESME_RBINDFAIL"
	case 0x0000000E:
		return "ESME_RINVPASWD"
	case 0x0000000F:
		return "ESME_RINVSYSID"
	case 0x00000011:
		return "ESME_RCANCELFAIL"
	case 0x00000013:
		return "ESME_RREPLACEFAIL"
	case 0x00000014:
		return "ESME_RMSGQFUL"
	case 0x00000045:
		return "ESME_RSUBMITFAIL"
	case 0x00000058:
		return "ESME_RTHROTTLED"
	case 0x00000064:
		return "ESME_RINVOPTPARSTREAM"
	case 0x000000FF:
		return "ESME_RUNKNOWNERR"
	default:
		return fmt.Sprintf("0x%08X", code)
	}
}

// StatusName returns the symbolic ESME_* name for a command_status value, or a
// hex string for codes we don't have a constant for. Exported for the CLI's
// result reporting.
func StatusName(code uint32) string {
	return statusName(code)
}
