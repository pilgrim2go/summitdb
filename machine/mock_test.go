package machine

import (
	"errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/garyburd/redigo/redis"
	"github.com/tidwall/finn"
	"github.com/tidwall/redcon"
	"github.com/tidwall/redlog"
)

var errTimeout = errors.New("timeout")

func mockCleanup() {
	fmt.Printf("Cleanup: may take some time... ")
	files, _ := ioutil.ReadDir(".")
	for _, file := range files {
		if strings.HasPrefix(file.Name(), "data-mock-") {
			os.RemoveAll(file.Name())
		}
	}
	fmt.Printf("OK\n")
}

type mockServer struct {
	port int
	join string
	n    *finn.Node
	m    *Machine
	conn redis.Conn
}

func (s *mockServer) Close() {
	if s.conn != nil {
		s.conn.Close()
	}
	s.m.Close()
	s.n.Close()
}
func (s *mockServer) Do(commandName string, args ...interface{}) (interface{}, error) {
	resps, err := s.DoPipeline([][]interface{}{
		append([]interface{}{commandName}, args...),
	})
	if err != nil {
		return nil, err
	}
	if len(resps) != 1 {
		return nil, errors.New("invalid number or responses")
	}
	return resps[0], nil
}

func (s *mockServer) DoPipeline(cmds [][]interface{}) ([]interface{}, error) {
	if s.conn == nil {
		var err error
		s.conn, err = redis.Dial("tcp", fmt.Sprintf(":%d", s.port))
		if err != nil {
			return nil, err
		}
	}
	//defer conn.Close()
	for _, cmd := range cmds {
		if err := s.conn.Send(cmd[0].(string), cmd[1:]...); err != nil {
			return nil, err
		}
	}
	if err := s.conn.Flush(); err != nil {
		return nil, err
	}
	var resps []interface{}
	for i := 0; i < len(cmds); i++ {
		resp, err := s.conn.Receive()
		if err != nil {
			resps = append(resps, err)
		} else {
			resps = append(resps, resp)
		}
	}
	return resps, nil
}

func (s *mockServer) waitForStartup() error {
	var lerr error
	start := time.Now()
	for {
		if time.Now().Sub(start) > time.Second*5 {
			if lerr != nil {
				return lerr
			}
			return errTimeout
		}
		if s.join == "" {
			resp, err := redis.String(s.Do("SET", "please", "allow"))
			if err != nil {
				lerr = err
			} else if resp != "OK" {
				lerr = errors.New("not OK")
			} else {
				resp, err := redis.Int(s.Do("DEL", "please"))
				if err != nil {
					lerr = err
				} else if resp != 1 {
					lerr = errors.New("not 1")
				} else {
					return nil
				}
			}
		} else {
			_, err := redis.String(s.Do("SET", "please", "allow"))
			if err == nil {
				lerr = fmt.Errorf("not TRY")
			} else if !strings.HasPrefix(err.Error(), "TRY ") {
				lerr = err
			} else {
				return nil
			}
		}
		time.Sleep(time.Millisecond * 100)
	}
}

