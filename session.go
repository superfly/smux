package smux

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
)

const (
	defaultAcceptBacklog = 1024
)

const (
	errBrokenPipe         = "broken pipe"
	errEncryptionNotReady = "encryption not ready yet"
	errNoEncryptionKey    = "no encryption key"
	errBadKeyExchange     = "malformed key exchange"
	errBadKey             = "cannot decrypt the message"
	errInvalidProtocol    = "invalid protocol version"
)

type writeRequest struct {
	frame  Frame
	result chan writeResult
}

type writeResult struct {
	n   int
	err error
}

// Session defines a multiplexed connection for streams
type Session struct {
	conn      io.ReadWriteCloser
	writeLock sync.Mutex

	config       *Config
	nextStreamID uint32 // next stream identifier

	bucket     int32      // token bucket
	bucketCond *sync.Cond // used for waiting for tokens

	streams    map[uint32]*Stream // all streams in this session
	streamLock sync.Mutex         // locks streams

	die       chan struct{} // flag session has died
	dieLock   sync.Mutex
	chAccepts chan *Stream

	xmitPool  sync.Pool
	dataReady int32 // flag data has arrived

	deadline atomic.Value

	writes chan writeRequest

	client            bool
	encrypted         bool
	chEncryptionReady chan struct{} // flag encryption has been established
	encryptionReady   int32         // flag encryption has been established

	cryptStreamLock sync.Mutex
	cryptStream     *cipher.Stream
	encryptionKey   *[32]byte
}

func newSession(config *Config, conn io.ReadWriteCloser, encrypted bool, client bool) *Session {
	s := new(Session)
	s.die = make(chan struct{})
	s.conn = conn
	s.config = config
	s.streams = make(map[uint32]*Stream)
	s.chAccepts = make(chan *Stream, defaultAcceptBacklog)
	s.bucket = int32(config.MaxReceiveBuffer)
	s.bucketCond = sync.NewCond(&sync.Mutex{})
	s.xmitPool.New = func() interface{} {
		return make([]byte, (1<<16)+headerSize)
	}
	s.writes = make(chan writeRequest)
	s.encrypted = encrypted
	s.chEncryptionReady = make(chan struct{})
	s.client = client
	atomic.StoreInt32(&s.encryptionReady, 0)

	if client {
		s.nextStreamID = 1
	} else {
		s.nextStreamID = 2
	}
	go s.recvLoop()
	go s.sendLoop()
	go s.keepalive()
	if client && encrypted {
		go s.exchangeKeys()
	}
	return s
}

// OpenStream is used to create a new stream
func (s *Session) OpenStream() (*Stream, error) {
	if s.IsClosed() {
		return nil, errors.New(errBrokenPipe)
	}

	if !s.requireEncryption() {
		return nil, errors.New(errEncryptionNotReady)
	}

	sid := atomic.AddUint32(&s.nextStreamID, 2)
	stream := newStream(sid, s.config.MaxFrameSize, s)

	if _, err := s.writeFrame(newFrame(cmdSYN, sid)); err != nil {
		return nil, errors.Wrap(err, "writeFrame")
	}

	s.streamLock.Lock()
	s.streams[sid] = stream
	s.streamLock.Unlock()
	return stream, nil
}

// AcceptStream is used to block until the next available stream
// is ready to be accepted.
func (s *Session) AcceptStream() (*Stream, error) {
	var deadline <-chan time.Time
	if d, ok := s.deadline.Load().(time.Time); ok && !d.IsZero() {
		timer := time.NewTimer(d.Sub(time.Now()))
		defer timer.Stop()
		deadline = timer.C
	}

	if !s.requireEncryption() {
		return nil, errors.New(errEncryptionNotReady)
	}

	select {
	case stream := <-s.chAccepts:
		return stream, nil
	case <-deadline:
		return nil, errTimeout
	case <-s.die:
		return nil, errors.New(errBrokenPipe)
	}
}

