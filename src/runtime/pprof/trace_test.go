// Copyright 2014 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pprof_test

import (
	"bytes"
	"internal/trace"
	"io"
	"net"
	"os"
	"runtime"
	. "runtime/pprof"
	"sync"
	"testing"
	"time"
)

func skipTraceTestsIfNeeded(t *testing.T) {
	switch runtime.GOOS {
	case "solaris":
		t.Skip("skipping: solaris timer can go backwards (http://golang.org/issue/8976)")
	}

	switch runtime.GOARCH {
	case "arm":
		t.Skip("skipping: arm tests fail with 'failed to parse trace' (http://golang.org/issue/9725)")
	}
}

func TestTraceStartStop(t *testing.T) {
	skipTraceTestsIfNeeded(t)
	buf := new(bytes.Buffer)
	if err := StartTrace(buf); err != nil {
		t.Fatalf("failed to start tracing: %v", err)
	}
	StopTrace()
	size := buf.Len()
	if size == 0 {
		t.Fatalf("trace is empty")
	}
	time.Sleep(100 * time.Millisecond)
	if size != buf.Len() {
		t.Fatalf("trace writes after stop: %v -> %v", size, buf.Len())
	}
}

func TestTraceDoubleStart(t *testing.T) {
	skipTraceTestsIfNeeded(t)
	StopTrace()
	buf := new(bytes.Buffer)
	if err := StartTrace(buf); err != nil {
		t.Fatalf("failed to start tracing: %v", err)
	}
	if err := StartTrace(buf); err == nil {
		t.Fatalf("succeed to start tracing second time")
	}
	StopTrace()
	StopTrace()
}

func TestTrace(t *testing.T) {
	skipTraceTestsIfNeeded(t)
	buf := new(bytes.Buffer)
	if err := StartTrace(buf); err != nil {
		t.Fatalf("failed to start tracing: %v", err)
	}
	StopTrace()
	_, err := trace.Parse(buf)
	if err != nil {
		t.Fatalf("failed to parse trace: %v", err)
	}
}

func parseTrace(r io.Reader) ([]*trace.Event, map[uint64]*trace.GDesc, error) {
	events, err := trace.Parse(r)
	if err != nil {
		return nil, nil, err
	}
	gs := trace.GoroutineStats(events)
	for goid := range gs {
		// We don't do any particular checks on the result at the moment.
		// But still check that RelatedGoroutines does not crash, hang, etc.
		_ = trace.RelatedGoroutines(events, goid)
	}
	return events, gs, nil
}

func TestTraceStress(t *testing.T) {
	skipTraceTestsIfNeeded(t)

	var wg sync.WaitGroup
	done := make(chan bool)

	// Create a goroutine blocked before tracing.
	wg.Add(1)
	go func() {
		<-done
		wg.Done()
	}()

	// Create a goroutine blocked in syscall before tracing.
	rp, wp, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create pipe: %v", err)
	}
	defer func() {
		rp.Close()
		wp.Close()
	}()
	wg.Add(1)
	go func() {
		var tmp [1]byte
		rp.Read(tmp[:])
		<-done
		wg.Done()
	}()
	time.Sleep(time.Millisecond) // give the goroutine above time to block

	buf := new(bytes.Buffer)
	if err := StartTrace(buf); err != nil {
		t.Fatalf("failed to start tracing: %v", err)
	}

	procs := runtime.GOMAXPROCS(10)
	time.Sleep(50 * time.Millisecond) // test proc stop/start events

	go func() {
		runtime.LockOSThread()
		for {
			select {
			case <-done:
				return
			default:
				runtime.Gosched()
			}
		}
	}()

	runtime.GC()
	// Trigger GC from malloc.
	for i := 0; i < 1e3; i++ {
		_ = make([]byte, 1<<20)
	}

	// Create a bunch of busy goroutines to load all Ps.
	for p := 0; p < 10; p++ {
		wg.Add(1)
		go func() {
			// Do something useful.
			tmp := make([]byte, 1<<16)
			for i := range tmp {
				tmp[i]++
			}
			_ = tmp
			<-done
			wg.Done()
		}()
	}

	// Block in syscall.
	wg.Add(1)
	go func() {
		var tmp [1]byte
		rp.Read(tmp[:])
		<-done
		wg.Done()
	}()

	// Test timers.
	timerDone := make(chan bool)
	go func() {
		time.Sleep(time.Millisecond)
		timerDone <- true
	}()
	<-timerDone

	// A bit of network.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		time.Sleep(time.Millisecond)
		var buf [1]byte
		c.Write(buf[:])
		c.Close()
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	var tmp [1]byte
	c.Read(tmp[:])
	c.Close()

	go func() {
		runtime.Gosched()
		select {}
	}()

	// Unblock helper goroutines and wait them to finish.
	wp.Write(tmp[:])
	wp.Write(tmp[:])
	close(done)
	wg.Wait()

	runtime.GOMAXPROCS(procs)

	StopTrace()
	_, _, err = parseTrace(buf)
	if err != nil {
		t.Fatalf("failed to parse trace: %v", err)
	}
}

