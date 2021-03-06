package tcp

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/buger/goreplay/size"
	"github.com/google/gopacket"
)

// Stats every message carry its own stats object
type Stats struct {
	LostData   int
	Length     int       // length of the data
	Start      time.Time // first packet's timestamp
	End        time.Time // last packet's timestamp
	SrcAddr    string
	DstAddr    string
	IsIncoming bool
	TimedOut   bool // timeout before getting the whole message
	Truncated  bool // last packet truncated due to max message size
	IPversion  byte
}

// Message is the representation of a tcp message
type Message struct {
	packets []*Packet
	done    chan bool
	data    []byte
	Stats
}

// NewMessage ...
func NewMessage(srcAddr, dstAddr string, ipVersion uint8) (m *Message) {
	m = new(Message)
	m.DstAddr = dstAddr
	m.SrcAddr = srcAddr
	m.IPversion = ipVersion
	m.done = make(chan bool)
	return
}

// UUID the unique id of a TCP session it is not granted to be unique overtime
func (m *Message) UUID() []byte {
	var src, dst string
	if m.IsIncoming {
		src = m.SrcAddr
		dst = m.DstAddr
	} else {
		src = m.DstAddr
		dst = m.SrcAddr
	}

	length := len(src) + len(dst)
	uuid := make([]byte, length)
	copy(uuid, src)
	copy(uuid[len(src):], dst)
	sha := sha1.Sum(uuid)
	uuid = make([]byte, 40)
	hex.Encode(uuid, sha[:])

	return uuid
}

func (m *Message) add(pckt *Packet) {
	m.Length += len(pckt.Payload)
	m.LostData += int(pckt.Lost)
	m.packets = append(m.packets, pckt)
	m.data = append(m.data, pckt.Payload...)
	m.End = pckt.Timestamp
}

// Packets returns packets of this message
func (m *Message) Packets() []*Packet {
	return m.packets
}

// Data returns data in this message
func (m *Message) Data() []byte {
	return m.data
}

// Sort a helper to sort packets
func (m *Message) Sort() {
	sort.SliceStable(m.packets, func(i, j int) bool { return m.packets[i].Seq < m.packets[j].Seq })
}

// Handler message handler
type Handler func(*Message)

// Debugger is the debugger function. first params is the indicator of the issue's priority
// the higher the number, the lower the priority. it can be 4 <= level <= 6.
type Debugger func(int, ...interface{})

// HintEnd hints the pool to stop the session, see MessagePool.End
// when set, it will be executed before checking FIN or RST flag
type HintEnd func(*Message) bool

// HintStart hints the pool to start the reassembling the message, see MessagePool.Start
// when set, it will be used instead of checking SYN flag
type HintStart func(*Packet) (IsIncoming, IsOutgoing bool)

// MessagePool holds data of all tcp messages in progress(still receiving/sending packets).
// Incoming message is identified by its source port and address e.g: 127.0.0.1:45785.
// Outgoing message is identified by  server.addr and dst.addr e.g: localhost:80=internet:45785.
type MessagePool struct {
	sync.Mutex
	debug         Debugger
	maxSize       size.Size // maximum message size, default 5mb
	pool          map[string]*Message
	handler       Handler
	messageExpire time.Duration // the maximum time to wait for the final packet, minimum is 100ms
	End           HintEnd
	Start         HintStart
}

// NewMessagePool returns a new instance of message pool
func NewMessagePool(maxSize size.Size, messageExpire time.Duration, debugger Debugger, handler Handler) (pool *MessagePool) {
	pool = new(MessagePool)
	pool.debug = debugger
	pool.handler = handler
	pool.messageExpire = time.Millisecond * 100
	if pool.messageExpire < messageExpire {
		pool.messageExpire = messageExpire
	}
	pool.maxSize = maxSize
	if pool.maxSize < 1 {
		pool.maxSize = 5 << 20
	}
	pool.pool = make(map[string]*Message)
	return pool
}

// Handler returns packet handler
func (pool *MessagePool) Handler(packet gopacket.Packet) {
	var in, out bool
	pckt, err := ParsePacket(packet)
	if err != nil || pckt == nil {
		go pool.say(4, fmt.Sprintf("error decoding packet(%dBytes):%s\n", packet.Metadata().CaptureLength, err))
		return
	}
	pool.Lock()
	defer pool.Unlock()
	srcKey := pckt.Src()
	dstKey := srcKey + "=" + pckt.Dst()
	m, ok := pool.pool[srcKey]
	if !ok {
		m, ok = pool.pool[dstKey]
	}
	if pckt.RST {
		if ok {
			<-m.done
		}
		if m, ok = pool.pool[pckt.Dst()]; !ok {
			m, ok = pool.pool[pckt.Dst()+"="+srcKey]
		}
		if ok {
			<-m.done
		}
		go pool.say(4, fmt.Sprintf("RST flag from %s to %s at %s\n", pckt.Src(), pckt.Dst(), pckt.Timestamp))
		return
	}
	switch {
	case ok:
		pool.addPacket(m, pckt)
		return
	case pool.Start != nil:
		if in, out = pool.Start(pckt); in || out {
			break
		}
		return
	case pckt.SYN:
		in = !pckt.ACK
	default:
		return
	}
	m = NewMessage(srcKey, pckt.Dst(), pckt.Version)
	m.IsIncoming = in
	key := srcKey
	if !m.IsIncoming {
		key = dstKey
	}
	pool.pool[key] = m
	m.Start = pckt.Timestamp
	go pool.dispatch(key, m)
	pool.addPacket(m, pckt)
}

func (pool *MessagePool) dispatch(key string, m *Message) {
	select {
	case <-m.done:
		defer func() { m.done <- true }()
	case <-time.After(pool.messageExpire):
		pool.Lock()
		defer pool.Unlock()
		m.TimedOut = true
	}
	delete(pool.pool, key)
	pool.handler(m)
}

func (pool *MessagePool) addPacket(m *Message, pckt *Packet) {
	trunc := m.Length + len(pckt.Payload) - int(pool.maxSize)
	if trunc > 0 {
		m.Truncated = true
		pckt.Payload = pckt.Payload[:int(pool.maxSize)-m.Length]
	}
	m.add(pckt)
	switch {
	case trunc >= 0:
	case pool.End != nil && pool.End(m):
	case pckt.FIN:
	default:
		return
	}
	m.done <- true
	<-m.done
}

// this function should not block other pool operations
func (pool *MessagePool) say(level int, args ...interface{}) {
	if pool.debug != nil {
		pool.debug(level, args...)
	}
}
