package main

import (
	"bufio"
	"errors"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/funny/gateway/aes256cbc"
	"github.com/funny/gateway/reuseport"
)

var (
	cfgSecret      []byte
	cfgDialRetry   = 1
	cfgDialTimeout = 3 * time.Second

	codeOK          = []byte("200")
	codeBadReq      = []byte("400")
	codeBadAddr     = []byte("401")
	codeDialErr     = []byte("502")
	codeDialTimeout = []byte("503")

	errBadRequest = errors.New("Bad request")

	testing     bool
	gatewayAddr string
	bufioPool   sync.Pool
)

func main() {
	if _, err := os.Stat("gateway.pid"); err == nil {
		log.Fatal("Already a pid file there")
	}
	pid := syscall.Getpid()
	if err := ioutil.WriteFile("gateway.pid", []byte(strconv.Itoa(pid)), 0644); err != nil {
		log.Fatal("Can't write pid file: %s", err)
	}
	defer os.Remove("gateway.pid")

	config()
	pprof()
	gateway()

	sigTERM := make(chan os.Signal, 1)
	sigINT := make(chan os.Signal, 1)
	signal.Notify(sigTERM, syscall.SIGTERM)
	signal.Notify(sigINT, syscall.SIGINT)

	log.Printf("Gateway running, pid = %d", pid)
	if !testing {
		select {
		case <-sigINT:
		case <-sigTERM:
		}
		log.Printf("Gateway killed")
	}
}

func config() {
	var err error

	cfgSecret = []byte(os.Getenv("GW_SECRET"))
	log.Printf("GW_SECRET=%s", cfgSecret)

	if v := os.Getenv("GW_DIAL_RETRY"); v != "" {
		cfgDialRetry, err = strconv.Atoi(v)
		if err != nil {
			log.Fatalf("GW_DIAL_RETRY - %s", err)
		}
		if cfgDialRetry == 0 {
			cfgDialRetry = 1
		}
	}
	log.Printf("GW_DIAL_RETRY=%d", cfgDialRetry)

	var timeout int
	if v := os.Getenv("GW_DIAL_TIMEOUT"); v != "" {
		timeout, err = strconv.Atoi(v)
		if err != nil {
			log.Fatalf("GW_DIAL_TIMEOUT - %s", err)
		}
	}
	if timeout == 0 {
		timeout = 3
	}
	cfgDialTimeout = time.Duration(timeout) * time.Second
	log.Printf("GW_DIAL_TIMEOUT=%d", timeout)
}

func pprof() {
	if v := os.Getenv("GW_PPROF_ADDR"); v != "" {
		listener, err := net.Listen("tcp", v)
		if err != nil {
			log.Fatalf("Setup pprof failed: %s", err)
		}
		log.Printf("Setup pprof at %s", listener.Addr())
		go http.Serve(listener, nil)
	}
}

func gateway() {
	var err error
	var listener net.Listener

	port := os.Getenv("GW_PORT")
	if port == "" {
		port = "0"
	}

	if os.Getenv("GW_REUSE_PORT") == "1" {
		listener, err = reuseport.NewReusablePortListener("tcp4", "0.0.0.0:"+port)
	} else {
		listener, err = net.Listen("tcp", "0.0.0.0:"+port)
	}

	if err != nil {
		log.Fatalf("Setup listener failed: %s", err)
	}

	gatewayAddr = listener.Addr().String()
	log.Printf("Setup gateway at %s", gatewayAddr)

	go loop(listener)
}

func loop(listener net.Listener) {
	defer listener.Close()
	for {
		conn, err := accept(listener)
		if err != nil {
			log.Printf("Gateway accept failed: %s", err)
			return
		}
		go handle(conn)
	}
}

func accept(listener net.Listener) (net.Conn, error) {
	var tempDelay time.Duration
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}
				if max := 1 * time.Second; tempDelay > max {
					tempDelay = max
				}
				time.Sleep(tempDelay)
				continue
			}
			return nil, err
		}
		tempDelay = 0
		return conn, nil
	}
}

