package main

import (
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nika/smppcli/smpp"
)

const tagMessagePayload = 0x0424

// buildSegments turns one logical message into the submit_sm PDUs needed to
// carry it (one PDU when short, several when concatenated).
func buildSegments(c *config, r *resolved, src, dst string) ([]smpp.SubmitParams, error) {
	base := smpp.SubmitParams{
		ServiceType:          c.serviceType,
		SrcTON:               r.srcTON,
		SrcNPI:               r.srcNPI,
		SrcAddr:              src,
		DstTON:               byte(c.dstTON),
		DstNPI:               byte(c.dstNPI),
		DstAddr:              dst,
		ProtocolID:           byte(c.protocolID),
		PriorityFlag:         byte(c.priority),
		ScheduleDeliveryTime: c.schedule,
		ValidityPeriod:       c.validity,
		DataCoding:           r.dcs,
	}
	if c.replace {
		base.ReplaceIfPresent = 1
	}
	switch {
	case c.registeredDlv >= 0:
		base.RegisteredDelivery = byte(c.registeredDlv)
	case c.dlr:
		base.RegisteredDelivery = 1
	}
	esmBase := byte(c.esmClass)

	// Raw binary payload (--hex).
	if r.isHex {
		p := base
		p.ESMClass = esmBase
		if c.usePayload {
			p.TLVs = append([]smpp.TLV{{Tag: tagMessagePayload, Value: r.payload}}, r.tlvs...)
		} else {
			if len(r.payload) > 254 {
				return nil, fmt.Errorf("hex payload is %d bytes (>254); use --message-payload", len(r.payload))
			}
			p.ShortMessage = r.payload
			p.TLVs = r.tlvs
		}
		return []smpp.SubmitParams{p}, nil
	}

	chunks, ok := smpp.Segment(c.text, r.coding)
	if !ok {
		return nil, fmt.Errorf("cannot encode text in the selected coding")
	}

	// Whole message in message_payload TLV (no UDH, single PDU).
	if c.usePayload {
		full := concat(chunks)
		p := base
		p.ESMClass = esmBase
		p.TLVs = append([]smpp.TLV{{Tag: tagMessagePayload, Value: full}}, r.tlvs...)
		return []smpp.SubmitParams{p}, nil
	}

	// Single segment.
	if len(chunks) == 1 {
		p := base
		p.ESMClass = esmBase
		p.ShortMessage = chunks[0]
		p.TLVs = r.tlvs
		return []smpp.SubmitParams{p}, nil
	}

	// Concatenated multipart.
	total := len(chunks)
	ref8 := byte(rand.Intn(256))
	ref16 := uint16(rand.Intn(1 << 16))
	out := make([]smpp.SubmitParams, 0, total)
	for i, ch := range chunks {
		seq := i + 1
		p := base
		if c.udhViaOpt {
			p.ESMClass = esmBase
			p.ShortMessage = ch
			p.TLVs = append([]smpp.TLV{
				{Tag: 0x020C, Value: []byte{byte(ref16 >> 8), byte(ref16)}}, // sar_msg_ref_num
				{Tag: 0x020E, Value: []byte{byte(total)}},                   // sar_total_segments
				{Tag: 0x020F, Value: []byte{byte(seq)}},                     // sar_segment_seqnum
			}, r.tlvs...)
		} else {
			p.ESMClass = esmBase | 0x40 // UDHI: short_message starts with a UDH
			p.ShortMessage = append(smpp.ConcatUDH(ref8, total, seq), ch...)
			p.TLVs = r.tlvs
		}
		out = append(out, p)
	}
	return out, nil
}

func concat(chunks [][]byte) []byte {
	var b []byte
	for _, c := range chunks {
		b = append(b, c...)
	}
	return b
}

func pickSrc(c *config) string {
	if c.senders != "" {
		if pfx, n, ok := splitPrefixLen(c.senders); ok {
			return randomDigits(pfx, n)
		}
		return c.senders
	}
	return c.from
}

func pickDst(c *config) string {
	if c.recipients != "" {
		if pfx, n, ok := splitPrefixLen(c.recipients); ok {
			return randomDigits(pfx, n)
		}
		return c.recipients
	}
	return c.to
}

func randomDigits(prefix string, n int) string {
	if n <= 0 {
		return prefix
	}
	var sb strings.Builder
	sb.WriteString(prefix)
	for i := 0; i < n; i++ {
		sb.WriteByte(byte('0' + rand.Intn(10)))
	}
	return sb.String()
}

// limiter is a shared global rate limiter (messages per second across threads).
type limiter struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

func newLimiter(rate float64) *limiter {
	if rate <= 0 {
		return nil
	}
	return &limiter{interval: time.Duration(float64(time.Second) / rate)}
}

func (l *limiter) wait() {
	if l == nil {
		return
	}
	l.mu.Lock()
	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}
	t := l.next
	l.next = l.next.Add(l.interval)
	l.mu.Unlock()
	if d := time.Until(t); d > 0 {
		time.Sleep(d)
	}
}

type counters struct {
	sent, accepted, failed int64
}

