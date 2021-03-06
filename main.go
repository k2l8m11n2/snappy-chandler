package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"
	"log"

	"github.com/dgraph-io/badger/v2"
	"github.com/restic/chunker"
	"lukechampine.com/blake3"
)

func panil(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	version := []byte{0}

	db, err := badger.Open(badger.DefaultOptions(".snappy-chandler"))
	panil(err)
	defer db.Close()
	err = db.Update(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte("version"))
		if err != nil {
			log.Println("version key not found, assuming fresh db and writing initial data")
			txn.Set([]byte("version"), []byte{0})
			poly, err := chunker.RandomPolynomial()
			if err != nil {
				return err
			}
			b := make([]byte, 8)
			binary.LittleEndian.PutUint64(b, uint64(poly))
			txn.Set([]byte("polynomial"), b)
			return nil
		}
		return item.Value(func(val []byte) error {
			if err != nil {
				return fmt.Errorf("version check failed: %w", err)
			}
			if val[0] != version[0] { // yep, I hope we never get past 256 versions
				return fmt.Errorf("version check failed: %v (db) != %v (tool)", val, version)
			}
			return nil
		})
	})
	panil(err)

	data := []byte("we out here!")
	superhash, err := insert(db, bytes.NewReader(data))
	panil(err)
	reader, err := retrieve(db, superhash)
	panil(err)
	redata, err := ioutil.ReadAll(reader)
	panil(err)
	fmt.Println(string(data), string(redata))
}

func insert(db *badger.DB, r io.Reader) ([32]byte, error) {
	var poly chunker.Pol
	err := db.View(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte("polynomial"))
		if err != nil {
			return err
		}
		item.Value(func(val []byte) error {
			poly = chunker.Pol(binary.LittleEndian.Uint64(val))
			return nil
		})
		return nil
	})
	if err != nil {
		return [32]byte{}, err
	}
	ch := chunker.New(r, poly)
	buf := make([]byte, 16*1024*1024)
	var hashes []byte
	var superhash [32]byte
	err = db.Update(func(txn *badger.Txn) error {
		for {
			chunk, err := ch.Next(buf)
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}

			data := make([]byte, len(chunk.Data))
			// BadgerDB can't have data changed under it before it commits
			// chunker reuses the underlying buffer so we have to copy it off
			// TODO: commit after every chunk for memory savings?
			copy(data, chunk.Data)
			hash := blake3.Sum256(chunk.Data)
			hashes = append(hashes, hash[:]...)

			_, err = txn.Get(append([]byte("chunk/"), hash[:]...))
			if err == nil { // chunk exists
				continue
			}
			if err != badger.ErrKeyNotFound {
				return err
			}

			// chunk doesn't exist, inserting
			err = txn.Set(append([]byte("chunk/"), hash[:]...), data)
			if err != nil {
				return err
			}
		}
		superhash = blake3.Sum256(hashes)
		return txn.Set(append([]byte("blob/"), superhash[:]...), hashes)
	})
	if err != nil {
		return [32]byte{}, err
	}

	return superhash, nil
}

type retRdr struct {
	db     *badger.DB
	hashes []byte
	offset int
}

func (r *retRdr) Read(p []byte) (int, error) {
	if len(r.hashes) == 0 {
		return 0, io.EOF
	}
	hash := r.hashes[:32]
	var n, l int
	err := r.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(append([]byte("chunk/"), hash[:]...))
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			l = len(val) - r.offset
			n = copy(p, val[r.offset:])
			return nil
		})
	})
	if n != l {
		r.offset = n
	} else {
		r.hashes = r.hashes[32:]
	}

	return n, err
}

func retrieve(db *badger.DB, superhash [32]byte) (io.Reader, error) {
	r := &retRdr{db: db}
	err := db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(append([]byte("blob/"), superhash[:]...))
		if err != nil {
			return err
		}
		hashes, err := item.ValueCopy(nil)
		r.hashes = hashes
		return err
	})
	return r, err
}
