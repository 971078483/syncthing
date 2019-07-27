// Copyright (C) 2018 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package db

import (
	"encoding/binary"
	"sort"

	"github.com/syncthing/syncthing/lib/sync"
	"github.com/syndtr/goleveldb/leveldb/util"
)

// A smallIndex is an in memory bidirectional []byte to uint32 map. It gives
// fast lookups in both directions and persists to the database. Don't use for
// storing more items than fit comfortably in RAM.
type smallIndex struct {
	prefix []byte
	id2val map[uint32]string
	val2id map[string]uint32
	nextID uint32
	mut    sync.Mutex
}

func newSmallIndex(r reader, prefix []byte) *smallIndex {
	idx := &smallIndex{
		prefix: prefix,
		id2val: make(map[uint32]string),
		val2id: make(map[string]uint32),
		mut:    sync.NewMutex(),
	}
	idx.load(r)
	return idx
}

// load iterates over the prefix space in the database and populates the in
// memory maps.
func (i *smallIndex) load(r reader) {
	it := r.NewIterator(util.BytesPrefix(i.prefix), nil)
	defer it.Release()
	for it.Next() {
		val := string(it.Value())
		id := binary.BigEndian.Uint32(it.Key()[len(i.prefix):])
		if val != "" {
			// Empty value means the entry has been deleted.
			i.id2val[id] = val
			i.val2id[val] = id
		}
		if id >= i.nextID {
			i.nextID = id + 1
		}
	}
}

// ID returns the index number for the given byte slice, allocating a new one
// and persisting this to the database if necessary.
func (i *smallIndex) ID(w writer, val []byte) uint32 {
	i.mut.Lock()
	// intentionally avoiding defer here as we want this call to be as fast as
	// possible in the general case (folder ID already exists). The map lookup
	// with the conversion of []byte to string is compiler optimized to not
	// copy the []byte, which is why we don't assign it to a temp variable
	// here.
	if id, ok := i.val2id[string(val)]; ok {
		i.mut.Unlock()
		return id
	}

	id := i.nextID
	i.nextID++

	valStr := string(val)
	i.val2id[valStr] = id
	i.id2val[id] = valStr

	key := make([]byte, len(i.prefix)+8) // prefix plus uint32 id
	copy(key, i.prefix)
	binary.BigEndian.PutUint32(key[len(i.prefix):], id)
	if err := w.Put(key, val, nil); err != nil {
		panic(err)
	}

	i.mut.Unlock()
	return id
}

// IDRO returns the index number for the given byte slice, *without*
// allocating a new one if it wasn't present.
func (i *smallIndex) IDRO(val []byte) (uint32, bool) {
	i.mut.Lock()
	id, ok := i.val2id[string(val)]
	i.mut.Unlock()
	if !ok {
		return 0, false
	}
	return id, true
}

// Val returns the value for the given index number, or (nil, false) if there
// is no such index number.
func (i *smallIndex) Val(id uint32) ([]byte, bool) {
	i.mut.Lock()
	val, ok := i.id2val[id]
	i.mut.Unlock()
	if !ok {
		return nil, false
	}

	return []byte(val), true
}

func (i *smallIndex) Delete(w writer, val []byte) {
	i.mut.Lock()
	defer i.mut.Unlock()

	// Check the reverse mapping to get the ID for the value.
	if id, ok := i.val2id[string(val)]; ok {
		// Generate the corresponding database key.
		key := make([]byte, len(i.prefix)+8) // prefix plus uint32 id
		copy(key, i.prefix)
		binary.BigEndian.PutUint32(key[len(i.prefix):], id)

		// Put an empty value into the database. This indicates that the
		// entry does not exist any more and prevents the ID from being
		// reused in the future.
		if err := w.Put(key, []byte{}, nil); err != nil {
			panic(err)
		}

		// Delete reverse mapping.
		delete(i.id2val, id)
	}

	// Delete forward mapping.
	delete(i.val2id, string(val))
}

// Values returns the set of values in the index
func (i *smallIndex) Values() []string {
	// In principle this method should return [][]byte because all the other
	// methods deal in []byte keys. However, in practice, where it's used
	// wants a []string and it's easier to just create that here rather than
	// having to convert both here and there...

	i.mut.Lock()
	vals := make([]string, 0, len(i.val2id))
	for val := range i.val2id {
		vals = append(vals, val)
	}
	i.mut.Unlock()

	sort.Strings(vals)
	return vals
}
