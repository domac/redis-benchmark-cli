package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var ip = flag.String("ip", "127.0.0.1", "redis/ledis/ssdb server ip")
var port = flag.Int("port", 6380, "redis/ledis/ssdb server port")
var number = flag.Int("n", 1000, "request number")
var clients = flag.Int("c", 50, "number of clients")
var round = flag.Int("r", 1, "benchmark round number")
var valueSize = flag.Int("vsize", 100, "kv value size")
var pipeline = flag.Int("p", 0, "pipeline bulk")
var tests = flag.String("t", "set,get,hset,hget", "only run the comma separated list of tests")

//string convert
type argInt []int

// get int by index from int slice
func (a argInt) Get(i int, args ...int) (r int) {
	if i >= 0 && i < len(a) {
		r = a[i]
	}
	if len(args) > 0 {
		r = args[0]
	}
	return
}

// ToStr interface to string
func ToStr(value interface{}, args ...int) (s string) {
	switch v := value.(type) {
	case bool:
		s = strconv.FormatBool(v)
	case float32:
		s = strconv.FormatFloat(float64(v), 'f', argInt(args).Get(0, -1), argInt(args).Get(1, 32))
	case float64:
		s = strconv.FormatFloat(v, 'f', argInt(args).Get(0, -1), argInt(args).Get(1, 64))
	case int:
		s = strconv.FormatInt(int64(v), argInt(args).Get(0, 10))
	case int8:
		s = strconv.FormatInt(int64(v), argInt(args).Get(0, 10))
	case int16:
		s = strconv.FormatInt(int64(v), argInt(args).Get(0, 10))
	case int32:
		s = strconv.FormatInt(int64(v), argInt(args).Get(0, 10))
	case int64:
		s = strconv.FormatInt(v, argInt(args).Get(0, 10))
	case uint:
		s = strconv.FormatUint(uint64(v), argInt(args).Get(0, 10))
	case uint8:
		s = strconv.FormatUint(uint64(v), argInt(args).Get(0, 10))
	case uint16:
		s = strconv.FormatUint(uint64(v), argInt(args).Get(0, 10))
	case uint32:
		s = strconv.FormatUint(uint64(v), argInt(args).Get(0, 10))
	case uint64:
		s = strconv.FormatUint(v, argInt(args).Get(0, 10))
	case string:
		s = v
	case []byte:
		s = string(v)
	default:
		s = fmt.Sprintf("%v", v)
	}
	return s
}

func readResp(rd *bufio.Reader, n int, opts *Options) error {
	for i := 0; i < n; i++ {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			return err
		}
		switch line[0] {
		default:
			return errors.New("invalid server response")
		case '+', ':':
		case '-':
			opts.Stderr.Write(line)
		case '$':
			n, err := strconv.ParseInt(string(line[1:len(line)-2]), 10, 64)
			if err != nil {
				return err
			}
			if n >= 0 {
				if _, err = io.CopyN(ioutil.Discard, rd, n+2); err != nil {
					return err
				}
			}
		case '*':
			n, err := strconv.ParseInt(string(line[1:len(line)-2]), 10, 64)
			if err != nil {
				return err
			}
			readResp(rd, int(n), opts)
		}
	}
	return nil
}

type Options struct {
	Requests int
	Clients  int
	Pipeline int
	Quiet    bool
	CSV      bool
	Stdout   io.Writer
	Stderr   io.Writer
}

var DefaultOptions = &Options{
	Requests: 100000,
	Clients:  50,
	Pipeline: 1,
	Quiet:    false,
	CSV:      false,
	Stdout:   os.Stdout,
	Stderr:   os.Stderr,
}

