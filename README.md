# redis-benchmark-cli
command line tool for redis benchmark


```
  -c int
        number of clients (default 50)
  -ip string
        redis/ledis/ssdb server ip (default "127.0.0.1")
  -n int
        request number (default 1000)
  -p int
        pipeline bulk
  -port int
        redis/ledis/ssdb server port (default 6380)
  -r int
        benchmark round number (default 1)
  -t string
        only run the comma separated list of tests (default "set,get,hset,hget")
  -vsize int
        kv value size (default 100)
```