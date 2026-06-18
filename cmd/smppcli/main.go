// Command smppcli is a small SMPP v3.4 client for sending SMS from the command
// line. Its option style mirrors Nordic Messaging's emgload load tool.
package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nika/smppcli/smpp"
)

// stringSlice is a repeatable string flag (e.g. --smpptlv a --smpptlv b).
type stringSlice []string

func (s *stringSlice) String() string     { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }

type config struct {
	host       string
	port       int
	protocol   string
	username   string
	password   string
	systemType string
	bindTRX    bool
	bindRX     bool

	from       string
	to         string
	senders    string
	recipients string
	srcTON     int
	srcNPI     int
	dstTON     int
	dstNPI     int

	text          string
	textFile      string
	hexData       string
	coding        string
	dcs           int
	udhViaOpt     bool
	usePayload    bool
	serviceType   string
	esmClass      int
	protocolID    int
	priority      int
	validity      string
	schedule      string
	dlr           bool
	registeredDlv int
	replace       bool
	tlvs          stringSlice

	messages       int
	threads        int
	window         int
	rate           float64
	connectTimeout time.Duration
	respTimeout    time.Duration
	keepAlive      int
	wait           time.Duration

	verbose bool
	debug   bool
}

func main() {
	cfg := parseFlags()

	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "smppcli: %v\n", err)
		os.Exit(1)
	}
}

func parseFlags() *config {
	c := &config{}
	fs := flag.NewFlagSet("smppcli", flag.ExitOnError)

	// Connection
	fs.StringVar(&c.host, "host", "127.0.0.1", "SMSC host")
	fs.IntVar(&c.port, "port", 2775, "SMSC port")
	fs.StringVar(&c.protocol, "protocol", "smpp", "protocol (only smpp supported)")

	// Auth
	fs.StringVar(&c.username, "username", "", "system_id (bind username)")
	fs.StringVar(&c.username, "system-id", "", "alias for --username")
	fs.StringVar(&c.password, "password", "", "bind password")
	fs.StringVar(&c.systemType, "system-type", "", "system_type")
	fs.BoolVar(&c.bindTRX, "smppbindtrx", false, "bind as transceiver instead of transmitter")
	fs.BoolVar(&c.bindRX, "smppbindrx", false, "bind as receiver only")

	// Addressing
	fs.StringVar(&c.from, "from", "", "source address (originator)")
	fs.StringVar(&c.from, "source", "", "alias for --from")
	fs.StringVar(&c.to, "to", "", "destination address (recipient)")
	fs.StringVar(&c.to, "dest", "", "alias for --to")
	fs.StringVar(&c.senders, "senders", "", "random source addresses, prefix:len (e.g. 99532:6)")
	fs.StringVar(&c.recipients, "recipients", "", "random destination addresses, prefix:len (e.g. 9955:9)")
	fs.IntVar(&c.srcTON, "src-ton", -1, "source TON (-1 = auto)")
	fs.IntVar(&c.srcNPI, "src-npi", -1, "source NPI (-1 = auto)")
	fs.IntVar(&c.dstTON, "dst-ton", 1, "destination TON")
	fs.IntVar(&c.dstNPI, "dst-npi", 1, "destination NPI")

	// Content
	fs.StringVar(&c.text, "text", "", "message text (UTF-8)")
	fs.StringVar(&c.text, "message", "", "alias for --text")
	fs.StringVar(&c.textFile, "text-file", "", "read message text from file")
	fs.StringVar(&c.hexData, "hex", "", "binary payload as hex (overrides text)")
	fs.StringVar(&c.coding, "coding", "auto", "auto|gsm|ucs2|latin1|ia5|binary")
	fs.IntVar(&c.dcs, "dcs", -1, "override data_coding byte (0-255)")
	fs.BoolVar(&c.udhViaOpt, "smpp_udh_via_optional", false, "use SAR TLVs instead of UDH for concatenation")
	fs.BoolVar(&c.usePayload, "message-payload", false, "send the whole message in the message_payload TLV (no segmentation)")
	fs.StringVar(&c.serviceType, "service-type", "", "service_type")
	fs.IntVar(&c.esmClass, "esm-class", 0, "esm_class base value")
	fs.IntVar(&c.protocolID, "protocol-id", 0, "protocol_id")
	fs.IntVar(&c.priority, "priority", 0, "priority_flag (0-3)")
	fs.StringVar(&c.validity, "validity", "", "validity_period (SMPP time format)")
	fs.StringVar(&c.schedule, "schedule", "", "schedule_delivery_time")
	fs.BoolVar(&c.dlr, "dlr", false, "request a delivery receipt (registered_delivery=1)")
	fs.IntVar(&c.registeredDlv, "registered-delivery", -1, "registered_delivery byte (overrides --dlr)")
	fs.BoolVar(&c.replace, "replace-if-present", false, "set replace_if_present_flag")
	fs.Var(&c.tlvs, "smpptlv", "add optional TLV tag:hexvalue (repeatable), tag in hex or dec")

	// Throughput
	fs.IntVar(&c.messages, "messages", 1, "number of messages to send")
	fs.IntVar(&c.threads, "threads", 1, "number of parallel SMPP sessions")
	fs.IntVar(&c.window, "window", 10, "outstanding submit_sm per session")
	fs.Float64Var(&c.rate, "rate", 0, "max messages per second (0 = unlimited)")
	fs.DurationVar(&c.connectTimeout, "connect-timeout", 10*time.Second, "TCP connect timeout")
	fs.DurationVar(&c.respTimeout, "timeout", 10*time.Second, "response timeout")
	fs.IntVar(&c.keepAlive, "keepalive", 0, "enquire_link interval in seconds (0 = off)")
	fs.DurationVar(&c.wait, "wait", 0, "after sending, keep the session open this long to receive deliver_sm/DLRs")

	fs.BoolVar(&c.verbose, "verbose", false, "verbose output")
	fs.BoolVar(&c.verbose, "v", false, "alias for --verbose")
	fs.BoolVar(&c.debug, "debug", false, "log PDU headers on the wire")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "smppcli — SMPP v3.4 client (emgload-style)\n\n")
		fmt.Fprintf(os.Stderr, "Usage: smppcli [options] [host [port]]\n\n")
		fmt.Fprintf(os.Stderr, "Single send:\n")
		fmt.Fprintf(os.Stderr, "  smppcli --host smsc.example.com --port 2775 \\\n")
		fmt.Fprintf(os.Stderr, "      --username user --password pass \\\n")
		fmt.Fprintf(os.Stderr, "      --from INFO --to 995599123456 --text \"გამარჯობა\"\n\n")
		fmt.Fprintf(os.Stderr, "Load test:\n")
		fmt.Fprintf(os.Stderr, "  smppcli --host 127.0.0.1 --port 2775 -u user -p pass \\\n")
		fmt.Fprintf(os.Stderr, "      --recipients 9955:9 --senders INFO --messages 1000 \\\n")
		fmt.Fprintf(os.Stderr, "      --threads 4 --window 20 --rate 200\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		fs.PrintDefaults()
	}

	_ = fs.Parse(os.Args[1:])

	// Positional fallback: host [port].
	if args := fs.Args(); len(args) > 0 {
		c.host = args[0]
		if len(args) > 1 {
			if p, err := strconv.Atoi(args[1]); err == nil {
				c.port = p
			}
		}
	}
	return c
}

