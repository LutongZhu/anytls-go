package session

import (
	"anytls/proxy/padding"
	"anytls/util"
	"crypto/md5"
	"encoding/binary"
	"io"
	"net"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing/common/buf"
	"github.com/sirupsen/logrus"
)

var clientDebugPaddingScheme = os.Getenv("CLIENT_DEBUG_PADDING_SCHEME") == "1"

type Session struct {
	conn     net.Conn
	connLock sync.Mutex

	streams    map[uint32]*Stream
	streamId   atomic.Uint32
	streamLock sync.RWMutex

	dieOnce sync.Once
	die     chan struct{}
	dieHook func()

	// pool
	seq       uint64
	idleSince time.Time

	// client
	isClient   bool
	buffering  bool
	buffer     []byte
	pktCounter atomic.Uint32

	// server
	onNewStream func(stream *Stream)
}

func NewClientSession(conn net.Conn) *Session {
	s := &Session{
		conn:     conn,
		isClient: true,
	}
	s.die = make(chan struct{})
	s.streams = make(map[uint32]*Stream)
	return s
}

func NewServerSession(conn net.Conn, onNewStream func(stream *Stream)) *Session {
	s := &Session{
		conn:        conn,
		onNewStream: onNewStream,
		isClient:    false,
	}
	s.die = make(chan struct{})
	s.streams = make(map[uint32]*Stream)
	return s
}

func (s *Session) Run(isServer bool) {
	if isServer {
		s.recvLoop()
		return
	}

	settings := util.StringMap{
		"v":           "1",
		"client":      "anytls/0.0.1",
		"padding-md5": padding.DefaultPaddingFactory.Load().Md5,
	}
	f := newFrame(cmdSettings, 0)
	f.data = settings.ToBytes()
	s.buffering = true
	s.writeFrame(f)

	go s.recvLoop()
}

// IsClosed does a safe check to see if we have shutdown
func (s *Session) IsClosed() bool {
	select {
	case <-s.die:
		return true
	default:
		return false
	}
}

// Close is used to close the session and all streams.
func (s *Session) Close() error {
	var once bool
	s.dieOnce.Do(func() {
		close(s.die)
		once = true
	})

	if once {
		if s.dieHook != nil {
			s.dieHook()
		}
		s.streamLock.Lock()
		for k := range s.streams {
			s.streams[k].sessionClose()
		}
		s.streamLock.Unlock()
		return s.conn.Close()
	} else {
		return io.ErrClosedPipe
	}
}

// OpenStream is used to create a new stream for CLIENT
func (s *Session) OpenStream() (*Stream, error) {
	if s.IsClosed() {
		return nil, io.ErrClosedPipe
	}

	sid := s.streamId.Add(1)
	stream := newStream(sid, s)

	//logrus.Debugln("stream open", sid, s.streams)

	if _, err := s.writeFrame(newFrame(cmdSYN, sid)); err != nil {
		return nil, err
	}

	s.buffering = false // proxy Write it's SocksAddr to flush the buffer

	s.streamLock.Lock()
	defer s.streamLock.Unlock()
	select {
	case <-s.die:
		return nil, io.ErrClosedPipe
	default:
		s.streams[sid] = stream
		return stream, nil
	}
}

