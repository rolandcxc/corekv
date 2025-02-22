// Copyright 2021 hardcore-os Project Authors
//
// Licensed under the Apache License, Version 2.0 (the "License")
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package file

import (
	"bufio"
	"bytes"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sync"

	"github.com/hardcore-os/corekv/utils"
	"github.com/pkg/errors"
)

// WalFile _
type WalFile struct {
	lock    *sync.RWMutex
	f       *MmapFile
	fid     uint32
	opts    *Options
	buf     *bytes.Buffer
	size    uint32
	writeAt uint32
}

// Close _
func (wf *WalFile) Close() error {
	fileName := wf.f.Fd.Name()
	if err := wf.f.Close(); err != nil {
		return err
	}
	os.Remove(fileName)
	return nil
}

// Name _
func (wf *WalFile) Name() string {
	return wf.f.Fd.Name()
}

// Size _
func (wf *WalFile) Size() uint32 {
	return wf.size
}

// OpenWalFile _
func OpenWalFile(opt *Options) *WalFile {
	omf, err := OpenMmapFile(opt.FileName, os.O_CREATE|os.O_RDWR, opt.MaxSz)
	wf := &WalFile{f: omf, lock: &sync.RWMutex{}, opts: opt}
	wf.buf = &bytes.Buffer{}
	wf.size = uint32(len(wf.f.Data))
	utils.Err(err)
	return wf
}

func (wf *WalFile) Write(entry *utils.Entry) error {
	// 落预写日志简单的同步写即可
	// 序列化为磁盘结构
	wf.lock.Lock()
	plen := utils.WalCodec(wf.buf, entry)
	utils.CondPanic(plen != copy(wf.f.Data[wf.writeAt:], wf.buf.Bytes()), errors.New("wal.Write"))
	wf.writeAt += uint32(plen)
	wf.lock.Unlock()
	return nil
}

// Iterate 从磁盘中遍历wal，获得数据
func (wf *WalFile) Iterate(readOnly bool, offset uint32, fn utils.LogEntry) (uint32, error) {
	// For now, read directly from file, because it allows
	reader := bufio.NewReader(wf.f.NewReader(int(offset)))
	read := SafeRead{
		K:            make([]byte, 10),
		V:            make([]byte, 10),
		RecordOffset: offset,
		LF:           wf,
	}
	var validEndOffset uint32 = offset
loop:
	for {
		e, err := read.MakeEntry(reader)
		switch {
		case err == io.EOF:
			break loop
		case err == io.ErrUnexpectedEOF || err == utils.ErrTruncate:
			break loop
		case err != nil:
			return 0, err
		case e.IsZero():
			break loop
		}

		var vp utils.ValuePtr // 给kv分离的设计留下扩展,可以不用考虑其作用
		size := uint32(int(e.LogHeaderLen()) + len(e.Key) + len(e.Value) + crc32.Size)
		read.RecordOffset += size
		validEndOffset = read.RecordOffset
		if err := fn(e, &vp); err != nil {
			if err == utils.ErrStop {
				break
			}
			return 0, errors.WithMessage(err, "Iteration function")
		}
	}
	return validEndOffset, nil
}

// Truncate _
// TODO Truncate 函数
func (wf *WalFile) Truncate(end int64) error {
	if fi, err := wf.f.Fd.Stat(); err != nil {
		return fmt.Errorf("while file.stat on file: %s, error: %v\n", wf.Name(), err)
	} else if fi.Size() == end {
		return nil
	}
	wf.size = uint32(end)
	return wf.f.Truncature(end)
}

// 封装kv分离的读操作
type SafeRead struct {
	K []byte
	V []byte

	RecordOffset uint32
	LF           *WalFile
}

// MakeEntry _
func (r *SafeRead) MakeEntry(reader io.Reader) (*utils.Entry, error) {
	tee := utils.NewHashReader(reader)
	var h utils.WalHeader
	hlen, err := h.Decode(tee)
	if err != nil {
		return nil, err
	}
	if h.KeyLen > uint32(1<<16) { // Key length must be below uint16.
		return nil, utils.ErrTruncate
	}
	kl := int(h.KeyLen)
	if cap(r.K) < kl {
		r.K = make([]byte, 2*kl)
	}
	vl := int(h.ValueLen)
	if cap(r.V) < vl {
		r.V = make([]byte, 2*vl)
	}

	e := &utils.Entry{}
	e.Offset = r.RecordOffset
	e.Hlen = hlen
	buf := make([]byte, h.KeyLen+h.ValueLen)
	if _, err := io.ReadFull(tee, buf[:]); err != nil {
		if err == io.EOF {
			err = utils.ErrTruncate
		}
		return nil, err
	}
	e.Key = buf[:h.KeyLen]
	e.Value = buf[h.KeyLen:]
	var crcBuf [crc32.Size]byte
	if _, err := io.ReadFull(reader, crcBuf[:]); err != nil {
		if err == io.EOF {
			err = utils.ErrTruncate
		}
		return nil, err
	}
	crc := utils.BytesToU32(crcBuf[:])
	if crc != tee.Sum32() {
		return nil, utils.ErrTruncate
	}
	e.ExpiresAt = h.ExpiresAt
	return e, nil
}