//压测发起
func Bench(
	name string,
	addr string,
	opts *Options,
	prep func(conn net.Conn) bool,
	fill func(buf []byte) []byte,
) {
	if opts == nil {
		opts = DefaultOptions
	}
	if opts.Stderr == nil {
		opts.Stderr = ioutil.Discard
	}
	if opts.Stdout == nil {
		opts.Stdout = ioutil.Discard
	}

	//相关统计信息
	var totalPayload uint64
	var count uint64
	var duration int64
	rpc := opts.Requests / opts.Clients
	rpcex := opts.Requests % opts.Clients
	var tstop int64
	remaining := int64(opts.Clients)

	errs := make([]error, opts.Clients)
	durs := make([][]time.Duration, opts.Clients)
	conns := make([]net.Conn, opts.Clients)

	for i := 0; i < opts.Clients; i++ {
		crequests := rpc //每个客户端需要负责请求命令的数目
		if i == opts.Clients-1 {
			crequests += rpcex
		}
		durs[i] = make([]time.Duration, crequests)
		for j := 0; j < len(durs[i]); j++ {
			durs[i][j] = -1
		}
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			if i == 0 {
				fmt.Fprintf(opts.Stderr, "%s\n", err.Error())
				return
			}
			errs[i] = err
		}
		if conn != nil && prep != nil {
			if !prep(conn) {
				conn.Close()
				conn = nil
			}
		}
		conns[i] = conn
	}

	//统计开始
	tstart := time.Now()
	for i := 0; i < opts.Clients; i++ {
		crequests := rpc
		if i == opts.Clients-1 {
			crequests += rpcex
		}

		go func(conn net.Conn, client, crequests int) {
			defer func() {
				atomic.AddInt64(&remaining, -1)
			}()
			if conn == nil {
				return
			}
			err := func() error {
				var buf []byte

				//conn buff reader
				rd := bufio.NewReader(conn)

				//pipeline处理
				for i := 0; i < crequests; i += opts.Pipeline {
					n := opts.Pipeline
					if i+n > crequests {
						n = crequests - i
					}

					//清除上一次的buffer
					buf = buf[:0]
					for i := 0; i < n; i++ {
						//返回封装的redis协议信息
						buf = fill(buf)
					}
					atomic.AddUint64(&totalPayload, uint64(len(buf)))
					start := time.Now()
					_, err := conn.Write(buf)
					if err != nil {
						return err
					}

					//获取响应信息
					if err := readResp(rd, n, opts); err != nil {
						return err
					}

					//统计耗时
					stop := time.Since(start)
					for j := 0; j < n; j++ {
						durs[client][i+j] = stop / time.Duration(n)
					}
					atomic.AddInt64(&duration, int64(stop))
					atomic.AddUint64(&count, uint64(n))
					atomic.StoreInt64(&tstop, int64(time.Since(tstart)))
				}
				return nil
			}()
			if err != nil {
				errs[client] = err
			}
		}(conns[i], i, crequests)
	}
	var die bool
	for {
		remaining := int(atomic.LoadInt64(&remaining))
		count := int(atomic.LoadUint64(&count))
		real := time.Duration(atomic.LoadInt64(&tstop))
		totalPayload := int(atomic.LoadUint64(&totalPayload))
		more := remaining > 0
		var realrps float64
		if real > 0 {
			realrps = float64(count) / (float64(real) / float64(time.Second))
		}
		if !opts.CSV {
			fmt.Fprintf(opts.Stdout, "\r%s: %.2f", name, realrps)
			if more {
				fmt.Fprintf(opts.Stdout, "\r")
			} else if opts.Quiet {
				fmt.Fprintf(opts.Stdout, " requests per second\n")
			} else {
				fmt.Fprintf(opts.Stdout, "\r====== %s ======\n", name)
				fmt.Fprintf(opts.Stdout, "  %d requests finish in %.2f seconds\n", opts.Requests, float64(real)/float64(time.Second))
				fmt.Fprintf(opts.Stdout, "  %d requests clients\n", opts.Clients)
				fmt.Fprintf(opts.Stdout, "  %d bytes payload\n", totalPayload/opts.Requests)
				fmt.Fprintf(opts.Stdout, "\n")
				var limit time.Duration
				var lastper float64

				//打印延时百分比
				for {
					limit += time.Millisecond
					var hits, count int
					for i := 0; i < len(durs); i++ {
						for j := 0; j < len(durs[i]); j++ {
							dur := durs[i][j]
							if dur == -1 {
								continue
							}
							if dur < limit {
								hits++
							}
							count++
						}
					}
					per := float64(hits) / float64(count)
					if math.Floor(per*10000) == math.Floor(lastper*10000) {
						continue
					}
					lastper = per
					fmt.Fprintf(opts.Stdout, "%.2f%% <= %d milliseconds\n", per*100, (limit-time.Millisecond)/time.Millisecond)
					if per == 1.0 {
						break
					}
				}
				fmt.Fprintf(opts.Stdout, "%.2f requests per second\n\n", realrps)
			}
		}
		if !more {
			if opts.CSV {
				fmt.Fprintf(opts.Stdout, "\"%s\",\"%.2f\"\n", name, realrps)
			}
			for _, err := range errs {
				if err != nil {
					fmt.Fprintf(opts.Stderr, "%s\n", err)
					die = true
					if count == 0 {
						break
					}
				}
			}
			break
		}
		time.Sleep(time.Second / 5)
	}

	for i := 0; i < len(conns); i++ {
		if conns[i] != nil {
			conns[i].Close()
		}
	}
	if die {
		os.Exit(1)
	}
}