func handle(conn net.Conn) {
	defer func() {
		conn.Close()
		if err := recover(); err != nil {
			log.Printf("Unhandled panic in connection handler: %v\n\n%s", err, debug.Stack())
		}
	}()

	reader, ok := bufioPool.Get().(*bufio.Reader)
	if ok {
		reader.Reset(conn)
	} else {
		reader = bufio.NewReader(conn)
	}
	bufioReleased := false
	defer func() {
		if !bufioReleased {
			reader.Reset(nil)
			bufioPool.Put(reader)
		}
	}()

	addr, err := handshake(conn, reader)
	if err != nil {
		return
	}

	var agent net.Conn
	agent, err = dial(string(addr), conn, reader)
	if err != nil {
		return
	}
	defer agent.Close()

	// now we can release the reader.
	bufioReleased = true
	reader.Reset(nil)
	bufioPool.Put(reader)

	if _, err = conn.Write(codeOK); err != nil {
		return
	}
	go safeCopy(agent, conn)
	io.Copy(conn, agent)
}

func handshake(conn net.Conn, reader *bufio.Reader) ([]byte, error) {
	firstByte, err := reader.ReadByte()
	if err != nil {
		conn.Write(codeBadReq)
		return nil, err
	}
	switch firstByte {
	case 0:
		return handshakeBinary(conn, reader)
	default:
		if err = reader.UnreadByte(); err != nil {
			return nil, err
		}
		return handshakeText(conn, reader)
	}
}

func handshakeBinary(conn net.Conn, reader *bufio.Reader) (addr []byte, err error) {
	var n byte
	n, err = reader.ReadByte()
	if err != nil {
		conn.Write(codeBadReq)
		return nil, err
	}

	var buf [256]byte
	bin := buf[:n]
	if _, err = io.ReadFull(reader, bin); err != nil {
		conn.Write(codeBadReq)
		return nil, err
	}

	if addr, err = aes256cbc.Decrypt(cfgSecret, bin); err != nil {
		conn.Write(codeBadAddr)
		return nil, err
	}
	return
}

func handshakeText(conn net.Conn, reader *bufio.Reader) (addr []byte, err error) {
	base64, err := reader.ReadSlice('\n')
	if err != nil {
		conn.Write(codeBadReq)
		return nil, err
	}
	if addr, err = aes256cbc.DecryptBase64(cfgSecret, base64); err != nil {
		conn.Write(codeBadAddr)
		return nil, err
	}
	return
}

func dial(addr string, conn net.Conn, reader *bufio.Reader) (agent net.Conn, err error) {
	for i := 0; i < cfgDialRetry; i++ {
		agent, err = net.DialTimeout("tcp", addr, cfgDialTimeout)
		if err == nil {
			break
		}
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			continue
		}
		conn.Write(codeDialErr)
		return nil, err
	}
	if err != nil {
		conn.Write(codeDialTimeout)
		return nil, err
	}
	if err = agentInit(agent, conn, reader); err != nil {
		agent.Close()
		conn.Write(codeDialErr)
		return nil, err
	}
	return
}

func agentInit(agent, conn net.Conn, reader *bufio.Reader) (err error) {
	err = agent.SetWriteDeadline(time.Now().Add(cfgDialTimeout))
	if err != nil {
		return
	}

	// Send client address to backend
	var buf [256]byte
	addr := conn.RemoteAddr().String()
	addrBuf := buf[:byte(len(addr)+1)]
	addrBuf[0] = byte(len(addr))
	copy(addrBuf[1:], addr)
	if _, err = agent.Write(addrBuf); err != nil {
		return
	}

	// Send bufio.Reader buffered data and release bufio.Reader.
	var data []byte
	if data, err = reader.Peek(reader.Buffered()); err != nil {
		return
	}
	if _, err = agent.Write(data); err != nil {
		return
	}

	return agent.SetWriteDeadline(time.Time{})
}

func safeCopy(dst io.Writer, src io.Reader) {
	defer func() {
		if err := recover(); err != nil {
			log.Printf("Unhandled panic in safe copy: %v\n\n%s", err, debug.Stack())
		}
	}()
	io.Copy(dst, src)
}
