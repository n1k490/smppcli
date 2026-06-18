# smppcli — SMPP v3.4 კლიენტი (emgload-ის სტილში)

`smppcli` არის Go-ზე დაწერილი ბრძანების ხაზის (CLI) უტილიტა SMS-ის გასაგზავნად
SMPP v3.4 პროტოკოლით. პარამეტრების გადაცემის ლოგიკა მაქსიმალურად
ახლოსაა emgload-თან (GNU-ს გრძელი ფლაგების სტილი: `--host`, `--threads`,
`--senders prefix:len` და ა.შ.), ამიტომ თუ emgload-ს იცნობ, აქ თავს კომფორტულად
იგრძნობ.

ინსტრუმენტი **მთლიანად stdlib-ზეა აწყობილი — გარე დამოკიდებულებების გარეშე**.
SMPP-ის მთელი ფენა (PDU-ების შეკვრა/გაშლა, bind, submit_sm, deliver_sm,
enquire_link, TLV-ები, ტექსტის კოდირება/სეგმენტაცია) ხელითაა დაწერილი. შედეგი —
ერთი სტატიკური ბინარი, ნულოვანი დამოკიდებულებებით, რომელიც მარტივად აუდიტდება.

---

## 1. რას აკეთებს

- უკავშირდება SMSC-ს TCP-ით და აკეთებს bind-ს როგორც **transmitter** (ნაგულისხმევი),
  **receiver** ან **transceiver**.
- აგზავნის `submit_sm`-ს — ერთ შეტყობინებას ან მასობრივ ნაკადს (load test).
- **ავტომატურად ირჩევს კოდირებას**: ქართული → UCS2 (UTF-16BE), ლათინური ტექსტი →
  GSM 7-bit. შეგიძლია ხელითაც აიძულო.
- **ანაწევრებს გრძელ ტექსტს** (concatenated SMS) — UDH-ით ან SAR TLV-ებით.
- იღებს **მიწოდების ქვითარს** (DLR) და ბეჭდავს გაშიფრულ `deliver_sm`-ს.
- მრავალნაკადიანი (`--threads`), ფანჯრიანი (`--window`) ასინქრონული გაგზავნა და
  სიჩქარის ლიმიტი (`--rate`).
- დამხმარე **mock SMSC** სერვერი ლოკალური ტესტირებისთვის.

---

## 2. აგება (build)

საჭიროა Go 1.22+.

```bash
# კლიენტი — სტატიკური ბინარი, გარე დამოკიდებულებების გარეშე
CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/smppcli ./cmd/smppcli/

# სატესტო mock SMSC სერვერი
CGO_ENABLED=0 go build -o bin/mocksmsc ./cmd/mocksmsc/
```

`CGO_ENABLED=0` უზრუნველყოფს სრულად სტატიკურ ბინარს — შეგიძლია გადაიტანო
ნებისმიერ Linux-ზე glibc-ის version-ებზე ფიქრის გარეშე (იდეალურია მინიმალურ
კონტეინერებში, scratch image-ში და ა.შ.). შემოწმება:

```bash
file bin/smppcli      # -> statically linked
ldd  bin/smppcli      # -> not a dynamic executable
```

ჯვარედინი კომპილაცია (მაგ. ARM64-ისთვის, შენი lazy-state-ის სტილში):

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/smppcli-arm64 ./cmd/smppcli/
```

---

## 3. პროექტის სტრუქტურა

```
smppcli/
├── go.mod
├── smpp/                 # SMPP 3.4-ის ბირთვი (ხელახლა გამოყენებადი პაკეტი)
│   ├── coding.go         # GSM 03.38 / UCS2 კოდირება + სეგმენტაცია + დეკოდირება
│   ├── pdu.go            # PDU-ების კონსტანტები, header-ის marshal, TLV-ები
│   └── session.go        # კავშირი, bind, submit, keepalive, MO/DLR მიღება
└── cmd/
    ├── smppcli/          # თავად CLI
    │   ├── main.go       # ფლაგები, კონფიგი, ვალიდაცია
    │   └── send.go       # გაგზავნის ძრავა (threads/window/rate, სეგმენტაცია)
    └── mocksmsc/
        └── main.go       # სატესტო ყალბი SMSC