// Close is used to close the session and all streams.
func (s *Session) Close() (err error) {
	s.dieLock.Lock()

	select {
	case <-s.die:
		s.dieLock.Unlock()
		return errors.New(errBrokenPipe)
	default:
		close(s.die)
		s.dieLock.Unlock()
		s.streamLock.Lock()
		for k := range s.streams {
			s.streams[k].sessionClose()
		}
		s.streamLock.Unlock()
		s.bucketCond.Signal()
		return s.conn.Close()
	}
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

func (s *Session) requireEncryption() bool {
	tickerTimeout := time.NewTicker(s.config.KeyHandshakeTimeout)
	defer tickerTimeout.Stop()
	if s.encrypted {
		select {
		case <-s.chEncryptionReady:
			return true
		case <-tickerTimeout.C:
			return false
		}
	}
	return true
}

// NumStreams returns the number of currently open streams
func (s *Session) NumStreams() int {
	if s.IsClosed() {
		return 0
	}
	s.streamLock.Lock()
	defer s.streamLock.Unlock()
	return len(s.streams)
}

// SetDeadline sets a deadline used by Accept* calls.
// A zero time value disables the deadline.
func (s *Session) SetDeadline(t time.Time) error {
	s.deadline.Store(t)
	return nil
}

// notify the session that a stream has closed
func (s *Session) streamClosed(sid uint32) {
	s.streamLock.Lock()
	if n := s.streams[sid].recycleTokens(); n > 0 { // return remaining tokens to the bucket
		if atomic.AddInt32(&s.bucket, int32(n)) > 0 {
			s.bucketCond.Signal()
		}
	}
	delete(s.streams, sid)
	s.streamLock.Unlock()
}

// returnTokens is called by stream to return token after read
func (s *Session) returnTokens(n int) {
	oldvalue := atomic.LoadInt32(&s.bucket)
	newvalue := atomic.AddInt32(&s.bucket, int32(n))
	if oldvalue <= 0 && newvalue > 0 {
		s.bucketCond.Signal()
	}

}

// session read a frame from underlying connection
// it's data is pointed to the input buffer
func (s *Session) readFrame(buffer []byte) (f Frame, err error) {
	if _, err := io.ReadFull(s.conn, buffer[:headerSize]); err != nil {
		return f, errors.Wrap(err, "readFrame")
	}

	dec := rawHeader(buffer)
	if dec.Version() != version {
		return f, errors.New(errInvalidProtocol)
	}

	f.ver = dec.Version()
	f.cmd = dec.Cmd()
	f.sid = dec.StreamID()
	if length := dec.Length(); length > 0 {
		if _, err := io.ReadFull(s.conn, buffer[headerSize:headerSize+length]); err != nil {
			return f, errors.Wrap(err, "readFrame")
		}
		f.data = buffer[headerSize : headerSize+length]
		if s.encrypted && f.cmd == cmdPSH {
			if err := decrypt(s, f.data, f.data); err != nil {
				return f, errors.Wrap(err, "readFrame")
			}
		}
	}
	return f, nil
}

// recvLoop keeps on reading from underlying connection if tokens are available
func (s *Session) recvLoop() {
	buffer := make([]byte, (1<<16)+headerSize)
	for {
		s.bucketCond.L.Lock()
		for atomic.LoadInt32(&s.bucket) <= 0 && !s.IsClosed() {
			s.bucketCond.Wait()
		}
		s.bucketCond.L.Unlock()

		if s.IsClosed() {
			return
		}

		if f, err := s.readFrame(buffer); err == nil {
			atomic.StoreInt32(&s.dataReady, 1)

			switch f.cmd {
			case cmdNOP:
			case cmdSYN:
				s.streamLock.Lock()
				if _, ok := s.streams[f.sid]; !ok {
					stream := newStream(f.sid, s.config.MaxFrameSize, s)
					s.streams[f.sid] = stream
					select {
					case s.chAccepts <- stream:
					case <-s.die:
					}
				}
				s.streamLock.Unlock()
			case cmdKXR:
				// only set key once for the duration of the session
				if !s.client && atomic.CompareAndSwapInt32(&s.encryptionReady, 0, 1) {
					key, err := verifyKeyExchange(&s.config.ServerPrivateKey, f.data)
					if err != nil {

						s.Close()
						return
					}
					s.setEncryptionStream(key)
					s.writeFrame(newKXSFrame(f.data))
				}
			case cmdKXS:
				// only set key once for the duration of the session
				if atomic.CompareAndSwapInt32(&s.encryptionReady, 0, 1) {
					// server accepted the encryption key
					s.writeFrame(newKXSFrame(f.data))
					close(s.chEncryptionReady)
				} else {
					// client accepted the encryption key
					close(s.chEncryptionReady)
				}
			case cmdRST:
				s.streamLock.Lock()
				if stream, ok := s.streams[f.sid]; ok {
					stream.markRST()
					stream.notifyReadEvent()
				}
				s.streamLock.Unlock()
			case cmdPSH:
				s.streamLock.Lock()
				if stream, ok := s.streams[f.sid]; ok {
					atomic.AddInt32(&s.bucket, -int32(len(f.data)))
					stream.pushBytes(f.data)
					stream.notifyReadEvent()
				}
				s.streamLock.Unlock()
			default:
				s.Close()
				return
			}
		} else {
			s.Close()
			return
		}
	}
}

func (s *Session) keepalive() {
	tickerPing := time.NewTicker(s.config.KeepAliveInterval)
	tickerTimeout := time.NewTicker(s.config.KeepAliveTimeout)
	defer tickerPing.Stop()
	defer tickerTimeout.Stop()
	for {
		select {
		case <-tickerPing.C:
			s.writeFrame(newFrame(cmdNOP, 0))
			s.bucketCond.Signal() // force a signal to the recvLoop
		case <-tickerTimeout.C:
			if !atomic.CompareAndSwapInt32(&s.dataReady, 1, 0) {
				s.Close()
				return
			}
		case <-s.die:
			return
		}
	}
}

func (s *Session) exchangeKeys() {
	pubKey, privKey, err := newKeyPair()
	if err != nil {
		s.Close()
		return
	}
	secret := newSecret(privKey, &s.config.ServerPublicKey)
	data, err := sealSecret(secret, pubKey)
	if err != nil {
		s.Close()
		return
	}

	s.setEncryptionStream(secret)

	s.writeFrame(newKXRFrame(data))
	s.bucketCond.Signal() // force a signal to the recvLoop
}

func (s *Session) setEncryptionStream(key *[32]byte) error {
	s.cryptStreamLock.Lock()
	defer s.cryptStreamLock.Unlock()
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return err
	}

	// If the key is unique for each ciphertext, then it's ok to use a zero IV.
	var iv [aes.BlockSize]byte
	stream := cipher.NewOFB(block, iv[:])
	s.cryptStream = &stream
	s.encryptionKey = key
	return nil
}

