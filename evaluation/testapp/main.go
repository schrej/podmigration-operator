package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	listenAddr = "0.0.0.0:8080"
	parallel   = 10
)

var (
	mem    [][1000000]byte
	tstart time.Time
)

func main() {
	tstart = time.Now()
	delay := flag.Int("d", 0, "A artifical delay for the startup")
	alloc := flag.Int("m", 0, "Amount of memory in MB to allocate artificially")
	flag.Parse()

	if alloc != nil && *alloc > 0 {
		log("allocating %vMB of memory", *alloc)
		mem = make([][1000000]byte, *alloc)
		bs := (*alloc + parallel - 1) / parallel
		wg := sync.WaitGroup{}
		wg.Add(parallel)
		for p := 0; p < parallel; p++ {
			start := p * bs
			go func(start, end int) {
				for i := start; i < end; i++ {
					mem[i] = [1000000]byte{}
				}
				wg.Done()
			}(start, min(start+bs, len(mem)))
		}
		wg.Wait()
		log("memory allocated")
	}

	if delay != nil {
		log("applying delay: %d", *delay)
		dt := time.Duration(*delay)*time.Second - time.Since(tstart)
		if dt < 0 {
			log("WARNING: memory allocation took %s longer than delay.", (-dt).Round(time.Millisecond).String())
		} else {
			log("sleeping for remaining delay: %s", dt.Round(time.Millisecond).String())
			time.Sleep(dt)
		}
	}

	log("startup finished, starting http server on %s", listenAddr)
	l, err := net.Listen("tcp", listenAddr)
	if err != nil {
		panic(err)
	}
	counter := 0
	if err = http.Serve(l, http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		counter++
		_, err := rw.Write([]byte(strconv.Itoa(counter) + "\n"))
		if err != nil {
			panic(err)
		}
	})); err != nil {
		panic(err)
	}
}

func log(msg string, args ...interface{}) {
	fmt.Printf("["+time.Since(tstart).Round(time.Millisecond).String()+"] "+msg+"\n", args...)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