// resolved holds derived, validated settings.
type resolved struct {
	addr     string
	bindMode smpp.BindMode
	coding   byte
	dcs      byte
	payload  []byte // for --hex
	isHex    bool
	tlvs     []smpp.TLV
	srcTON   byte
	srcNPI   byte
}

func run(c *config) error {
	if strings.ToLower(c.protocol) != "smpp" {
		return fmt.Errorf("only protocol smpp is supported (got %q)", c.protocol)
	}
	if c.username == "" {
		return fmt.Errorf("--username is required")
	}

	r := &resolved{addr: fmt.Sprintf("%s:%d", c.host, c.port)}

	switch {
	case c.bindRX:
		r.bindMode = smpp.BindRX
	case c.bindTRX:
		r.bindMode = smpp.BindTRX
	default:
		r.bindMode = smpp.BindTX
	}

	// Resolve message content + coding.
	if c.hexData != "" {
		raw, err := hex.DecodeString(strings.ReplaceAll(c.hexData, " ", ""))
		if err != nil {
			return fmt.Errorf("invalid --hex: %w", err)
		}
		r.payload = raw
		r.isHex = true
		r.coding = smpp.CodingBinary
	} else {
		if c.textFile != "" {
			b, err := os.ReadFile(c.textFile)
			if err != nil {
				return fmt.Errorf("read --text-file: %w", err)
			}
			c.text = strings.TrimRight(string(b), "\r\n")
		}
		if c.text == "" {
			if c.messages > 1 {
				c.text = "smppcli test message" // sensible default for benchmarks
			} else {
				return fmt.Errorf("provide message content via --text, --text-file or --hex")
			}
		}
		cd, err := resolveCoding(c.coding, c.text)
		if err != nil {
			return err
		}
		r.coding = cd
	}

	// data_coding byte.
	if c.dcs >= 0 {
		if c.dcs > 255 {
			return fmt.Errorf("--dcs must be 0-255")
		}
		r.dcs = byte(c.dcs)
	} else {
		r.dcs = r.coding
	}

	// Source TON/NPI auto-resolution.
	r.srcTON, r.srcNPI = resolveSrcTONNPI(c)

	// Parse user TLVs.
	tlvs, err := parseTLVs(c.tlvs)
	if err != nil {
		return err
	}
	r.tlvs = tlvs

	// Destination required for single, fixed send.
	if c.recipients == "" && c.to == "" {
		return fmt.Errorf("provide a recipient via --to or --recipients")
	}

	return send(c, r)
}