func (s *Session) sendLoop() {
	for {
		select {
		case <-s.die:
			return
		case request, ok := <-s.writes:
			if !ok {
				continue
			}
			buf := s.xmitPool.Get().([]byte)
			buf[0] = request.frame.ver
			buf[1] = request.frame.cmd
			binary.LittleEndian.PutUint16(buf[2:], uint16(len(request.frame.data)))
			binary.LittleEndian.PutUint32(buf[4:], request.frame.sid)
			copy(buf[headerSize:], request.frame.data)

			s.writeLock.Lock()
			n, err := s.conn.Write(buf[:headerSize+len(request.frame.data)])
			s.writeLock.Unlock()
			s.xmitPool.Put(buf)

			n -= headerSize
			if n < 0 {
				n = 0
			}

			result := writeResult{
				n:   n,
				err: err,
			}

			request.result <- result
			close(request.result)
		}
	}
}

// writeFrame writes the frame to the underlying connection
// and returns the number of bytes written if successful
func (s *Session) writeFrame(f Frame) (n int, err error) {
	req := writeRequest{
		frame:  f,
		result: make(chan writeResult, 1),
	}
	if s.encrypted && req.frame.cmd == cmdPSH {
		if err := encrypt(s, req.frame.data, req.frame.data); err != nil {
			return 0, err
		}
	}

	select {
	case <-s.die:
		return 0, errors.New(errBrokenPipe)
	case s.writes <- req:
	}

	result := <-req.result
	return result.n, result.err
}
