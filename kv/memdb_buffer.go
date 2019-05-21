// Copyright 2015 PingCAP, Inc.
//
// Copyright 2015 Wenbin Xiao
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package kv

import (
	"github.com/sirupsen/logrus"
	"reflect"
	"sync/atomic"

	"github.com/juju/errors"
	"github.com/pingcap/goleveldb/leveldb"
	"github.com/pingcap/goleveldb/leveldb/comparer"
	"github.com/pingcap/goleveldb/leveldb/iterator"
	"github.com/pingcap/goleveldb/leveldb/memdb"
	"github.com/pingcap/goleveldb/leveldb/util"
	"github.com/pingcap/tidb/terror"
)

// memDBBuffer implements the MemBuffer interface.
type memDbBuffer struct {
	db              *memdb.DB
	entrySizeLimit  int
	bufferLenLimit  uint64
	bufferSizeLimit int
}

type memDbIter struct {
	iter    iterator.Iterator
	reverse bool
}

// NewMemDbBuffer creates a new memDbBuffer.
func NewMemDbBuffer(cap int) MemBuffer {
	logrus.Printf("new MemDbBuffer with leveldb")
	return &memDbBuffer{
		db:              memdb.New(comparer.DefaultComparer, cap),
		entrySizeLimit:  TxnEntrySizeLimit,
		bufferLenLimit:  atomic.LoadUint64(&TxnEntryCountLimit),
		bufferSizeLimit: TxnTotalSizeLimit,
	}
}

// Seek creates an Iterator.
func (m *memDbBuffer) Seek(k Key) (Iterator, error) {
	logrus.Printf("seek in memDbBuffer by leveldb NewIterator")
	var i Iterator
	if k == nil {
		i = &memDbIter{iter: m.db.NewIterator(&util.Range{}), reverse: false}
	} else {
		i = &memDbIter{iter: m.db.NewIterator(&util.Range{Start: []byte(k)}), reverse: false}
	}
	err := i.Next()
	if err != nil {
		return nil, errors.Trace(err)
	}
	return i, nil
}

func (m *memDbBuffer) SetCap(cap int) {

}

func (m *memDbBuffer) SeekReverse(k Key) (Iterator, error) {
	var i *memDbIter
	if k == nil {
		i = &memDbIter{iter: m.db.NewIterator(&util.Range{}), reverse: true}
	} else {
		i = &memDbIter{iter: m.db.NewIterator(&util.Range{Limit: []byte(k)}), reverse: true}
	}
	i.iter.Last()
	return i, nil
}

// Get returns the value associated with key.
func (m *memDbBuffer) Get(k Key) ([]byte, error) {
	v, err := m.db.Get(k)
	if terror.ErrorEqual(err, leveldb.ErrNotFound) {
		return nil, ErrNotExist
	}
	return v, nil
}

// Set associates key with value.
// code_analysis for_trigger
func (m *memDbBuffer) Set(k Key, v []byte) error {
	logrus.Printf("buffer kv in memDbBuffer: key[%s],value[%s]", k, v)
	if len(v) == 0 {
		return errors.Trace(ErrCannotSetNilValue)
	}
	if len(k)+len(v) > m.entrySizeLimit {
		return ErrEntryTooLarge.Gen("entry too large, size: %d", len(k)+len(v))
	}

	err := m.db.Put(k, v)
	if m.Size() > m.bufferSizeLimit {
		return ErrTxnTooLarge.Gen("transaction too large, size:%d", m.Size())
	}
	if m.Len() > int(m.bufferLenLimit) {
		return ErrTxnTooLarge.Gen("transaction too large, len:%d", m.Len())
	}
	return errors.Trace(err)
}

// Delete removes the entry from buffer with provided key.
func (m *memDbBuffer) Delete(k Key) error {
	err := m.db.Put(k, nil)
	return errors.Trace(err)
}

// Size returns sum of keys and values length.
func (m *memDbBuffer) Size() int {
	return m.db.Size()
}

// Len returns the number of entries in the DB.
func (m *memDbBuffer) Len() int {
	return m.db.Len()
}

// Reset cleanup the MemBuffer.
func (m *memDbBuffer) Reset() {
	m.db.Reset()
}

// Next implements the Iterator Next.
func (i *memDbIter) Next() error {
	if i.reverse {
		i.iter.Prev()
	} else {
		i.iter.Next()
	}
	return nil
}

// Valid implements the Iterator Valid.
func (i *memDbIter) Valid() bool {
	return i.iter.Valid()
}

// Key implements the Iterator Key.
func (i *memDbIter) Key() Key {
	return i.iter.Key()
}

// Value implements the Iterator Value.
func (i *memDbIter) Value() []byte {
	return i.iter.Value()
}

// Close Implements the Iterator Close.
func (i *memDbIter) Close() {
	i.iter.Release()
}

// WalkMemBuffer iterates all buffered kv pairs in memBuf
func WalkMemBuffer(memBuf MemBuffer, f func(k Key, v []byte) error) error {
	logrus.Printf("WalkMemBuffer %s", reflect.TypeOf(memBuf))
	iter, err := memBuf.Seek(nil)
	if err != nil {
		return errors.Trace(err)
	}

	defer iter.Close()
	for iter.Valid() {
		if err = f(iter.Key(), iter.Value()); err != nil {
			return errors.Trace(err)
		}
		err = iter.Next()
		if err != nil {
			return errors.Trace(err)
		}
	}

	return nil
}