func mockOpenServer(join *mockServer) (*mockServer, error) {
	rand.Seed(time.Now().UnixNano())
	port := rand.Int()%20000 + 20000
	dir := fmt.Sprintf("data-mock-%d", port)
	fmt.Printf("Starting test server at port %d\n", port)
	logOutput := ioutil.Discard
	if os.Getenv("PRINTLOG") == "1" {
		logOutput = os.Stderr
	}
	var opts finn.Options
	opts.Backend = finn.FastLog
	opts.Durability = finn.High
	opts.Consistency = finn.High
	opts.LogLevel = finn.Debug
	opts.LogOutput = logOutput
	addr := fmt.Sprintf(":%d", port)
	m, err := New(redlog.New(logOutput).Sub('M'), addr)
	if err != nil {
		return nil, err
	}
	opts.ConnAccept = func(conn redcon.Conn) bool {
		return m.ConnAccept(conn)
	}
	opts.ConnClosed = func(conn redcon.Conn, err error) {
		m.ConnClosed(conn, err)
	}
	var joinAddr string
	if join != nil {
		joinAddr = fmt.Sprintf(":%d", join.port)
	}
	// open the raft machine
	n, err := finn.Open(dir, addr, joinAddr, m, &opts)
	if err != nil {
		m.Close()
		return nil, err
	}
	s := &mockServer{port: port, n: n, m: m, join: joinAddr}
	if err := s.waitForStartup(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

type mockCluster struct {
	ss []*mockServer
	cs *mockServer // current server
}

func mockOpenCluster(count int) (*mockCluster, error) {
	fmt.Printf("Starting Raft cluster of %d servers\n", count)
	var ss []*mockServer
	for i := 0; i < 3; i++ {
		var l *mockServer
		if i > 0 {
			l = ss[0]
		}
		s, err := mockOpenServer(l)
		if err != nil {
			i--
			for ; i >= 0; i-- {
				s.Close()
			}
			return nil, err
		}
		ss = append(ss, s)
	}
	return &mockCluster{ss: ss}, nil
}
func (mc *mockCluster) ResetConn() {
	if mc.cs != nil {
		if mc.cs.conn != nil {
			mc.cs.conn.Close()
			mc.cs.conn = nil
		}
		mc.cs = nil
	}
}

func (mc *mockCluster) ServerForPort(port int) *mockServer {
	for _, s := range mc.ss {
		if s.port == port {
			return s
		}
	}
	return nil
}

func (mc *mockCluster) Do(commandName string, args ...interface{}) (interface{}, error) {
	s := mc.cs
	if s == nil {
		s = mc.ss[rand.Int()%len(mc.ss)]
	}
	for {
		mc.cs = s
		resp, err := s.Do(commandName, args...)
		if rerr, ok := resp.(error); ok && err == nil {
			err = rerr
		}
		if err != nil {
			if strings.HasPrefix(err.Error(), "TRY ") {
				n, err := strconv.ParseInt(err.Error()[5:], 10, 64)
				if err != nil {
					return nil, err
				}
				s = mc.ServerForPort(int(n))
				continue
			}
			return nil, err
		}
		return resp, err
	}
}

func (mc *mockCluster) DoBatch(commands [][]interface{}) error {
	for i := 0; i < len(commands); i += 2 {
		cmds := commands[i]
		if dur, ok := cmds[0].(time.Duration); ok {
			time.Sleep(dur)
		} else {
			if err := mc.DoExpect(commands[i+1][0], cmds[0].(string), cmds[1:]...); err != nil {
				return err
			}
		}
	}
	return nil
}
func normalize(v interface{}) interface{} {
	switch v := v.(type) {
	default:
		return v
	case []interface{}:
		for i := 0; i < len(v); i++ {
			v[i] = normalize(v[i])
		}
	case []uint8:
		return string(v)
	}
	return v
}
func (mc *mockCluster) DoExpect(expect interface{}, commandName string, args ...interface{}) error {
	resp, err := mc.Do(commandName, args...)
	if err != nil {
		if exs, ok := expect.(string); ok {
			if err.Error() == exs {
				return nil
			}
		}
		return err
	}
	resp = normalize(resp)
	if expect == nil && resp != nil {
		return fmt.Errorf("expected '%v', got '%v'", expect, resp)
	}
	if vv, ok := resp.([]interface{}); ok {
		var ss []string
		for _, v := range vv {
			if v == nil {
				ss = append(ss, "nil")
			} else if s, ok := v.(string); ok {
				ss = append(ss, s)
			} else if b, ok := v.([]uint8); ok {
				if b == nil {
					ss = append(ss, "nil")
				} else {
					ss = append(ss, string(b))
				}
			} else {
				ss = append(ss, fmt.Sprintf("%v", v))
			}
		}
		resp = ss
	}
	if b, ok := resp.([]uint8); ok {
		if b == nil {
			resp = nil
		} else {
			resp = string([]byte(b))
		}
	}
	if fn, ok := expect.(func(v interface{}) (resp, expect interface{})); ok {
		resp, expect = fn(resp)
	}
	if fmt.Sprintf("%v", resp) != fmt.Sprintf("%v", expect) {
		return fmt.Errorf("expected '%v', got '%v'", expect, resp)
	}
	return nil
}

func (mc *mockCluster) Close() {
	for _, s := range mc.ss {
		s.Close()
	}
}

func round(v float64, decimals int) float64 {
	var pow float64 = 1
	for i := 0; i < decimals; i++ {
		pow *= 10
	}
	return float64(int((v*pow)+0.5)) / pow
}

func exfloat(v float64, decimals int) func(v interface{}) (resp, expect interface{}) {
	ex := round(v, decimals)
	return func(v interface{}) (resp, expect interface{}) {
		var s string
		if b, ok := v.([]uint8); ok {
			s = string(b)
		} else {
			s = fmt.Sprintf("%v", v)
		}
		n, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return v, ex
		}
		return round(n, decimals), ex
	}
}
