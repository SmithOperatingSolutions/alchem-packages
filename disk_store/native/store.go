// Package native is the log behind the DiskStore module.
//
// A store keeps text under text keys in one file, so that what a program
// records outlasts it. The file is a log: every change is appended as one
// record and written through to the disk before the call that made it
// returns, so a change that was acknowledged survives a crash, and a record
// cut short by one is dropped when the file is next opened. Keys and values
// are also held in memory, keys in order, so reads never touch the disk.
//
// The file begins with magic. A record is the length of its body and the
// body's CRC-32, four bytes each, then the body: one or more changes, each a
// byte saying put or delete, the key, and for a put the value, with lengths
// as varints. The changes of one record take effect together.
package native

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

const magic = "ALCHEMSTORE1\n"

const (
	putOp    = 1
	deleteOp = 2
	// slack is how far the file may outgrow what it holds before it is
	// written afresh.
	slack = 1 << 20
)

// Store is an open store. Every method may be called from several
// goroutines at once.
type Store struct {
	path string
	// mu guards what is in memory and closed; reads take it shared.
	mu     sync.RWMutex
	values map[string]string
	keys   []string
	closed bool
	// updates is held shared by a plain write and alone by Update, so that
	// nothing changes between what an update read and what it writes.
	updates sync.RWMutex
	// gate is held shared while a write is handed to the writer and alone
	// to close the queue, so nothing is sent on a closed queue. The writer
	// never takes it, so a full queue always drains.
	gate  sync.RWMutex
	queue chan *write
	ended chan struct{}
	// file, size and live belong to the writer: the log, its length, and
	// the length it would have holding only what is current.
	file *os.File
	size int64
	live int64
}

// Entry is a key with its value.
type Entry struct {
	Key, Value string
}

type change struct {
	delete     bool
	key, value string
}

// write is one record waiting to be written, or a request to rewrite the
// file when it has no changes.
type write struct {
	changes []change
	compact bool
	done    chan error
}

// opened holds the stores that are open, by path: opening a file a second
// time gives the same store, since two logs appending to one file would
// spoil it.
var opened = struct {
	sync.Mutex
	byPath map[string]*Store
}{byPath: map[string]*Store{}}

var errClosed = errors.New("closed")

// problem words a file error the way the File module does.
func problem(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, os.ErrNotExist):
		return errors.New("enoent")
	case errors.Is(err, os.ErrPermission):
		return errors.New("eacces")
	case errors.Is(err, os.ErrExist):
		return errors.New("eexist")
	}
	return err
}

func record(changes []change) []byte {
	body := []byte{}
	for _, c := range changes {
		if c.delete {
			body = append(body, deleteOp)
			body = binary.AppendUvarint(body, uint64(len(c.key)))
			body = append(body, c.key...)
			continue
		}
		body = append(body, putOp)
		body = binary.AppendUvarint(body, uint64(len(c.key)))
		body = append(body, c.key...)
		body = binary.AppendUvarint(body, uint64(len(c.value)))
		body = append(body, c.value...)
	}
	out := binary.LittleEndian.AppendUint32(make([]byte, 0, len(body)+8), uint32(len(body)))
	out = binary.LittleEndian.AppendUint32(out, crc32.ChecksumIEEE(body))
	return append(out, body...)
}

// changesOf reads the body of a record, or reports that it is not one.
func changesOf(body []byte) ([]change, bool) {
	changes := []change{}
	text := func() (string, bool) {
		size, used := binary.Uvarint(body)
		if used <= 0 || size > uint64(len(body)-used) {
			return "", false
		}
		value := string(body[used : used+int(size)])
		body = body[used+int(size):]
		return value, true
	}
	for len(body) > 0 {
		op := body[0]
		body = body[1:]
		key, ok := text()
		if !ok || op != putOp && op != deleteOp {
			return nil, false
		}
		c := change{delete: op == deleteOp, key: key}
		if !c.delete {
			if c.value, ok = text(); !ok {
				return nil, false
			}
		}
		changes = append(changes, c)
	}
	return changes, true
}

// cost is what one entry adds to a file holding only current entries.
func cost(key, value string) int64 { return int64(len(key) + len(value) + 20) }

// apply makes changes in memory. The caller holds mu.
func (s *Store) apply(changes []change) {
	for _, c := range changes {
		old, had := s.values[c.key]
		at, _ := slices.BinarySearch(s.keys, c.key)
		switch {
		case c.delete && had:
			delete(s.values, c.key)
			s.keys = slices.Delete(s.keys, at, at+1)
			s.live -= cost(c.key, old)
		case c.delete:
		default:
			if had {
				s.live -= cost(c.key, old)
			} else {
				s.keys = slices.Insert(s.keys, at, c.key)
			}
			s.values[c.key] = c.value
			s.live += cost(c.key, c.value)
		}
	}
}

