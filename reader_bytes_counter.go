package otelchi

import (
	"io"
	"sync/atomic"
)

type readerBytesCounter struct {
	io.ReadCloser
	writtenBytes int64
}

func (counter *readerBytesCounter) Read(buf []byte) (int, error) {
	n, err := counter.ReadCloser.Read(buf)
	atomic.AddInt64(&counter.writtenBytes, int64(n))
	return n, err
}

func (counter *readerBytesCounter) Close() error {
	return counter.ReadCloser.Close()
}

func (counter *readerBytesCounter) bytes() int64 {
	return atomic.LoadInt64(&counter.writtenBytes)
}