// Do a bunch of various stuff (timers, GC, network, etc) in a separate goroutine.
// And concurrently with all that start/stop trace 3 times.
func TestTraceStressStartStop(t *testing.T) {
	skipTraceTestsIfNeeded(t)

	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(8))
	outerDone := make(chan bool)

	go func() {
		defer func() {
			outerDone <- true
		}()

		var wg sync.WaitGroup
		done := make(chan bool)

		wg.Add(1)
		go func() {
			<-done
			wg.Done()
		}()

		rp, wp, err := os.Pipe()
		if err != nil {
			t.Fatalf("failed to create pipe: %v", err)
		}
		defer func() {
			rp.Close()
			wp.Close()
		}()
		wg.Add(1)
		go func() {
			var tmp [1]byte
			rp.Read(tmp[:])
			<-done
			wg.Done()
		}()
		time.Sleep(time.Millisecond)

		go func() {
			runtime.LockOSThread()
			for {
				select {
				case <-done:
					return
				default:
					runtime.Gosched()
				}
			}
		}()

		runtime.GC()
		// Trigger GC from malloc.
		for i := 0; i < 1e3; i++ {
			_ = make([]byte, 1<<20)
		}

		// Create a bunch of busy goroutines to load all Ps.
		for p := 0; p < 10; p++ {
			wg.Add(1)
			go func() {
				// Do something useful.
				tmp := make([]byte, 1<<16)
				for i := range tmp {
					tmp[i]++
				}
				_ = tmp
				<-done
				wg.Done()
			}()
		}

		// Block in syscall.
		wg.Add(1)
		go func() {
			var tmp [1]byte
			rp.Read(tmp[:])
			<-done
			wg.Done()
		}()

		runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))

		// Test timers.
		timerDone := make(chan bool)
		go func() {
			time.Sleep(time.Millisecond)
			timerDone <- true
		}()
		<-timerDone

		// A bit of network.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen failed: %v", err)
		}
		defer ln.Close()
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			time.Sleep(time.Millisecond)
			var buf [1]byte
			c.Write(buf[:])
			c.Close()
		}()
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial failed: %v", err)
		}
		var tmp [1]byte
		c.Read(tmp[:])
		c.Close()

		go func() {
			runtime.Gosched()
			select {}
		}()

		// Unblock helper goroutines and wait them to finish.
		wp.Write(tmp[:])
		wp.Write(tmp[:])
		close(done)
		wg.Wait()
	}()

	for i := 0; i < 3; i++ {
		buf := new(bytes.Buffer)
		if err := StartTrace(buf); err != nil {
			t.Fatalf("failed to start tracing: %v", err)
		}
		time.Sleep(time.Millisecond)
		StopTrace()
		if _, _, err := parseTrace(buf); err != nil {
			t.Fatalf("failed to parse trace: %v", err)
		}
	}
	<-outerDone
}