// Open opens the store in a file, creating the file when there is none.
func Open(path string) (*Store, error) {
	full, err := filepath.Abs(path)
	if err != nil {
		return nil, problem(err)
	}
	opened.Lock()
	defer opened.Unlock()
	if s, open := opened.byPath[full]; open {
		return s, nil
	}
	data, err := os.ReadFile(full)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, problem(err)
	}
	s := &Store{path: full, values: map[string]string{}, queue: make(chan *write, 256), ended: make(chan struct{}), live: int64(len(magic))}
	kept := len(magic)
	switch {
	case len(data) == 0 || len(data) < len(magic) && strings.HasPrefix(magic, string(data)):
		// A new file, or one whose first write was cut short.
		data, kept = nil, 0
	case !strings.HasPrefix(string(data), magic):
		return nil, errors.New(path + " is not a store")
	}
	for rest := data[kept:]; len(rest) >= 8; {
		size := binary.LittleEndian.Uint32(rest)
		if uint64(size) > uint64(len(rest)-8) || crc32.ChecksumIEEE(rest[8:8+size]) != binary.LittleEndian.Uint32(rest[4:]) {
			break
		}
		changes, ok := changesOf(rest[8 : 8+size])
		if !ok {
			break
		}
		s.apply(changes)
		kept += 8 + int(size)
		rest = rest[8+size:]
	}
	s.file, err = os.OpenFile(full, os.O_RDWR|os.O_CREATE, 0600)
	if err == nil && kept == 0 {
		_, err = s.file.WriteAt([]byte(magic), 0)
		kept = len(magic)
	}
	if err == nil {
		// What follows the last whole record was cut short by a crash.
		err = s.file.Truncate(int64(kept))
	}
	if err == nil {
		_, err = s.file.Seek(int64(kept), io.SeekStart)
	}
	if err == nil {
		err = s.file.Sync()
	}
	if err != nil {
		if s.file != nil {
			s.file.Close()
		}
		return nil, problem(err)
	}
	s.size = int64(kept)
	opened.byPath[full] = s
	go s.run()
	return s, nil
}

// run is the store's one writer. It takes whatever writes are waiting,
// appends their records together and makes them durable with one sync, so
// that many processes writing at once share the cost of the disk.
func (s *Store) run() {
	defer close(s.ended)
	for first := range s.queue {
		batch := []*write{first}
	gather:
		for len(batch) < 256 {
			select {
			case next, open := <-s.queue:
				if !open {
					break gather
				}
				batch = append(batch, next)
			default:
				break gather
			}
		}
		records, compact := []byte{}, false
		for _, w := range batch {
			if len(w.changes) > 0 {
				records = append(records, record(w.changes)...)
			}
			compact = compact || w.compact
		}
		var err error
		if len(records) > 0 {
			if _, err = s.file.Write(records); err == nil {
				err = s.file.Sync()
			}
			if err != nil {
				// Nothing of a failed write may be read back later.
				s.file.Truncate(s.size)
				s.file.Seek(s.size, io.SeekStart)
			} else {
				s.size += int64(len(records))
				s.mu.Lock()
				for _, w := range batch {
					s.apply(w.changes)
				}
				s.mu.Unlock()
			}
		}
		if err == nil && (compact || s.size > 2*s.live+slack) {
			err = s.rewrite()
			if !compact {
				// Tidying up is not what these writes asked for.
				err = nil
			}
		}
		for _, w := range batch {
			w.done <- err
		}
	}
	s.file.Close()
}

