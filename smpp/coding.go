package smpp

import (
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf16"
)

// DataCoding values (SMPP v3.4, section 5.2.19).
const (
	CodingGSM7   = 0x00 // SMSC default alphabet (GSM 03.38)
	CodingIA5    = 0x01 // IA5 / ASCII
	CodingLatin1 = 0x03 // ISO-8859-1
	CodingBinary = 0x04 // 8-bit binary
	CodingUCS2   = 0x08 // UCS2 (UTF-16BE)
)

// GSM 03.38 basic alphabet. Index == septet value. 0x1B (index 27) is the
// escape to the extension table and is therefore not itself encodable.
var gsm7BasicRunes = [128]rune{
	'@', '£', '$', '¥', 'è', 'é', 'ù', 'ì', 'ò', 'Ç', '\n', 'Ø', 'ø', '\r', 'Å', 'å',
	'Δ', '_', 'Φ', 'Γ', 'Λ', 'Ω', 'Π', 'Ψ', 'Σ', 'Θ', 'Ξ', 0x1B, 'Æ', 'æ', 'ß', 'É',
	' ', '!', '"', '#', '¤', '%', '&', '\'', '(', ')', '*', '+', ',', '-', '.', '/',
	'0', '1', '2', '3', '4', '5', '6', '7', '8', '9', ':', ';', '<', '=', '>', '?',
	'¡', 'A', 'B', 'C', 'D', 'E', 'F', 'G', 'H', 'I', 'J', 'K', 'L', 'M', 'N', 'O',
	'P', 'Q', 'R', 'S', 'T', 'U', 'V', 'W', 'X', 'Y', 'Z', 'Ä', 'Ö', 'Ñ', 'Ü', '§',
	'¿', 'a', 'b', 'c', 'd', 'e', 'f', 'g', 'h', 'i', 'j', 'k', 'l', 'm', 'n', 'o',
	'p', 'q', 'r', 's', 't', 'u', 'v', 'w', 'x', 'y', 'z', 'ä', 'ö', 'ñ', 'ü', 'à',
}

// GSM 03.38 extension table. Encoded as 0x1B followed by the listed septet.
var gsm7ExtRunes = map[rune]byte{
	'\f': 0x0A, '^': 0x14, '{': 0x28, '}': 0x29, '\\': 0x2F,
	'[': 0x3C, '~': 0x3D, ']': 0x3E, '|': 0x40, '€': 0x65,
}

var gsm7BasicMap map[rune]byte

// gsm7ExtDecode is the inverse of gsm7ExtRunes: extension septet -> rune.
var gsm7ExtDecode map[byte]rune

func init() {
	gsm7BasicMap = make(map[rune]byte, 128)
	for i, r := range gsm7BasicRunes {
		if i == 0x1B {
			continue // escape placeholder, not a real character
		}
		// Skip duplicate mapping if a rune somehow appears twice; first wins.
		if _, dup := gsm7BasicMap[r]; !dup {
			gsm7BasicMap[r] = byte(i)
		}
	}

	gsm7ExtDecode = make(map[byte]rune, len(gsm7ExtRunes))
	for r, b := range gsm7ExtRunes {
		gsm7ExtDecode[b] = r
	}
}

// encodeUnits encodes s into per-rune byte units for the given coding. A "unit"
// is the indivisible byte sequence for one source rune (1-2 bytes for GSM7,
// 2 or 4 bytes for UCS2 surrogate pairs). Returning units lets the segmenter
// split a long message without ever cutting a multi-byte character in half.
//
// ok is false when s cannot be represented in the requested coding.
func encodeUnits(s string, coding byte) (units [][]byte, ok bool) {
	switch coding {
	case CodingGSM7:
		for _, r := range s {
			if b, found := gsm7BasicMap[r]; found {
				units = append(units, []byte{b})
			} else if e, found := gsm7ExtRunes[r]; found {
				units = append(units, []byte{0x1B, e})
			} else {
				return nil, false
			}
		}
		return units, true

	case CodingLatin1:
		for _, r := range s {
			if r > 0xFF {
				return nil, false
			}
			units = append(units, []byte{byte(r)})
		}
		return units, true

	case CodingIA5:
		for _, r := range s {
			if r > 0x7F {
				return nil, false
			}
			units = append(units, []byte{byte(r)})
		}
		return units, true

	case CodingUCS2:
		for _, r := range s {
			cu := utf16.Encode([]rune{r}) // 1 unit (BMP) or 2 (surrogate pair)
			b := make([]byte, len(cu)*2)
			for i, u := range cu {
				b[i*2] = byte(u >> 8)
				b[i*2+1] = byte(u)
			}
			units = append(units, b)
		}
		return units, true
	}
	return nil, false
}

