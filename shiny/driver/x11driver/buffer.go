// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package x11driver

import (
	"image"
	"log"
	"sync"
	"unsafe"

	"github.com/BurntSushi/xgb/shm"
	"github.com/BurntSushi/xgb/xproto"

	"golang.org/x/exp/shiny/driver/internal/swizzle"
	"golang.org/x/exp/shiny/screen"
)

type bufferImpl struct {
	s *screenImpl

	addr unsafe.Pointer
	buf  []byte
	rgba image.RGBA
	size image.Point
	xs   shm.Seg

	mu        sync.Mutex
	nUpload   uint32
	released  bool
	cleanedUp bool
}

func (b *bufferImpl) Size() image.Point       { return b.size }
func (b *bufferImpl) Bounds() image.Rectangle { return image.Rectangle{Max: b.size} }
func (b *bufferImpl) RGBA() *image.RGBA       { return &b.rgba }

func (b *bufferImpl) preUpload() {
	b.mu.Lock()
	if b.released {
		b.mu.Unlock()
		panic("x11driver: Buffer.Upload called after Buffer.Release")
	}
	needsSwizzle := b.nUpload == 0
	b.nUpload++
	b.mu.Unlock()

	if needsSwizzle {
		swizzle.BGRA(b.buf)
	}
}

func (b *bufferImpl) postUpload() {
	b.mu.Lock()
	b.nUpload--
	more := b.nUpload != 0
	released := b.released
	b.mu.Unlock()

	if more {
		return
	}
	if released {
		b.cleanUp()
	} else {
		swizzle.BGRA(b.buf)
	}
}

func (b *bufferImpl) Release() {
	b.mu.Lock()
	cleanUp := !b.released && b.nUpload == 0
	b.released = true
	b.mu.Unlock()

	if cleanUp {
		b.cleanUp()
	}
}

func (b *bufferImpl) cleanUp() {
	b.mu.Lock()
	alreadyCleanedUp := b.cleanedUp
	b.cleanedUp = true
	b.mu.Unlock()

	if alreadyCleanedUp {
		panic("x11driver: Buffer clean-up occurred twice")
	}

	b.s.mu.Lock()
	delete(b.s.buffers, b.xs)
	b.s.mu.Unlock()

	shm.Detach(b.s.xc, b.xs)
	if err := shmClose(b.addr); err != nil {
		log.Printf("x11driver: shmClose: %v", err)
	}
}

func (b *bufferImpl) upload(u screen.Uploader, xd xproto.Drawable, xg xproto.Gcontext, depth uint8,
	dp image.Point, sr image.Rectangle, sender screen.Sender) {

	b.preUpload()

	// TODO: adjust if dp is outside dst bounds, or sr is outside src bounds.
	dr := sr.Sub(sr.Min).Add(dp)

	cookie := shm.PutImage(
		b.s.xc, xd, xg,
		uint16(b.size.X), uint16(b.size.Y), // TotalWidth, TotalHeight,
		uint16(sr.Min.X), uint16(sr.Min.Y), // SrcX, SrcY,
		uint16(dr.Dx()), uint16(dr.Dy()), // SrcWidth, SrcHeight,
		int16(dr.Min.X), int16(dr.Min.Y), // DstX, DstY,
		depth, xproto.ImageFormatZPixmap,
		1, b.xs, 0, // 1 means send a completion event, 0 means a zero offset.
	)

	b.s.mu.Lock()
	b.s.uploads[cookie.Sequence] = completion{
		sender: sender,
		event: screen.UploadedEvent{
			Buffer:   b,
			Uploader: u,
		},
	}
	b.s.mu.Unlock()
}