func (s *Session) recvLoop() error {
	defer s.Close()

	var receivedSettingsFromClient bool
	var hdr rawHeader

	for {
		if s.IsClosed() {
			return io.ErrClosedPipe
		}
		// read header first
		if _, err := io.ReadFull(s.conn, hdr[:]); err == nil {
			sid := hdr.StreamID()
			switch hdr.Cmd() {
			case cmdPSH:
				if hdr.Length() > 0 {
					buffer := buf.Get(int(hdr.Length()))
					if _, err := io.ReadFull(s.conn, buffer); err == nil {
						s.streamLock.RLock()
						stream, ok := s.streams[sid]
						s.streamLock.RUnlock()
						if ok {
							stream.pipeW.Write(buffer)
						}
						buf.Put(buffer)
					} else {
						buf.Put(buffer)
						return err
					}
				}
			case cmdSYN: // should be server only
				if !s.isClient && !receivedSettingsFromClient {
					f := newFrame(cmdAlert, 0)
					f.data = []byte("client did not send its settings")
					s.writeFrame(f)
					return nil
				}
				s.streamLock.Lock()
				if _, ok := s.streams[sid]; !ok {
					stream := newStream(sid, s)
					s.streams[sid] = stream
					if s.onNewStream != nil {
						go s.onNewStream(stream)
					} else {
						go s.Close()
					}
				}
				s.streamLock.Unlock()
			case cmdFIN:
				s.streamLock.RLock()
				stream, ok := s.streams[sid]
				s.streamLock.RUnlock()
				if ok {
					stream.Close()
				}
				//logrus.Debugln("stream fin", sid, s.streams)
			case cmdWaste:
				if hdr.Length() > 0 {
					buffer := buf.Get(int(hdr.Length()))
					if _, err := io.ReadFull(s.conn, buffer); err != nil {
						buf.Put(buffer)
						return err
					}
					buf.Put(buffer)
				}
			case cmdSettings:
				if hdr.Length() > 0 {
					buffer := buf.Get(int(hdr.Length()))
					if _, err := io.ReadFull(s.conn, buffer); err != nil {
						buf.Put(buffer)
						return err
					}
					if !s.isClient {
						receivedSettingsFromClient = true
						m := util.StringMapFromBytes(buffer)
						paddingF := padding.DefaultPaddingFactory.Load()
						if m["padding-md5"] != paddingF.Md5 {
							// logrus.Debugln("remote md5 is", m["padding-md5"])
							f := newFrame(cmdUpdatePaddingScheme, 0)
							f.data = paddingF.RawScheme
							_, err = s.writeFrame(f)
							if err != nil {
								buf.Put(buffer)
								return err
							}
						}
					}
					buf.Put(buffer)
				}
			case cmdAlert:
				if hdr.Length() > 0 {
					buffer := buf.Get(int(hdr.Length()))
					if _, err := io.ReadFull(s.conn, buffer); err != nil {
						buf.Put(buffer)
						return err
					}
					if s.isClient {
						logrus.Errorln("[Alert from server]", string(buffer))
					}
					buf.Put(buffer)
					return nil
				}
			case cmdUpdatePaddingScheme:
				if hdr.Length() > 0 {
					buffer := buf.Get(int(hdr.Length()))
					if _, err := io.ReadFull(s.conn, buffer); err != nil {
						buf.Put(buffer)
						return err
					}
					if s.isClient && !clientDebugPaddingScheme {
						if padding.UpdatePaddingScheme(buffer) {
							logrus.Infof("[Update padding succeed] %x\n", md5.Sum(buffer))
						} else {
							logrus.Warnf("[Update padding failed] %x\n", md5.Sum(buffer))
						}
					}
					buf.Put(buffer)
				}
			default:
				// 不知道什么命令（不能带有数据）
			}
		} else {
			return err
		}
	}
}

// notify the session that a stream has closed
func (s *Session) streamClosed(sid uint32) error {
	_, err := s.writeFrame(newFrame(cmdFIN, sid))
	s.streamLock.Lock()
	delete(s.streams, sid)
	s.streamLock.Unlock()
	return err
}

func (s *Session) writeFrame(frame frame) (int, error) {
	dataLen := len(frame.data)

	buffer := buf.NewSize(dataLen + headerOverHeadSize)
	buffer.WriteByte(frame.cmd)
	binary.BigEndian.PutUint32(buffer.Extend(4), frame.sid)
	binary.BigEndian.PutUint16(buffer.Extend(2), uint16(dataLen))
	buffer.Write(frame.data)
	_, err := s.writeConn(buffer.Bytes())
	buffer.Release()
	if err != nil {
		return 0, err
	}

	return dataLen, nil
}

func (s *Session) writeConn(b []byte) (n int, err error) {
	s.connLock.Lock()
	defer s.connLock.Unlock()

	if s.buffering {
		s.buffer = slices.Concat(s.buffer, b)
		return len(b), nil
	} else if len(s.buffer) > 0 {
		b = slices.Concat(s.buffer, b)
		s.buffer = nil
	}

	// calulate & send padding
	if s.isClient {
		pkt := s.pktCounter.Add(1)
		paddingF := padding.DefaultPaddingFactory.Load()
		if pkt < paddingF.Stop {
			pktSizes := paddingF.GenerateRecordPayloadSizes(pkt)
			for _, l := range pktSizes {
				remainPayloadLen := len(b)
				if l == padding.CheckMark {
					if remainPayloadLen == 0 {
						break
					} else {
						continue
					}
				}
				// logrus.Debugln(pkt, "write", l, "len", remainPayloadLen, "remain", remainPayloadLen-l)
				if remainPayloadLen > l { // this packet is all payload
					_, err = s.conn.Write(b[:l])
					if err != nil {
						return 0, err
					}
					n += l
					b = b[l:]
				} else if remainPayloadLen > 0 { // this packet contains padding and the last part of payload
					paddingLen := l - remainPayloadLen
					if paddingLen > 0 {
						padding := make([]byte, headerOverHeadSize+paddingLen)
						padding[0] = cmdWaste
						binary.BigEndian.PutUint32(padding[1:5], 0)
						binary.BigEndian.PutUint16(padding[5:7], uint16(paddingLen))
						b = slices.Concat(b, padding)
					}
					_, err = s.conn.Write(b)
					if err != nil {
						return 0, err
					}
					n += remainPayloadLen
					b = nil
				} else { // this packet is all padding
					padding := make([]byte, headerOverHeadSize+l)
					padding[0] = cmdWaste
					binary.BigEndian.PutUint32(padding[1:5], 0)
					binary.BigEndian.PutUint16(padding[5:7], uint16(l))
					_, err = s.conn.Write(b)
					if err != nil {
						return 0, err
					}
					b = nil
				}
			}
			// maybe still remain payload to write
			if len(b) == 0 {
				return
			}
		}
	}

	return s.conn.Write(b)
}