// PickCoding chooses the most compact coding that can represent s, preferring
// GSM 7-bit, then Latin-1, then UCS2. Georgian, Cyrillic, emoji, etc. fall
// through to UCS2.
func PickCoding(s string) byte {
	if _, ok := encodeUnits(s, CodingGSM7); ok {
		return CodingGSM7
	}
	return CodingUCS2
}

// segmentLimits returns (singleMax, partMax) in bytes for a coding.
//   - singleMax: max payload bytes in a non-concatenated message.
//   - partMax:   max payload bytes per segment when concatenated, leaving room
//     for the 6-byte concatenation UDH.
func segmentLimits(coding byte) (singleMax, partMax int) {
	switch coding {
	case CodingUCS2:
		return 140, 134 // 70 vs 67 BMP chars
	default: // GSM7 / Latin1 / IA5: 1 byte per char (GSM7 unpacked over SMPP)
		return 160, 153
	}
}

// Segment encodes s for the given coding and splits it into one or more payload
// chunks (without UDH). When more than one chunk is returned the caller must
// prepend a concatenation UDH (or use SAR TLVs). It returns the chunks plus the
// coding actually used (relevant only if you let it auto-pick upstream).
func Segment(s string, coding byte) (chunks [][]byte, ok bool) {
	units, ok := encodeUnits(s, coding)
	if !ok {
		return nil, false
	}
	singleMax, partMax := segmentLimits(coding)

	total := 0
	for _, u := range units {
		total += len(u)
	}

	// Fits in a single PDU: no concatenation needed.
	if total <= singleMax {
		buf := make([]byte, 0, total)
		for _, u := range units {
			buf = append(buf, u...)
		}
		return [][]byte{buf}, true
	}

	// Concatenated: greedily pack units up to partMax bytes each, never
	// splitting a unit (so surrogate pairs and GSM escape sequences stay whole).
	var cur []byte
	for _, u := range units {
		if len(cur)+len(u) > partMax && len(cur) > 0 {
			chunks = append(chunks, cur)
			cur = nil
		}
		cur = append(cur, u...)
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks, true
}

// ConcatUDH builds the 6-byte concatenated short-message UDH (IEI 0x00,
// 1-byte reference) for segment seq of total, per 3GPP TS 23.040.
func ConcatUDH(ref byte, total, seq int) []byte {
	return []byte{0x05, 0x00, 0x03, ref, byte(total), byte(seq)}
}

// DecodeMessage decodes a received short_message body that was encoded with the
// given data coding back into a Go string. It is the inverse of the segment
// encoders and is used to render incoming deliver_sm payloads (MO messages and
// delivery receipts). The body must already have any UDH stripped. Unknown
// codings fall back to a hex dump so nothing is silently lost.
func DecodeMessage(coding byte, b []byte) string {
	switch coding {
	case CodingGSM7, CodingIA5:
		// We send GSM7 unpacked (one septet per octet), so decode byte-wise.
		// IA5/ASCII shares the low range and round-trips through the same path.
		var sb strings.Builder
		for i := 0; i < len(b); i++ {
			c := b[i]
			if coding == CodingGSM7 && c == 0x1B { // escape to extension table
				i++
				if i < len(b) {
					if r, ok := gsm7ExtDecode[b[i]]; ok {
						sb.WriteRune(r)
					} else {
						sb.WriteByte(' ')
					}
				}
				continue
			}
			if coding == CodingIA5 {
				sb.WriteByte(c & 0x7F)
				continue
			}
			if int(c) < len(gsm7BasicRunes) {
				if r := gsm7BasicRunes[c]; r != 0x1B {
					sb.WriteRune(r)
				}
			}
		}
		return sb.String()

	case CodingUCS2:
		if len(b)%2 != 0 {
			b = b[:len(b)-1] // drop a stray trailing octet rather than panic
		}
		u := make([]uint16, len(b)/2)
		for i := range u {
			u[i] = uint16(b[i*2])<<8 | uint16(b[i*2+1])
		}
		return string(utf16.Decode(u))

	case CodingLatin1:
		r := make([]rune, len(b))
		for i, c := range b {
			r[i] = rune(c) // ISO-8859-1 code points map 1:1 onto Unicode
		}
		return string(r)

	default:
		return fmt.Sprintf("<%d bytes: %s>", len(b), hex.EncodeToString(b))
	}
}
