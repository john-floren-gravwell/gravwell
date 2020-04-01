/*************************************************************************
 * Copyright 2020 Gravwell, Inc. All rights reserved.
 * Contact: <legal@gravwell.io>
 *
 * This software may be modified and distributed under the terms of the
 * BSD 2-clause license. See the LICENSE file for details.
 **************************************************************************/

// Package chancacher implements a pipeline of channels (in->out) that
// provides internal buffering (via a simple buffered channel), and caching
// data to disk.
package chancacher

import (
	"encoding/gob"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The maximum channel depth, which is also used when the channel depth is set
// to 0. We could set this to MaxInt but we'd likely just run out of memory
// without a clean way to triage. It's best to just enforce a sensible maximum.
const MaxDepth = 1000000

const TIMEOUT = time.Second

// A ChanCacher is a pipeline of channels with a variable-sized internal
// buffer. The buffer can also cache to disk. The user is expected to connect
// ChanCacher.In and ChanCacher.Out.
type ChanCacher struct {
	In      chan interface{}
	Out     chan interface{}
	runDone bool

	cachePath      string
	cache          bool
	cacheR         *os.File
	cacheW         *os.File
	cacheEnc       *gob.Encoder
	cacheModified  bool
	cacheLock      sync.Mutex
	cacheReading   bool
	cachePaused    chan bool
	cacheDone      chan bool
	cacheAck       chan bool
	cacheIsDone    bool
	cacheCommitted bool
}

// Create a new ChanCacher with maximum depth, and optional backing file.  If
// maxDepth == 0, the ChanCacher will be unbuffered. If maxDepth == -1, the
// ChanCacher depth will be set to MaxDepth. To enable a backing store,
// provide a path to backingPath. chancachers create two files using this
// prefix named cache_a and cache_b.
//
// When a new ChanCacher is made, if cachePath points to existing cache files,
// the ChanCacher will immediately attempt to drain them from disk. In this
// way, you can recover data sent to disk on a crash or previous use of
// Commit().
func NewChanCacher(maxDepth int, cachePath string) *ChanCacher {
	// as close to infinite as possible...
	if maxDepth == -1 || maxDepth > MaxDepth {
		maxDepth = MaxDepth
	}
	c := &ChanCacher{
		In:          make(chan interface{}),
		Out:         make(chan interface{}, maxDepth),
		cachePath:   cachePath,
		cache:       cachePath != "",
		cachePaused: make(chan bool),
		cacheDone:   make(chan bool),
		cacheAck:    make(chan bool),
	}

	// we start the cache unpaused, and because of go idioms, we have to
	// make the channel in order for "closed" states to work - we can't
	// just leave it initiated...
	close(c.cachePaused)

	go c.run()
	if c.cache {
		var err error

		err = os.MkdirAll(c.cachePath, 0755)
		if err != nil {
			// TODO: log
			return nil
		}

		// create r and w files
		//c.cacheR, err = os.Create(filepath.Join(c.cachePath, "cache_a"))
		c.cacheR, err = os.OpenFile(filepath.Join(c.cachePath, "cache_a"), os.O_APPEND|os.O_CREATE|os.O_RDWR, 0644)
		if err != nil {
			// TODO: log
		}

		//c.cacheW, err = os.Create(filepath.Join(c.cachePath, "cache_b"))
		c.cacheW, err = os.OpenFile(filepath.Join(c.cachePath, "cache_b"), os.O_APPEND|os.O_CREATE|os.O_RDWR, 0644)
		if err != nil {
			// TODO: log
		}

		c.cacheEnc = gob.NewEncoder(c.cacheW)

		// if the write cache data data in it already (recover), then
		// mark the cache as modified.
		fi, err := c.cacheW.Stat()
		if err != nil {
			// TODO: log
		}
		if fi.Size() != 0 {
			c.cacheModified = true
		}

		go c.cacheHandler()
	}
	return c
}

// run connects in->out channels, watching the depth on out. When out is full,
// we block on reads from in. Optionally, we redirect input to a backing store
// with gob, and continue reading from in indefinitely. When the backing store
// is enabled, we end up plumbing in->cache->out.
func (c *ChanCacher) run() {
	for v := range c.In {
		select {
		case c.Out <- v:
		default:
			// The buffer is full. If we're not caching, just
			// block on putting the value into the buffer
			if !c.cache {
				c.Out <- v
			} else {
				// select on putting the value into out and
				// checking the paused state. This allows us to
				// block until the cache unpauses or the buffer
				// drains, whichever comes first.
				select {
				case c.Out <- v:
				case <-c.cachePaused:
					c.cacheValue(v)
				}
			}
		}
	}

	c.runDone = true

	if c.cache {
		// closing c.In stops reading input, but we allow the cache to drain
		// before closing c.Out.
		for c.CacheHasData() && !c.cacheCommitted {
			time.Sleep(100 * time.Millisecond)
		}

		// stop cacheHandler()
		c.finishCache()

		// verify the cache reader has stopped trying to write to c.Out
		<-c.cacheAck
	}

	// Buffered channels allow reading data until they're empty, even if
	// close, so we just close and move on.
	close(c.Out)
}

func (c *ChanCacher) cacheHandler() {
	// the main cache loop. We read from R, putting data into out directly
	// until R is drained. Once R is drained, wait for W to have data and
	// for run() to signal that we can swap buffers.
	c.cacheReading = true
	for {
		var err error

		dec := gob.NewDecoder(c.cacheR)
		var v interface{}
		for {
			err = dec.Decode(&v)
			if err != nil {
				break
			}
			if v == nil {
				continue
			}

			c.Out <- v
		}
		if err != io.EOF {
			// TODO: log
		}

		c.cacheReading = false

		// This is the only place where CacheHasData() will return false

		select {
		case <-c.cacheDone:
			close(c.cacheAck)
			return
		default:
		}

		c.cacheR.Seek(0, 0)
		c.cacheR.Truncate(0)

		// Wait for W to have data.
		for !c.cacheModified {
			select {
			case <-c.cacheDone:
				close(c.cacheAck)
				return
			case <-time.After(time.Second):
			}
		}

		// swap caches
		c.cacheLock.Lock()
		c.cacheR, c.cacheW = c.cacheW, c.cacheR
		c.cacheR.Seek(0, 0)
		c.cacheEnc = gob.NewEncoder(c.cacheW)
		c.cacheModified = false
		c.cacheReading = true
		c.cacheLock.Unlock()
	}
}

func (c *ChanCacher) cacheValue(v interface{}) {
	c.cacheLock.Lock()
	defer c.cacheLock.Unlock()
	err := c.cacheEnc.Encode(&v)
	if err != nil {
		// TODO: log
	}
	c.cacheModified = true
}

// Return if the cache has outstanding data not written to the output channel.
func (c *ChanCacher) CacheHasData() bool {
	return c.cacheModified || c.cacheReading
}

// Returns the number of elements on the internal buffer.
func (c *ChanCacher) BufferSize() int {
	return len(c.Out)
}

// Enable a stopped cache.
func (c *ChanCacher) CacheStart() {
	if !c.cache {
		return
	}
	c.cacheLock.Lock()
	defer c.cacheLock.Unlock()
	select {
	case <-c.cachePaused:
	default:
		close(c.cachePaused)
	}
}

// Stop a running cache. Calling Stop() will prevent the ChanCacher from
// writing any new data to the backing file, but will not stop it from reading
// (draining) the cache to the output channel.
func (c *ChanCacher) CacheStop() {
	if !c.cache {
		return
	}
	c.cacheLock.Lock()
	defer c.cacheLock.Unlock()
	select {
	case <-c.cachePaused:
		c.cachePaused = make(chan bool)
	default:
	}
}

// Drain blocks until the internal buffer is empty. It's possible that new data
// is still being consumed, so care should be taken when using Drain(). You
// probably don't want to use Drain(), but instead close ChanCacher.In and wait
// for the ChanCacher.Out to close, which does carry guarantees that the
// internal buffers and cache are fully drained.
func (c *ChanCacher) Drain() {
	for len(c.Out) != 0 {
		time.Sleep(100 * time.Millisecond)
	}
}

// Commit drains the buffer to the backing file and shuts down the cache.
// Commit should be called after closing the input channel if the buffer needs
// to be saved. Commit will block until the In channel is closed. The
// ChanCacher will not close the output channel until it's empty, so a typical
// production would look like:
//	close(c.In)
//	drainSomeDataFrom(c.Out)
//
//	// commit the rest of my data to disk
//	c.Commit()
//
//	// c.Out is now closed
//
// Once Commit() is called, draining the cache cannot be restarted, though
// writing to the cache will still work. Commit should only be used for teardown
// scenarios.
func (c *ChanCacher) Commit() {
	if !c.cache {
		c.cacheCommitted = true
		return
	}

	c.finishCache()

	// read from out and write back to the cache
	readerStopped := false
	for !c.runDone || len(c.Out) != 0 || !readerStopped {
		select {
		case <-c.cacheAck:
			readerStopped = true
		case v := <-c.Out:
			c.cacheValue(v)
		}
	}

	c.cacheR.Close()
	c.cacheW.Close()

	c.cacheCommitted = true
}

func (c *ChanCacher) finishCache() {
	c.cacheLock.Lock()
	defer c.cacheLock.Unlock()

	if !c.cacheIsDone {
		close(c.cacheDone)
		c.cacheIsDone = true
	}
}