//参考：http://redisdoc.com/topic/protocol.html

func AppendCommand(buf []byte, args ...string) []byte {
	buf = append(buf, '*')
	buf = strconv.AppendInt(buf, int64(len(args)), 10)
	buf = append(buf, '\r', '\n')
	for _, arg := range args {
		buf = append(buf, '$')
		buf = strconv.AppendInt(buf, int64(len(arg)), 10) //mykey
		buf = append(buf, '\r', '\n')
		buf = append(buf, arg...) //myvalue
		buf = append(buf, '\r', '\n')
	}
	return buf
}

func main() {

	runtime.GOMAXPROCS(runtime.NumCPU())

	flag.Parse()

	var kvSetBase int64 = 0
	var kvGetBase int64 = 0
	var hashSetBase int64 = 0
	var hashGetBase int64 = 0

	opt := &Options{
		Requests: *number,
		Clients:  *clients,
		Pipeline: *pipeline,
		Stdout:   os.Stdout,
		Stderr:   os.Stdout,
	}

	ts := strings.Split(*tests, ",")

	addr := fmt.Sprintf("%s:%d", *ip, *port)

	for _, s := range ts {
		switch strings.ToLower(s) {
		case "set":
			Bench("SET", addr, opt, nil, func(buf []byte) []byte {
				value := make([]byte, *valueSize)
				n := atomic.AddInt64(&kvSetBase, 1)

				nstr := ToStr(n)
				vstr := ToStr(value)
				return AppendCommand(buf, "SET", nstr, vstr)
			})
		case "get":
			Bench("GET", addr, opt, nil, func(buf []byte) []byte {
				n := atomic.AddInt64(&kvGetBase, 1)
				nstr := ToStr(n)
				return AppendCommand(buf, "GET", nstr)
			})

		case "hset":
			Bench("HSET", addr, opt, nil, func(buf []byte) []byte {
				value := make([]byte, *valueSize)
				n := atomic.AddInt64(&hashSetBase, 1)

				nstr := ToStr(n)
				vstr := ToStr(value)
				return AppendCommand(buf, "HSET", nstr, vstr)
			})

		case "hget":
			Bench("HGET", addr, opt, nil, func(buf []byte) []byte {
				n := atomic.AddInt64(&hashGetBase, 1)
				nstr := ToStr(n)
				return AppendCommand(buf, "HGET", nstr)
			})
		default:
			panic("unknow cmd")
		}
	}

}
