package node

import (
	"errors"
	"io"
	"sync"
)

type runtimeTrackedRWC struct {
	inner      io.ReadWriteCloser
	onRead     func(int)
	onWrite    func(int)
	writeLimit func() *runtimeLimiter
	closeOnce  sync.Once
}

func newRuntimeTrackedRWC(inner io.ReadWriteCloser, onRead, onWrite func(int), writeLimit func() *runtimeLimiter) *runtimeTrackedRWC {
	return &runtimeTrackedRWC{inner: inner, onRead: onRead, onWrite: onWrite, writeLimit: writeLimit}
}

func (c *runtimeTrackedRWC) Read(p []byte) (int, error) {
	n, err := c.inner.Read(p)
	if n > 0 && c.onRead != nil {
		c.onRead(n)
	}
	return n, err
}

func (c *runtimeTrackedRWC) Write(p []byte) (int, error) {
	if c.writeLimit != nil {
		if limit := c.writeLimit(); limit != nil {
			if err := limit.wait(len(p)); err != nil {
				return 0, err
			}
		}
	}
	n, err := c.inner.Write(p)
	if n > 0 && c.onWrite != nil {
		c.onWrite(n)
	}
	return n, err
}

func (c *runtimeTrackedRWC) Close() error {
	var err error
	c.closeOnce.Do(func() { err = c.inner.Close() })
	return err
}

func (c *runtimeTrackedRWC) CloseWrite() error {
	if closer, ok := c.inner.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}
	return errors.New("tracked endpoint does not support write half-close")
}

func (c *runtimeTrackedRWC) Abort() error {
	if aborter, ok := c.inner.(interface{ Abort() error }); ok {
		return aborter.Abort()
	}
	return c.Close()
}
