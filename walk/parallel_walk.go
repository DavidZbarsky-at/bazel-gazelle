// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package fastwalk provides a faster version of [filepath.Walk] for file system
// scanning tools.
package walk

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

type WalkFn func(path string, ents []fs.DirEntry) error

// Walk is a faster implementation of [filepath.Walk].
//
// [filepath.Walk]'s design necessarily calls [os.Lstat] on each file,
// even if the caller needs less info.
// Many tools need only the type of each file.
// On some platforms, this information is provided directly by the readdir
// system call, avoiding the need to stat each file individually.
// fastwalk_unix.go contains a fork of the syscall routines.
//
// See golang.org/issue/16399.
//
// Walk walks the file tree rooted at root, calling walkFn for
// each file or directory in the tree, including root.
//
// If Walk returns [filepath.SkipDir], the directory is skipped.
//
// Unlike [filepath.Walk]:
//   - file stat calls must be done by the user.
//     The only provided metadata is the file type, which does not include
//     any permission bits.
//   - multiple goroutines stat the filesystem concurrently. The provided
//     walkFn must be safe for concurrent use.
func ParallelWalk(root string, walkFn WalkFn) error {
	// TODO(bradfitz): make numWorkers configurable? We used a
	// minimum of 4 to give the kernel more info about multiple
	// things we want, in hopes its I/O scheduling can take
	// advantage of that. Hopefully most are in cache. Maybe 4 is
	// even too low of a minimum. Profile more.
	numWorkers := 4
	if n := runtime.NumCPU(); n > numWorkers {
		numWorkers = n
	}

	// Make sure to wait for all workers to finish, otherwise
	// walkFn could still be called after returning. This Wait call
	// runs after close(e.donec) below.
	var wg sync.WaitGroup
	defer wg.Wait()

	w := &walker{
		fn:       walkFn,
		enqueuec: make(chan walkItem, numWorkers), // buffered for performance
		workc:    make(chan walkItem, numWorkers), // buffered for performance
		donec:    make(chan struct{}),

		// buffered for correctness & not leaking goroutines:
		resc: make(chan error, numWorkers),
	}
	defer close(w.donec)

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go w.doWork(&wg)
	}
	todo := []walkItem{{dir: root}}
	out := 0
	for {
		workc := w.workc
		var workItem walkItem
		if len(todo) == 0 {
			workc = nil
		} else {
			workItem = todo[len(todo)-1]
		}
		select {
		case workc <- workItem:
			todo = todo[:len(todo)-1]
			out++
		case it := <-w.enqueuec:
			todo = append(todo, it)
		case err := <-w.resc:
			out--
			if err != nil {
				return err
			}
			if out == 0 && len(todo) == 0 {
				// It's safe to quit here, as long as the buffered
				// enqueue channel isn't also readable, which might
				// happen if the worker sends both another unit of
				// work and its result before the other select was
				// scheduled and both w.resc and w.enqueuec were
				// readable.
				select {
				case it := <-w.enqueuec:
					todo = append(todo, it)
				default:
					return nil
				}
			}
		}
	}
}

// doWork reads directories as instructed (via workc) and runs the
// user's callback function.
func (w *walker) doWork(wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case <-w.donec:
			return
		case it := <-w.workc:
			select {
			case <-w.donec:
				return
			case w.resc <- w.walk(it.dir):
			}
		}
	}
}

type walker struct {
	fn WalkFn

	donec    chan struct{} // closed on fastWalk's return
	workc    chan walkItem // to workers
	enqueuec chan walkItem // from workers
	resc     chan error    // from workers
}

type walkItem struct {
	dir string
}

func (w *walker) enqueue(it walkItem) {
	select {
	case w.enqueuec <- it:
	case <-w.donec:
	}
}

func (w *walker) walk(root string) error {
	ents, err := os.ReadDir(root)
	if err != nil {
		return err
	}

	err = w.fn(root, ents)
	if err == filepath.SkipDir {
		return nil
	}
	if err != nil {
		return err
	}

	for _, ent := range ents {
		if ent.Type() == os.ModeDir {
			joined := filepath.Join(root, ent.Name())
			w.enqueue(walkItem{dir: joined})
		}
	}

	return nil
}
