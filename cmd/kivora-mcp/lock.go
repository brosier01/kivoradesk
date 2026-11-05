package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// actionLock lets one acting tool run at a time, in this process and in the other
// MCP sessions of the VM: their clicks and keys would mix on the same desktop. The
// lock on the file goes with the process.
type actionLock struct {
	path string // empty for this process only

	once sync.Once
	turn chan struct{}
	file *os.File
}

func (l *actionLock) acquire(ctx context.Context) (release func(), err error) {
	l.once.Do(func() {
		l.turn = make(chan struct{}, 1)
		if l.path == "" {
			return
		}
		// Without the file, on a full disk, only this session is kept in order.
		f, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			log.Printf("action lock: %v", err)
			return
		}
		l.file = f
	})
	busy := func() error {
		return fmt.Errorf("another action is still running, in this session or another one: %w", ctx.Err())
	}
	select {
	case l.turn <- struct{}{}:
	case <-ctx.Done():
		return nil, busy()
	}
	if l.file != nil {
		for {
			err := unix.Flock(int(l.file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
			if err == nil {
				break
			}
			if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
				<-l.turn
				return nil, fmt.Errorf("action lock: %w", err)
			}
			select {
			case <-ctx.Done():
				<-l.turn
				return nil, busy()
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	return func() {
		if l.file != nil {
			unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
		}
		<-l.turn
	}, nil
}