func resolveCoding(name, text string) (byte, error) {
	switch strings.ToLower(name) {
	case "auto", "":
		return smpp.PickCoding(text), nil
	case "gsm", "gsm7", "default":
		if _, ok := smpp.Segment(text, smpp.CodingGSM7); !ok {
			return 0, fmt.Errorf("text contains characters not in the GSM 03.38 alphabet; use --coding ucs2")
		}
		return smpp.CodingGSM7, nil
	case "ucs2", "ucs-2", "unicode", "utf16":
		return smpp.CodingUCS2, nil
	case "latin1", "latin-1", "iso-8859-1", "8859":
		if _, ok := smpp.Segment(text, smpp.CodingLatin1); !ok {
			return 0, fmt.Errorf("text contains characters outside Latin-1; use --coding ucs2")
		}
		return smpp.CodingLatin1, nil
	case "ia5", "ascii":
		if _, ok := smpp.Segment(text, smpp.CodingIA5); !ok {
			return 0, fmt.Errorf("text contains non-ASCII characters; use --coding ucs2")
		}
		return smpp.CodingIA5, nil
	case "binary", "8bit", "8-bit":
		return smpp.CodingBinary, nil
	default:
		return 0, fmt.Errorf("unknown --coding %q", name)
	}
}

func resolveSrcTONNPI(c *config) (ton, npi byte) {
	src := c.from
	if c.senders != "" {
		if pfx, _, ok := splitPrefixLen(c.senders); ok {
			src = pfx
		}
	}
	autoTON, autoNPI := byte(1), byte(1) // international / E.164
	if src != "" && !isNumeric(src) {
		autoTON, autoNPI = 5, 0 // alphanumeric
	}
	if c.srcTON >= 0 {
		ton = byte(c.srcTON)
	} else {
		ton = autoTON
	}
	if c.srcNPI >= 0 {
		npi = byte(c.srcNPI)
	} else {
		npi = autoNPI
	}
	return ton, npi
}

func isNumeric(s string) bool {
	s = strings.TrimPrefix(s, "+")
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func splitPrefixLen(s string) (prefix string, n int, ok bool) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return s, 0, false // treated as a fixed value
	}
	prefix = s[:i]
	v, err := strconv.Atoi(s[i+1:])
	if err != nil || v < 0 {
		return s, 0, false
	}
	return prefix, v, true
}

func parseTLVs(in []string) ([]smpp.TLV, error) {
	var out []smpp.TLV
	for _, raw := range in {
		i := strings.Index(raw, ":")
		if i < 0 {
			return nil, fmt.Errorf("--smpptlv %q must be tag:hexvalue", raw)
		}
		tagStr, valStr := raw[:i], raw[i+1:]
		tag, err := strconv.ParseUint(strings.TrimPrefix(tagStr, "0x"), hexBase(tagStr), 16)
		if err != nil {
			return nil, fmt.Errorf("--smpptlv bad tag %q: %w", tagStr, err)
		}
		val, err := hex.DecodeString(strings.ReplaceAll(valStr, " ", ""))
		if err != nil {
			return nil, fmt.Errorf("--smpptlv bad hex value %q: %w", valStr, err)
		}
		out = append(out, smpp.TLV{Tag: uint16(tag), Value: val})
	}
	return out, nil
}

func hexBase(s string) int {
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		return 16
	}
	// Bare tags are interpreted as hex too (SMPP tags are conventionally hex).
	return 16
}