```

`smpp/` დამოუკიდებელი პაკეტია — შეგიძლია სხვა Go პროექტშიც გამოიყენო.

---

## 4. სწრაფი დასაწყისი

### 4.1. გაუშვი mock SMSC ერთ ტერმინალში

```bash
./bin/mocksmsc -addr 127.0.0.1:2775 -dlr -verbose
```

- `-dlr` — ყოველ registered შეტყობინებაზე უკან აბრუნებს მიწოდების ქვითარს
  (მხოლოდ RX/TRX bind-ისთვის).
- `-verbose` — ბეჭდავს PDU-ების hex dump-ს.

### 4.2. გააგზავნე ქართული შეტყობინება მეორე ტერმინალში

```bash
./bin/smppcli --host 127.0.0.1 --port 2775 \
    --username test --password pass \
    --from MyService --to 995599123456 \
    --text "გამარჯობა, ეს არის სატესტო შეტყობინება" --verbose
```

შედეგი:

```
thread 0: bound as transmitter to 127.0.0.1:2775
thread 0: MyService -> 995599123456, coding=ucs2 dcs=0x08 segments=1
  submit_sm_resp OK  message_id="0000000001"
thread 0: unbound
```

mock-ის მხარეს დაინახავ, რომ ქართული ზუსტად აღდგა:

```
submit_sm #0000000001  MyService->995599123456  coding=UCS2(8) ...
        text="გამარჯობა, ეს არის სატესტო შეტყობინება"
```

---

## 5. გამოყენების მაგალითები

### ერთი ქართული SMS (ავტომატური UCS2)

```bash
./bin/smppcli --host smsc.example.com --port 2775 \
    -u myuser -p mypass \
    --from INFO --to 995599123456 \
    --text "თქვენი ვერიფიკაციის კოდია: 4821"
```

ქართული ასოები ვერ ჯდება GSM 7-bit ცხრილში, ამიტომ `smppcli` ავტომატურად ირჩევს
UCS2-ს (`data_coding = 0x08`). არაფრის ხელით მითითება არ გჭირდება.

### გრძელი ტექსტი (ავტომატური დანაწევრება)

```bash
./bin/smppcli -u u -p p --from INFO --to 995599123456 \
    --text "ძალიან გრძელი ტექსტი რომელიც 70 სიმბოლოს სცდება და ავტომატურად დაიყოფა რამდენიმე ნაწილად UDH-ით..."
```

ნაგულისხმევად სეგმენტაცია ხდება **UDH**-ით (`05 00 03 ref total seq`) და
`esm_class`-ში ინთება User-Data-Header ბიტი (0x40). ტელეფონი ნაწილებს ერთ
შეტყობინებად აწებებს.

### დანაწევრება SAR TLV-ებით (UDH-ის ნაცვლად)

```bash
./bin/smppcli -u u -p p --from INFO --to 995599123456 \
    --smpp_udh_via_optional \
    --text "გრძელი ტექსტი..."
```

ამ დროს UDH-ის ნაცვლად გამოიყენება ოპციური პარამეტრები:
`sar_msg_ref_num` (0x020C), `sar_total_segments` (0x020E),
`sar_segment_seqnum` (0x020F). ზოგი SMSC ამ ვარიანტს ითხოვს.

### მიწოდების ქვითარი (DLR)

```bash
./bin/smppcli -u u -p p --smppbindtrx \
    --from BANK --to 995577001122 \
    --text "თქვენი კოდია 4821" \
    --dlr --wait 5s --verbose