func send(c *config, r *resolved) error {
	threads := c.threads
	if threads < 1 {
		threads = 1
	}
	total := c.messages
	if total < 1 {
		total = 1
	}
	if threads > total {
		threads = total
	}
	window := c.window
	if window < 1 {
		window = 1
	}
	lim := newLimiter(c.rate)
	single := total == 1 && threads == 1

	var cnt counters
	var firstErr error
	var errMu sync.Mutex
	recordErr := func(err error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		errMu.Unlock()
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
	}

	stop := make(chan struct{})
	var stopOnce sync.Once
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigc
		fmt.Fprintln(os.Stderr, "\ninterrupted — finishing outstanding PDUs and unbinding...")
		stopOnce.Do(func() { close(stop) })
	}()

	per := total / threads
	extra := total % threads
	start := time.Now()

	var wg sync.WaitGroup
	for t := 0; t < threads; t++ {
		count := per
		if t < extra {
			count++
		}
		wg.Add(1)
		go func(threadID, count int) {
			defer wg.Done()
			runSession(c, r, threadID, count, window, lim, stop, &cnt, recordErr, single)
		}(t, count)
	}
	wg.Wait()
	elapsed := time.Since(start)

	signal.Stop(sigc)

	acc := atomic.LoadInt64(&cnt.accepted)
	fmt.Printf("\n--- summary ---\n")
	fmt.Printf("sent (PDUs):  %d\n", atomic.LoadInt64(&cnt.sent))
	fmt.Printf("accepted:     %d\n", acc)
	fmt.Printf("failed:       %d\n", atomic.LoadInt64(&cnt.failed))
	fmt.Printf("elapsed:      %s\n", elapsed.Round(time.Millisecond))
	if secs := elapsed.Seconds(); secs > 0 {
		fmt.Printf("throughput:   %.1f msg/s\n", float64(acc)/secs)
	}

	if firstErr != nil && acc == 0 {
		return firstErr
	}
	return nil
}

func runSession(c *config, r *resolved, threadID, count, window int, lim *limiter,
	stop <-chan struct{}, cnt *counters, recordErr func(error), single bool) {

	sess, err := smpp.Dial(r.addr, c.connectTimeout, c.respTimeout)
	if err != nil {
		recordErr(fmt.Errorf("thread %d: connect %s: %w", threadID, r.addr, err))
		return
	}
	defer sess.Close()

	if c.debug {
		sess.Debug = func(dir, summary string, raw []byte) {
			fmt.Fprintf(os.Stderr, "[t%d %s] %s\n", threadID, dir, summary)
		}
	}
	sess.OnMO = func(src, dst string, dc byte, body []byte) {
		fmt.Printf("[deliver_sm] %s -> %s: %s\n", src, dst, smpp.DecodeMessage(dc, body))
	}

	if err := sess.Bind(r.bindMode, c.username, c.password, c.systemType); err != nil {
		recordErr(fmt.Errorf("thread %d: %w", threadID, err))
		return
	}
	if c.verbose || single {
		fmt.Fprintf(os.Stderr, "thread %d: bound as %s to %s\n", threadID, r.bindMode, r.addr)
	}
	if c.keepAlive > 0 {
		sess.StartKeepAlive(time.Duration(c.keepAlive) * time.Second)
	}

	sem := make(chan struct{}, window)
	var segWg sync.WaitGroup

loop:
	for i := 0; i < count; i++ {
		select {
		case <-stop:
			break loop
		case <-sess.Done():
			recordErr(fmt.Errorf("thread %d: session closed unexpectedly", threadID))
			break loop
		default:
		}

		src, dst := pickSrc(c), pickDst(c)
		segs, err := buildSegments(c, r, src, dst)
		if err != nil {
			recordErr(fmt.Errorf("thread %d: %w", threadID, err))
			break
		}
		if single || c.verbose {
			fmt.Fprintf(os.Stderr, "thread %d: %s -> %s, coding=%s dcs=0x%02X segments=%d\n",
				threadID, src, dst, codingName(r.coding), r.dcs, len(segs))
		}

		for _, seg := range segs {
			select {
			case <-stop:
				break loop
			default:
			}
			lim.wait()
			sem <- struct{}{}
			atomic.AddInt64(&cnt.sent, 1)
			resCh := sess.SubmitAsync(seg)
			segWg.Add(1)
			go func() {
				defer segWg.Done()
				res := <-resCh
				<-sem
				if res.Err == nil && res.Status == 0 {
					atomic.AddInt64(&cnt.accepted, 1)
					if single || c.verbose {
						fmt.Printf("  submit_sm_resp OK  message_id=%q\n", res.MessageID)
					}
				} else {
					atomic.AddInt64(&cnt.failed, 1)
					reason := ""
					if res.Err != nil {
						reason = res.Err.Error()
					} else {
						reason = smpp.StatusName(res.Status)
					}
					if single || c.verbose {
						fmt.Printf("  submit_sm FAILED: %s\n", reason)
					}
				}
			}()
		}
	}
	segWg.Wait()

	// Linger to receive trailing deliver_sm/DLRs if asked.
	if c.wait > 0 && (r.bindMode == smpp.BindRX || r.bindMode == smpp.BindTRX) {
		if c.verbose || single {
			fmt.Fprintf(os.Stderr, "thread %d: waiting %s for deliver_sm...\n", threadID, c.wait)
		}
		select {
		case <-time.After(c.wait):
		case <-stop:
		case <-sess.Done():
		}
	}

	_ = sess.Unbind()
	if c.verbose || single {
		fmt.Fprintf(os.Stderr, "thread %d: unbound\n", threadID)
	}
}

func codingName(c byte) string {
	switch c {
	case smpp.CodingGSM7:
		return "gsm7"
	case smpp.CodingIA5:
		return "ia5"
	case smpp.CodingLatin1:
		return "latin1"
	case smpp.CodingBinary:
		return "binary"
	case smpp.CodingUCS2:
		return "ucs2"
	default:
		return fmt.Sprintf("0x%02X", c)
	}
}