// rewrite replaces the log by one holding only what is current: written
// beside it, made durable, and renamed over it, so that a crash at any
// point leaves one whole file or the other.
func (s *Store) rewrite() error {
	fresh := []byte(magic)
	for _, key := range s.keys {
		fresh = append(fresh, record([]change{{key: key, value: s.values[key]}})...)
	}
	beside := s.path + ".compact"
	file, err := os.OpenFile(beside, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err = file.Write(fresh); err == nil {
		err = file.Sync()
	}
	if err != nil {
		file.Close()
		os.Remove(beside)
		return err
	}
	// The log is closed while it is replaced, which some systems require.
	file.Close()
	s.file.Close()
	renamed := os.Rename(beside, s.path)
	if renamed != nil {
		os.Remove(beside)
	}
	s.file, err = os.OpenFile(s.path, os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if renamed == nil {
		s.size = int64(len(fresh))
		if folder, err := os.Open(filepath.Dir(s.path)); err == nil {
			folder.Sync()
			folder.Close()
		}
	}
	if _, err = s.file.Seek(s.size, io.SeekStart); err != nil {
		return err
	}
	return renamed
}

// submit hands a write to the writer and waits until it is on the disk.
func (s *Store) submit(w *write) error {
	w.done = make(chan error, 1)
	s.gate.RLock()
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		s.gate.RUnlock()
		return errClosed
	}
	s.queue <- w
	s.gate.RUnlock()
	return problem(<-w.done)
}

// Put stores a value under a key.
func (s *Store) Put(key, value string) error {
	s.updates.RLock()
	defer s.updates.RUnlock()
	return s.submit(&write{changes: []change{{key: key, value: value}}})
}

// Delete removes a key.
func (s *Store) Delete(key string) error {
	s.updates.RLock()
	defer s.updates.RUnlock()
	return s.submit(&write{changes: []change{{delete: true, key: key}}})
}

// Commit makes several changes at once: all of them are kept, or after a
// crash none. keys and values are the puts, side by side.
func (s *Store) Commit(keys, values, deletes []string) error {
	changes := []change{}
	for _, key := range deletes {
		changes = append(changes, change{delete: true, key: key})
	}
	for i, key := range keys {
		changes = append(changes, change{key: key, value: values[i]})
	}
	if len(changes) == 0 {
		return nil
	}
	s.updates.RLock()
	defer s.updates.RUnlock()
	return s.submit(&write{changes: changes})
}

// Choice is a value, or with Keep false the lack of one: what a key holds
// before an update, and what the update leaves there.
type Choice struct {
	Value string
	Keep  bool
}

// Update changes one key by a function of what it holds, with no other
// write to the store in between. The function is given the value, if there
// is one, and answers the same way: no value deletes the key. Update gives
// what the function gave.
func (s *Store) Update(key string, f func(held Choice) Choice) (Choice, error) {
	s.updates.Lock()
	defer s.updates.Unlock()
	s.mu.RLock()
	old, had := s.values[key]
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return Choice{}, errClosed
	}
	next := f(Choice{Value: old, Keep: had})
	c := change{delete: true, key: key}
	if next.Keep {
		c = change{key: key, value: next.Value}
	} else if !had {
		return Choice{}, nil
	}
	if err := s.submit(&write{changes: []change{c}}); err != nil {
		return Choice{}, errors.New("the change could not be written")
	}
	return next, nil
}

// Compact rewrites the file so that it holds only what is current.
func (s *Store) Compact() error {
	s.updates.RLock()
	defer s.updates.RUnlock()
	return s.submit(&write{compact: true})
}

// Close ends the writer once what was waiting is written. A closed store
// takes no writes and gives no reads; opening its path again reads the file
// afresh. It reports whether the store was open.
func (s *Store) Close() bool {
	opened.Lock()
	defer opened.Unlock()
	s.gate.Lock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.gate.Unlock()
		return false
	}
	s.closed = true
	s.mu.Unlock()
	close(s.queue)
	s.gate.Unlock()
	<-s.ended
	delete(opened.byPath, s.path)
	return true
}

// reading takes the lock for a read. Reading a closed store is a mistake in
// the program, which fails the calling process.
func (s *Store) reading(function string) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		panic(errors.New("DiskStore." + function + ": the store is closed"))
	}
}

// Get gives the value under a key.
func (s *Store) Get(key string) (string, bool) {
	s.reading("get")
	defer s.mu.RUnlock()
	value, found := s.values[key]
	return value, found
}

// HasKey reports whether a key is present.
func (s *Store) HasKey(key string) bool {
	s.reading("has_key?")
	defer s.mu.RUnlock()
	_, found := s.values[key]
	return found
}

// Keys gives every key, in order.
func (s *Store) Keys() []string {
	s.reading("keys")
	defer s.mu.RUnlock()
	return slices.Clone(s.keys)
}

// Size is the number of keys.
func (s *Store) Size() int64 {
	s.reading("size")
	defer s.mu.RUnlock()
	return int64(len(s.keys))
}

// between gives the entries whose keys are at or after from and, when
// bounded, before to, in key order. The caller holds mu.
func (s *Store) between(from, to string, bounded bool) []Entry {
	start, _ := slices.BinarySearch(s.keys, from)
	end := len(s.keys)
	if bounded {
		end, _ = slices.BinarySearch(s.keys, to)
	}
	out := []Entry{}
	for i := start; i < end; i++ {
		out = append(out, Entry{s.keys[i], s.values[s.keys[i]]})
	}
	return out
}

// Range gives the entries from one key up to, and not including, another.
func (s *Store) Range(from, to string) []Entry {
	s.reading("range")
	defer s.mu.RUnlock()
	return s.between(from, to, true)
}

// Prefix gives the entries whose keys begin with a prefix: those up to the
// first key that could not, which is the prefix with its last byte that is
// not 0xff raised by one.
func (s *Store) Prefix(prefix string) []Entry {
	s.reading("prefix")
	defer s.mu.RUnlock()
	after := []byte(prefix)
	for len(after) > 0 && after[len(after)-1] == 0xff {
		after = after[:len(after)-1]
	}
	if len(after) == 0 {
		return s.between(prefix, "", false)
	}
	after[len(after)-1]++
	return s.between(prefix, string(after), true)
}