```

- `--smppbindtrx` — transceiver bind (DLR-ის მისაღებად საჭიროა RX ან TRX).
- `--dlr` — `registered_delivery = 1`.
- `--wait 5s` — გაგზავნის შემდეგ 5 წამი ელოდება შემოსულ `deliver_sm`-ს.

### Load test (მასობრივი ნაკადი)

```bash
./bin/smppcli --host 127.0.0.1 --port 2775 -u load -p p \
    --senders "INFO" --recipients "9955:9" \
    --text "Hello SMPP load test" \
    --messages 1000 --threads 4 --window 20 --rate 200
```

- `--messages 1000` — სულ 1000 შეტყობინება, თანაბრად გადანაწილებული ნაკადებზე.
- `--threads 4` — 4 ცალკე TCP კავშირი/bind, თითო თავის goroutine-ში.
- `--window 20` — ერთ კავშირზე ერთდროულად 20 დაუდასტურებელი `submit_sm`
  (ფანჯრიანი ასინქრონული გაგზავნა — გაცილებით სწრაფია, ვიდრე თითო-თითო ლოდინი).
- `--rate 200` — მთელ პროცესზე მაქს. 200 შეტყობინება წამში.
- `--recipients "9955:9"` — შემთხვევითი ნომრები: პრეფიქსი `9955` + 9 შემთხვევითი
  ციფრი. `--senders`-იც ანალოგიურად მუშაობს.

### ნედლი ბინარული payload (hex)

```bash
./bin/smppcli -u u -p p --from 1234 --to 995599111222 \
    --hex "DEADBEEF00FF" --coding binary
```

### მთელი შეტყობინება message_payload TLV-ში

```bash
./bin/smppcli -u u -p p --from INFO --to 995599123456 \
    --text "..." --message-payload
```

ტექსტი მთლიანად მიდის `message_payload` (0x0424) TLV-ში, `short_message`-ის
ნაცვლად — სეგმენტაცია არ ხდება. გამოსადეგია მაშინ, თუ SMSC ამ ფორმას ამჯობინებს.

### საკუთარი ოპერატორის TLV-ები

```bash
./bin/smppcli -u u -p p --from INFO --to 995599123456 --text "..." \
    --smpptlv "0x1400:0102" --smpptlv "5121:AABB"
