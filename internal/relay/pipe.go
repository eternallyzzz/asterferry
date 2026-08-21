package relay

import (
	"io"
	"sync"
)

type Counters struct{ In, Out func(uint64) }

const copyBufferSize = 32 * 1024

var copyBuffers = sync.Pool{New: func() any {
	buf := make([]byte, copyBufferSize)
	return &buf
}}

func Bidirectional(a, b io.ReadWriteCloser, counters Counters) {
	var wg sync.WaitGroup
	copyOne := func(dst io.WriteCloser, src io.Reader, count func(uint64)) {
		defer wg.Done()
		bufPtr := copyBuffers.Get().(*[]byte)
		buf := *bufPtr
		defer copyBuffers.Put(bufPtr)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				written := 0
				var werr error
				for written < n {
					countN, err := dst.Write(buf[written:n])
					if countN > 0 {
						written += countN
					}
					if err != nil || countN == 0 {
						werr = err
						if werr == nil {
							werr = io.ErrShortWrite
						}
						break
					}
				}
				if flusher, ok := dst.(interface{ Flush() }); ok {
					flusher.Flush()
				}
				if count != nil {
					count(uint64(written))
				}
				if werr != nil {
					halfCloseWrite(dst)
					halfCloseRead(src)
					return
				}
			}
			if err != nil {
				halfCloseWrite(dst)
				halfCloseRead(src)
				return
			}
		}
	}
	wg.Add(2)
	go copyOne(a, b, counters.In)
	go copyOne(b, a, counters.Out)
	wg.Wait()
	_ = a.Close()
	_ = b.Close()
}

func halfCloseWrite(v io.Writer) {
	switch x := v.(type) {
	case interface{ CloseWrite() }:
		x.CloseWrite()
	case interface{ CloseWrite() error }:
		_ = x.CloseWrite()
	case io.Closer:
		_ = x.Close()
	}
}

func halfCloseRead(v io.Reader) {
	switch x := v.(type) {
	case interface{ CloseRead() }:
		x.CloseRead()
	case interface{ CloseRead() error }:
		_ = x.CloseRead()
	}
}