```

`tag:hexvalue` — tag შეიძლება იყოს hex (`0x1400`) ან ათობითი (`5121`),
მნიშვნელობა — ყოველთვის hex. ფლაგი მეორდება რამდენჯერაც გინდა.

---

## 6. კოდირება და სეგმენტაცია — როგორ მუშაობს

ეს ყველაზე მნიშვნელოვანი ნაწილია ქართულისთვის, ამიტომ დეტალურად:

### კოდირების არჩევა (`--coding auto`, ნაგულისხმევი)

| ტექსტი | კოდირება | data_coding | სიმბოლო / 1 ნაწილი | სიმბოლო / ნაწილი (concat) |
|---|---|---|---|---|
| მხოლოდ GSM 03.38 ასოები | GSM 7-bit | 0x00 | 160 | 153 |
| ლათინური + ISO-8859-1 | Latin-1 | 0x03 | 140 | 134 |
| **ქართული, კირილიცა, emoji** | **UCS2** | **0x08** | **70** | **67** |

ქართული ანბანი GSM 7-bit ცხრილში არ არსებობს, ამიტომ **ყოველთვის UCS2-ში**
იგზავნება. UCS2 = UTF-16 big-endian; ქართულის ყველა ასო BMP-შია, ანუ 2 ბაიტი
თითო ასოზე. აქედან მოდის 70 ასოს ლიმიტი ერთ SMS-ში (140 ბაიტი ÷ 2).

### სეგმენტაცია

თუ ტექსტი ლიმიტს სცდება, `smppcli` ანაწევრებს. **სეგმენტი არასდროს იჭრება
სიმბოლოს შუაში** — ალგორითმი ჯერ ყოველ runes-ს ცალკე „ერთეულად" აქცევს
(surrogate წყვილებიც და GSM escape-ებიც მთლიანი რჩება) და მერე აჯგუფებს.

ორი ვარიანტი:

- **UDH** (ნაგულისხმევი): თითო ნაწილის დასაწყისში ემატება 6-ბაიტიანი თავსართი
  `05 00 03 <ref> <total> <seq>`, `esm_class |= 0x40`. `<ref>` შემთხვევითია.
- **SAR TLV** (`--smpp_udh_via_optional`): ნაცვლად UDH-ისა, ნაწილის
  იდენტიფიკაცია ოპციურ პარამეტრებში მიდის.

### ხელით გადაფარვა

- `--coding gsm|ucs2|latin1|ia5|binary` — აიძულებს კონკრეტულ კოდირებას.
- `--dcs N` — პირდაპირ წერს `data_coding` ბაიტს (0–255), თუ ოპერატორი
  სპეციფიკურ მნიშვნელობას ითხოვს.

> შენიშვნა: GSM 7-bit აქ **unpacked**-ად იგზავნება (1 ბაიტი თითო septet-ზე) —
> ეს SMPP-ში გავრცელებული კონვენციაა; თვითონ SMSC აკეთებს რეალურ 7-bit
> შეფუთვას ჰაერში გაშვებამდე.

---

## 7. mock SMSC — ლოკალური ტესტირება

`mocksmsc` არ არის ნამდვილი SMSC — ის სატესტო ხელსაწყოა. ის:

- იღებს ნებისმიერ bind-ს და პასუხობს `*_resp`-ით (status 0);
- ყოველ `submit_sm`-ს უპასუხებს `submit_sm_resp`-ით გენერირებული `message_id`-ით;
- **შიფრავს და ბეჭდავს მიღებულ ტექსტს** — ასე ამოწმებ ქართულის round-trip-ს;
- პასუხობს `enquire_link`-ს;
- სურვილისამებრ აბრუნებს DLR-ს (`-dlr`).

```bash
./bin/mocksmsc -addr 127.0.0.1:2775 -dlr -dlr-delay 500ms -verbose
```

| ფლაგი | მნიშვნელობა |
|---|---|
| `-addr` | მოსასმენი მისამართი (ნაგულისხმევი `127.0.0.1:2775`) |
| `-dlr` | DLR-ის გაგზავნა ყოველ registered submit-ზე |
| `-dlr-delay` | დაყოვნება DLR-ის გაგზავნამდე (ნაგულისხმევი 500ms) |
| `-verbose` | PDU-ების hex dump |

---

## 8. ფლაგების სრული ცნობარი

### კავშირი და bind
| ფლაგი | აღწერა | ნაგულისხმევი |
|---|---|---|
| `--host` | SMSC ჰოსტი | `127.0.0.1` |
| `--port` | SMSC პორტი | `2775` |
| `--protocol` | პროტოკოლი (მხოლოდ `smpp`) | `smpp` |
| `--username`, `--system-id`, `-u` | bind system_id | — (სავალდებულო) |
| `--password`, `-p` | bind password | — |
| `--system-type` | system_type | — |
| `--smppbindtrx` | transceiver bind | off (transmitter) |
| `--smppbindrx` | receiver bind | off |
| `--connect-timeout` | TCP connect timeout | `10s` |
| `--timeout` | პასუხის მოლოდინის timeout | `10s` |
| `--keepalive` | enquire_link ინტერვალი (წმ; 0 = გამორთული) | `0` |

### მისამართები
| ფლაგი | აღწერა | ნაგულისხმევი |
|---|---|---|
| `--from`, `--source` | წყაროს მისამართი | — |
| `--to`, `--dest` | დანიშნულების მისამართი | — |
| `--senders` | შემთხვევითი წყარო `prefix:len` | — |
| `--recipients` | შემთხვევითი დანიშნულება `prefix:len` | — |
| `--src-ton`, `--src-npi` | წყაროს TON/NPI (-1 = ავტო) | `-1` |
| `--dst-ton`, `--dst-npi` | დანიშნულების TON/NPI | `1` |

> ავტო-TON: ალფანუმერული გამგზავნი → TON 5 / NPI 0; მხოლოდ ციფრები → TON 1 / NPI 1.

### შიგთავსი და კოდირება
| ფლაგი | აღწერა | ნაგულისხმევი |
|---|---|---|
| `--text`, `--message` | შეტყობინების ტექსტი | — |
| `--text-file` | ტექსტის წაკითხვა ფაილიდან | — |
| `--hex` | ბინარული payload hex-ად | — |
| `--coding` | `auto\|gsm\|ucs2\|latin1\|ia5\|binary` | `auto` |
| `--dcs` | data_coding ბაიტის გადაფარვა (0–255) | `-1` |
| `--message-payload` | მთელი ტექსტი message_payload TLV-ში | off |
| `--smpp_udh_via_optional` | SAR TLV-ები UDH-ის ნაცვლად | off |
| `--smpptlv` | დამატებითი TLV `tag:hexvalue` (მეორდება) | — |

### submit_sm-ის ველები
| ფლაგი | აღწერა | ნაგულისხმევი |
|---|---|---|
| `--service-type` | service_type | — |
| `--esm-class` | esm_class საბაზისო მნიშვნელობა | `0` |
| `--protocol-id` | protocol_id | `0` |
| `--priority` | priority_flag (0–3) | `0` |
| `--validity` | validity_period | — |
| `--schedule` | schedule_delivery_time | — |
| `--dlr` | მოითხოვს ქვითარს (registered_delivery=1) | off |
| `--registered-delivery` | registered_delivery ბაიტი (გადაფარავს `--dlr`-ს) | `-1` |
| `--replace-if-present` | replace_if_present_flag | off |

### ნაკადი და სიჩქარე
| ფლაგი | აღწერა | ნაგულისხმევი |
|---|---|---|
| `--messages` | გასაგზავნი შეტყობინებების რაოდენობა | `1` |
| `--threads` | პარალელური კავშირების რაოდენობა | `1` |
| `--window` | ერთ კავშირზე დაუდასტურებელი PDU-ების მაქს. | `10` |
| `--rate` | მაქს. შეტყობინება/წამში (0 = ულიმიტო) | `0` |
| `--wait` | გაგზავნის შემდეგ DLR-ის მოლოდინის ხანგრძლივობა | `0` |
| `--verbose`, `-v` | დეტალური ლოგი | off |
| `--debug` | PDU header-ების ლოგი ხაზზე | off |

---

## 9. გასათვალისწინებელი დეტალები

- **მასშტაბი vs. SMSC ლიმიტები**: ნამდვილი ოპერატორები ხშირად ზღუდავენ window-სა
  და TPS-ს. დაიწყე კონსერვატიული `--window`/`--rate`-ით და მერე გაზარდე.
- **transmitter ვერ იღებს DLR-ს** — DLR-ისთვის გჭირდება `--smppbindtrx` ან
  `--smppbindrx` და `--wait`.
- **`short_message` ლიმიტია 254 ბაიტი**; ამის ზემოთ ავტომატურად გადადის
  `message_payload` TLV-ზე.
- **Ctrl+C** კორექტულად ამთავრებს — ხურავს bind-ებს და ბეჭდავს შემაჯამებელ
  სტატისტიკას.

---

## 10. ტესტირების შეჯამება

ლოკალურად `mocksmsc`-ის წინააღმდეგ შემოწმდა და დადასტურდა:

1. ქართული ერთსეგმენტიანი (UCS2) — ზუსტი round-trip.
2. გრძელი ქართული — 3-ნაწილიანი UDH concat, სწორი reassembly.
3. Load test — 50 შეტყობინება, 4 thread, window 10 — ყველა მიღებული.
4. DLR — TRX bind, registered delivery, ქვითარი მიღებული და გაშიფრული.
5. GSM7 ლათინური ტექსტის ავტო-დეტექცია.
6. Rate limit — `--rate 20` ზუსტად დაცული.
7. ნედლი hex / binary payload.
8. `message_payload` TLV.
9. საკუთარი ოპერატორის TLV-ები.
10. SAR TLV concat (`--smpp_udh_via_optional`) — sar_msg_ref_num/total/seqnum სწორი.
