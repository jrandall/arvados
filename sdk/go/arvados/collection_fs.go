// Copyright (C) The Arvados Authors. All rights reserved.
//
// SPDX-License-Identifier: Apache-2.0

package arvados

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ErrReadOnlyFile      = errors.New("read-only file")
	ErrNegativeOffset    = errors.New("cannot seek to negative offset")
	ErrFileExists        = errors.New("file exists")
	ErrInvalidOperation  = errors.New("invalid operation")
	ErrDirectoryNotEmpty = errors.New("directory not empty")
	ErrWriteOnlyMode     = errors.New("file is O_WRONLY")
	ErrSyncNotSupported  = errors.New("O_SYNC flag is not supported")
	ErrPermission        = os.ErrPermission

	maxBlockSize = 1 << 26
)

type File interface {
	io.Reader
	io.Writer
	io.Closer
	io.Seeker
	Size() int64
	Readdir(int) ([]os.FileInfo, error)
	Stat() (os.FileInfo, error)
	Truncate(int64) error
}

type keepClient interface {
	ReadAt(locator string, p []byte, off int) (int, error)
	PutB(p []byte) (string, int, error)
}

type fileinfo struct {
	name    string
	mode    os.FileMode
	size    int64
	modTime time.Time
}

// Name implements os.FileInfo.
func (fi fileinfo) Name() string {
	return fi.name
}

// ModTime implements os.FileInfo.
func (fi fileinfo) ModTime() time.Time {
	return fi.modTime
}

// Mode implements os.FileInfo.
func (fi fileinfo) Mode() os.FileMode {
	return fi.mode
}

// IsDir implements os.FileInfo.
func (fi fileinfo) IsDir() bool {
	return fi.mode&os.ModeDir != 0
}

// Size implements os.FileInfo.
func (fi fileinfo) Size() int64 {
	return fi.size
}

// Sys implements os.FileInfo.
func (fi fileinfo) Sys() interface{} {
	return nil
}

// A CollectionFileSystem is an http.Filesystem plus Stat() and
// support for opening writable files.
type CollectionFileSystem interface {
	http.FileSystem
	Stat(name string) (os.FileInfo, error)
	Create(name string) (File, error)
	OpenFile(name string, flag int, perm os.FileMode) (File, error)
	Mkdir(name string, perm os.FileMode) error
	Remove(name string) error
	MarshalManifest(string) (string, error)
}

type fileSystem struct {
	dirnode
}

func (fs *fileSystem) OpenFile(name string, flag int, perm os.FileMode) (File, error) {
	return fs.dirnode.OpenFile(path.Clean(name), flag, perm)
}

func (fs *fileSystem) Open(name string) (http.File, error) {
	return fs.dirnode.OpenFile(path.Clean(name), os.O_RDONLY, 0)
}

func (fs *fileSystem) Create(name string) (File, error) {
	return fs.dirnode.OpenFile(path.Clean(name), os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0)
}

func (fs *fileSystem) Stat(name string) (os.FileInfo, error) {
	f, err := fs.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Stat()
}

type inode interface {
	Parent() inode
	Read([]byte, filenodePtr) (int, filenodePtr, error)
	Write([]byte, filenodePtr) (int, filenodePtr, error)
	Truncate(int64) error
	Readdir() []os.FileInfo
	Size() int64
	Stat() os.FileInfo
	sync.Locker
	RLock()
	RUnlock()
}

// filenode implements inode.
type filenode struct {
	fileinfo fileinfo
	parent   *dirnode
	extents  []extent
	repacked int64 // number of times anything in []extents has changed len
	memsize  int64 // bytes in memExtents
	sync.RWMutex
}

// filenodePtr is an offset into a file that is (usually) efficient to
// seek to. Specifically, if filenode.repacked==filenodePtr.repacked
// then filenode.extents[filenodePtr.extentIdx][filenodePtr.extentOff]
// corresponds to file offset filenodePtr.off. Otherwise, it is
// necessary to reexamine len(filenode.extents[0]) etc. to find the
// correct extent and offset.
type filenodePtr struct {
	off       int64
	extentIdx int
	extentOff int
	repacked  int64
}

// seek returns a ptr that is consistent with both startPtr.off and
// the current state of fn. The caller must already hold fn.RLock() or
// fn.Lock().
//
// If startPtr points beyond the end of the file, ptr will point to
// exactly the end of the file.
//
// After seeking:
//
//     ptr.extentIdx == len(filenode.extents) // i.e., at EOF
//     ||
//     filenode.extents[ptr.extentIdx].Len() >= ptr.extentOff
func (fn *filenode) seek(startPtr filenodePtr) (ptr filenodePtr) {
	ptr = startPtr
	if ptr.off < 0 {
		// meaningless anyway
		return
	} else if ptr.off >= fn.fileinfo.size {
		ptr.off = fn.fileinfo.size
		ptr.extentIdx = len(fn.extents)
		ptr.extentOff = 0
		ptr.repacked = fn.repacked
		return
	} else if ptr.repacked == fn.repacked {
		// extentIdx and extentOff accurately reflect ptr.off,
		// but might have fallen off the end of an extent
		if ptr.extentOff >= fn.extents[ptr.extentIdx].Len() {
			ptr.extentIdx++
			ptr.extentOff = 0
		}
		return
	}
	defer func() {
		ptr.repacked = fn.repacked
	}()
	if ptr.off >= fn.fileinfo.size {
		ptr.extentIdx, ptr.extentOff = len(fn.extents), 0
		return
	}
	// Recompute extentIdx and extentOff.  We have already
	// established fn.fileinfo.size > ptr.off >= 0, so we don't
	// have to deal with edge cases here.
	var off int64
	for ptr.extentIdx, ptr.extentOff = 0, 0; off < ptr.off; ptr.extentIdx++ {
		// This would panic (index out of range) if
		// fn.fileinfo.size were larger than
		// sum(fn.extents[i].Len()) -- but that can't happen
		// because we have ensured fn.fileinfo.size is always
		// accurate.
		extLen := int64(fn.extents[ptr.extentIdx].Len())
		if off+extLen > ptr.off {
			ptr.extentOff = int(ptr.off - off)
			break
		}
		off += extLen
	}
	return
}

func (fn *filenode) appendExtent(e extent) {
	fn.Lock()
	defer fn.Unlock()
	fn.extents = append(fn.extents, e)
	fn.fileinfo.size += int64(e.Len())
}

func (fn *filenode) Parent() inode {
	return fn.parent
}

func (fn *filenode) Readdir() []os.FileInfo {
	return nil
}

func (fn *filenode) Read(p []byte, startPtr filenodePtr) (n int, ptr filenodePtr, err error) {
	ptr = fn.seek(startPtr)
	if ptr.off < 0 {
		err = ErrNegativeOffset
		return
	}
	if ptr.extentIdx >= len(fn.extents) {
		err = io.EOF
		return
	}
	n, err = fn.extents[ptr.extentIdx].ReadAt(p, int64(ptr.extentOff))
	if n > 0 {
		ptr.off += int64(n)
		ptr.extentOff += n
		if ptr.extentOff == fn.extents[ptr.extentIdx].Len() {
			ptr.extentIdx++
			ptr.extentOff = 0
			if ptr.extentIdx < len(fn.extents) && err == io.EOF {
				err = nil
			}
		}
	}
	return
}

func (fn *filenode) Size() int64 {
	fn.RLock()
	defer fn.RUnlock()
	return fn.fileinfo.Size()
}

func (fn *filenode) Stat() os.FileInfo {
	fn.RLock()
	defer fn.RUnlock()
	return fn.fileinfo
}

func (fn *filenode) Truncate(size int64) error {
	fn.Lock()
	defer fn.Unlock()
	if size < fn.fileinfo.size {
		ptr := fn.seek(filenodePtr{off: size, repacked: fn.repacked - 1})
		for i := ptr.extentIdx; i < len(fn.extents); i++ {
			if ext, ok := fn.extents[i].(*memExtent); ok {
				fn.memsize -= int64(ext.Len())
			}
		}
		if ptr.extentOff == 0 {
			fn.extents = fn.extents[:ptr.extentIdx]
		} else {
			fn.extents = fn.extents[:ptr.extentIdx+1]
			switch ext := fn.extents[ptr.extentIdx].(type) {
			case *memExtent:
				ext.Truncate(ptr.extentOff)
				fn.memsize += int64(ext.Len())
			default:
				fn.extents[ptr.extentIdx] = ext.Slice(0, ptr.extentOff)
			}
		}
		fn.fileinfo.size = size
		fn.repacked++
		return nil
	}
	for size > fn.fileinfo.size {
		grow := size - fn.fileinfo.size
		var e writableExtent
		var ok bool
		if len(fn.extents) == 0 {
			e = &memExtent{}
			fn.extents = append(fn.extents, e)
		} else if e, ok = fn.extents[len(fn.extents)-1].(writableExtent); !ok || e.Len() >= maxBlockSize {
			e = &memExtent{}
			fn.extents = append(fn.extents, e)
		} else {
			fn.repacked++
		}
		if maxgrow := int64(maxBlockSize - e.Len()); maxgrow < grow {
			grow = maxgrow
		}
		e.Truncate(e.Len() + int(grow))
		fn.fileinfo.size += grow
		fn.memsize += grow
	}
	return nil
}

func (fn *filenode) Write(p []byte, startPtr filenodePtr) (n int, ptr filenodePtr, err error) {
	ptr = fn.seek(startPtr)
	if ptr.off < 0 {
		err = ErrNegativeOffset
		return
	}
	for len(p) > 0 && err == nil {
		cando := p
		if len(cando) > maxBlockSize {
			cando = cando[:maxBlockSize]
		}
		// Rearrange/grow fn.extents (and shrink cando if
		// needed) such that cando can be copied to
		// fn.extents[ptr.extentIdx] at offset ptr.extentOff.
		cur := ptr.extentIdx
		prev := ptr.extentIdx - 1
		var curWritable bool
		if cur < len(fn.extents) {
			_, curWritable = fn.extents[cur].(writableExtent)
		}
		var prevAppendable bool
		if prev >= 0 && fn.extents[prev].Len() < maxBlockSize {
			_, prevAppendable = fn.extents[prev].(writableExtent)
		}
		if ptr.extentOff > 0 && !curWritable {
			// Split a non-writable block.
			if max := fn.extents[cur].Len() - ptr.extentOff; max <= len(cando) {
				// Truncate cur, and insert a new
				// extent after it.
				cando = cando[:max]
				fn.extents = append(fn.extents, nil)
				copy(fn.extents[cur+1:], fn.extents[cur:])
			} else {
				// Split cur into two copies, truncate
				// the one on the left, shift the one
				// on the right, and insert a new
				// extent between them.
				fn.extents = append(fn.extents, nil, nil)
				copy(fn.extents[cur+2:], fn.extents[cur:])
				fn.extents[cur+2] = fn.extents[cur+2].Slice(ptr.extentOff+len(cando), -1)
			}
			cur++
			prev++
			e := &memExtent{}
			e.Truncate(len(cando))
			fn.memsize += int64(len(cando))
			fn.extents[cur] = e
			fn.extents[prev] = fn.extents[prev].Slice(0, ptr.extentOff)
			ptr.extentIdx++
			ptr.extentOff = 0
			fn.repacked++
			ptr.repacked++
		} else if curWritable {
			if fit := int(fn.extents[cur].Len()) - ptr.extentOff; fit < len(cando) {
				cando = cando[:fit]
			}
		} else {
			if prevAppendable {
				// Shrink cando if needed to fit in prev extent.
				if cangrow := maxBlockSize - fn.extents[prev].Len(); cangrow < len(cando) {
					cando = cando[:cangrow]
				}
			}

			if cur == len(fn.extents) {
				// ptr is at EOF, filesize is changing.
				fn.fileinfo.size += int64(len(cando))
			} else if el := fn.extents[cur].Len(); el <= len(cando) {
				// cando is long enough that we won't
				// need cur any more. shrink cando to
				// be exactly as long as cur
				// (otherwise we'd accidentally shift
				// the effective position of all
				// extents after cur).
				cando = cando[:el]
				copy(fn.extents[cur:], fn.extents[cur+1:])
				fn.extents = fn.extents[:len(fn.extents)-1]
			} else {
				// shrink cur by the same #bytes we're growing prev
				fn.extents[cur] = fn.extents[cur].Slice(len(cando), -1)
			}

			if prevAppendable {
				// Grow prev.
				ptr.extentIdx--
				ptr.extentOff = fn.extents[prev].Len()
				fn.extents[prev].(writableExtent).Truncate(ptr.extentOff + len(cando))
				fn.memsize += int64(len(cando))
				ptr.repacked++
				fn.repacked++
			} else {
				// Insert an extent between prev and cur, and advance prev/cur.
				fn.extents = append(fn.extents, nil)
				if cur < len(fn.extents) {
					copy(fn.extents[cur+1:], fn.extents[cur:])
					ptr.repacked++
					fn.repacked++
				} else {
					// appending a new extent does
					// not invalidate any ptrs
				}
				e := &memExtent{}
				e.Truncate(len(cando))
				fn.memsize += int64(len(cando))
				fn.extents[cur] = e
				cur++
				prev++
			}
		}

		// Finally we can copy bytes from cando to the current extent.
		fn.extents[ptr.extentIdx].(writableExtent).WriteAt(cando, ptr.extentOff)
		n += len(cando)
		p = p[len(cando):]

		ptr.off += int64(len(cando))
		ptr.extentOff += len(cando)
		if ptr.extentOff >= maxBlockSize {
			fn.pruneMemExtents()
		}
		if fn.extents[ptr.extentIdx].Len() == ptr.extentOff {
			ptr.extentOff = 0
			ptr.extentIdx++
		}
	}
	return
}

// Write some data out to disk to reduce memory use. Caller must have
// write lock.
func (fn *filenode) pruneMemExtents() {
	// TODO: async (don't hold Lock() while waiting for Keep)
	// TODO: share code with (*dirnode)sync()
	// TODO: pack/flush small blocks too, when fragmented
	for idx, ext := range fn.extents {
		ext, ok := ext.(*memExtent)
		if !ok || ext.Len() < maxBlockSize {
			continue
		}
		locator, _, err := fn.parent.kc.PutB(ext.buf)
		if err != nil {
			// TODO: stall (or return errors from)
			// subsequent writes until flushing
			// starts to succeed
			continue
		}
		fn.memsize -= int64(ext.Len())
		fn.extents[idx] = storedExtent{
			kc:      fn.parent.kc,
			locator: locator,
			size:    ext.Len(),
			offset:  0,
			length:  ext.Len(),
		}
	}
}

// FileSystem returns a CollectionFileSystem for the collection.
func (c *Collection) FileSystem(client *Client, kc keepClient) (CollectionFileSystem, error) {
	fs := &fileSystem{dirnode: dirnode{
		client:   client,
		kc:       kc,
		fileinfo: fileinfo{name: ".", mode: os.ModeDir | 0755},
		parent:   nil,
		inodes:   make(map[string]inode),
	}}
	fs.dirnode.parent = &fs.dirnode
	if err := fs.dirnode.loadManifest(c.ManifestText); err != nil {
		return nil, err
	}
	return fs, nil
}

type file struct {
	inode
	ptr        filenodePtr
	append     bool
	readable   bool
	writable   bool
	unreaddirs []os.FileInfo
}

func (f *file) Read(p []byte) (n int, err error) {
	if !f.readable {
		return 0, ErrWriteOnlyMode
	}
	f.inode.RLock()
	defer f.inode.RUnlock()
	n, f.ptr, err = f.inode.Read(p, f.ptr)
	return
}

func (f *file) Seek(off int64, whence int) (pos int64, err error) {
	size := f.inode.Size()
	ptr := f.ptr
	switch whence {
	case os.SEEK_SET:
		ptr.off = off
	case os.SEEK_CUR:
		ptr.off += off
	case os.SEEK_END:
		ptr.off = size + off
	}
	if ptr.off < 0 {
		return f.ptr.off, ErrNegativeOffset
	}
	if ptr.off > size {
		ptr.off = size
	}
	if ptr.off != f.ptr.off {
		f.ptr = ptr
		// force filenode to recompute f.ptr fields on next
		// use
		f.ptr.repacked = -1
	}
	return f.ptr.off, nil
}

func (f *file) Truncate(size int64) error {
	return f.inode.Truncate(size)
}

func (f *file) Write(p []byte) (n int, err error) {
	if !f.writable {
		return 0, ErrReadOnlyFile
	}
	f.inode.Lock()
	defer f.inode.Unlock()
	if fn, ok := f.inode.(*filenode); ok && f.append {
		f.ptr = filenodePtr{
			off:       fn.fileinfo.size,
			extentIdx: len(fn.extents),
			extentOff: 0,
			repacked:  fn.repacked,
		}
	}
	n, f.ptr, err = f.inode.Write(p, f.ptr)
	return
}

func (f *file) Readdir(count int) ([]os.FileInfo, error) {
	if !f.inode.Stat().IsDir() {
		return nil, ErrInvalidOperation
	}
	if count <= 0 {
		return f.inode.Readdir(), nil
	}
	if f.unreaddirs == nil {
		f.unreaddirs = f.inode.Readdir()
	}
	if len(f.unreaddirs) == 0 {
		return nil, io.EOF
	}
	if count > len(f.unreaddirs) {
		count = len(f.unreaddirs)
	}
	ret := f.unreaddirs[:count]
	f.unreaddirs = f.unreaddirs[count:]
	return ret, nil
}

func (f *file) Stat() (os.FileInfo, error) {
	return f.inode.Stat(), nil
}

func (f *file) Close() error {
	// FIXME: flush
	return nil
}

type dirnode struct {
	fileinfo fileinfo
	parent   *dirnode
	client   *Client
	kc       keepClient
	inodes   map[string]inode
	sync.RWMutex
}

// sync flushes in-memory data (for all files in the tree rooted at
// dn) to persistent storage. Caller must hold dn.Lock().
func (dn *dirnode) sync() error {
	type shortBlock struct {
		fn  *filenode
		idx int
	}
	var pending []shortBlock
	var pendingLen int

	flush := func(sbs []shortBlock) error {
		if len(sbs) == 0 {
			return nil
		}
		block := make([]byte, 0, maxBlockSize)
		for _, sb := range sbs {
			block = append(block, sb.fn.extents[sb.idx].(*memExtent).buf...)
		}
		locator, _, err := dn.kc.PutB(block)
		if err != nil {
			return err
		}
		off := 0
		for _, sb := range sbs {
			data := sb.fn.extents[sb.idx].(*memExtent).buf
			sb.fn.extents[sb.idx] = storedExtent{
				kc:      dn.kc,
				locator: locator,
				size:    len(block),
				offset:  off,
				length:  len(data),
			}
			off += len(data)
			sb.fn.memsize -= int64(len(data))
		}
		return nil
	}

	names := make([]string, 0, len(dn.inodes))
	for name := range dn.inodes {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		fn, ok := dn.inodes[name].(*filenode)
		if !ok {
			continue
		}
		fn.Lock()
		defer fn.Unlock()
		for idx, ext := range fn.extents {
			ext, ok := ext.(*memExtent)
			if !ok {
				continue
			}
			if ext.Len() > maxBlockSize/2 {
				if err := flush([]shortBlock{{fn, idx}}); err != nil {
					return err
				}
				continue
			}
			if pendingLen+ext.Len() > maxBlockSize {
				if err := flush(pending); err != nil {
					return err
				}
				pending = nil
				pendingLen = 0
			}
			pending = append(pending, shortBlock{fn, idx})
			pendingLen += ext.Len()
		}
	}
	return flush(pending)
}

func (dn *dirnode) MarshalManifest(prefix string) (string, error) {
	dn.Lock()
	defer dn.Unlock()
	return dn.marshalManifest(prefix)
}

// caller must have read lock.
func (dn *dirnode) marshalManifest(prefix string) (string, error) {
	var streamLen int64
	type m1segment struct {
		name   string
		offset int64
		length int64
	}
	var segments []m1segment
	var subdirs string
	var blocks []string

	if err := dn.sync(); err != nil {
		return "", err
	}

	names := make([]string, 0, len(dn.inodes))
	for name, node := range dn.inodes {
		names = append(names, name)
		node.Lock()
		defer node.Unlock()
	}
	sort.Strings(names)

	for _, name := range names {
		node := dn.inodes[name]
		switch node := node.(type) {
		case *dirnode:
			subdir, err := node.marshalManifest(prefix + "/" + name)
			if err != nil {
				return "", err
			}
			subdirs = subdirs + subdir
		case *filenode:
			if len(node.extents) == 0 {
				segments = append(segments, m1segment{name: name})
				break
			}
			for _, e := range node.extents {
				switch e := e.(type) {
				case storedExtent:
					if len(blocks) > 0 && blocks[len(blocks)-1] == e.locator {
						streamLen -= int64(e.size)
					} else {
						blocks = append(blocks, e.locator)
					}
					next := m1segment{
						name:   name,
						offset: streamLen + int64(e.offset),
						length: int64(e.length),
					}
					if prev := len(segments) - 1; prev >= 0 &&
						segments[prev].name == name &&
						segments[prev].offset+segments[prev].length == next.offset {
						segments[prev].length += next.length
					} else {
						segments = append(segments, next)
					}
					streamLen += int64(e.size)
				default:
					// This can't happen: we
					// haven't unlocked since
					// calling sync().
					panic(fmt.Sprintf("can't marshal extent type %T", e))
				}
			}
		default:
			panic(fmt.Sprintf("can't marshal inode type %T", node))
		}
	}
	var filetokens []string
	for _, s := range segments {
		filetokens = append(filetokens, fmt.Sprintf("%d:%d:%s", s.offset, s.length, manifestEscape(s.name)))
	}
	if len(filetokens) == 0 {
		return subdirs, nil
	} else if len(blocks) == 0 {
		blocks = []string{"d41d8cd98f00b204e9800998ecf8427e+0"}
	}
	return manifestEscape(prefix) + " " + strings.Join(blocks, " ") + " " + strings.Join(filetokens, " ") + "\n" + subdirs, nil
}

func (dn *dirnode) loadManifest(txt string) error {
	// FIXME: faster
	var dirname string
	streams := strings.Split(txt, "\n")
	if streams[len(streams)-1] != "" {
		return fmt.Errorf("line %d: no trailing newline", len(streams))
	}
	for i, stream := range streams[:len(streams)-1] {
		lineno := i + 1
		var extents []storedExtent
		var anyFileTokens bool
		for i, token := range strings.Split(stream, " ") {
			if i == 0 {
				dirname = manifestUnescape(token)
				continue
			}
			if !strings.Contains(token, ":") {
				if anyFileTokens {
					return fmt.Errorf("line %d: bad file segment %q", lineno, token)
				}
				toks := strings.SplitN(token, "+", 3)
				if len(toks) < 2 {
					return fmt.Errorf("line %d: bad locator %q", lineno, token)
				}
				length, err := strconv.ParseInt(toks[1], 10, 32)
				if err != nil || length < 0 {
					return fmt.Errorf("line %d: bad locator %q", lineno, token)
				}
				extents = append(extents, storedExtent{
					locator: token,
					size:    int(length),
					offset:  0,
					length:  int(length),
				})
				continue
			} else if len(extents) == 0 {
				return fmt.Errorf("line %d: bad locator %q", lineno, token)
			}

			toks := strings.Split(token, ":")
			if len(toks) != 3 {
				return fmt.Errorf("line %d: bad file segment %q", lineno, token)
			}
			anyFileTokens = true

			offset, err := strconv.ParseInt(toks[0], 10, 64)
			if err != nil || offset < 0 {
				return fmt.Errorf("line %d: bad file segment %q", lineno, token)
			}
			length, err := strconv.ParseInt(toks[1], 10, 64)
			if err != nil || length < 0 {
				return fmt.Errorf("line %d: bad file segment %q", lineno, token)
			}
			name := path.Clean(dirname + "/" + manifestUnescape(toks[2]))
			err = dn.makeParentDirs(name)
			if err != nil {
				return fmt.Errorf("line %d: cannot use path %q: %s", lineno, name, err)
			}
			f, err := dn.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0700)
			if err != nil {
				return fmt.Errorf("line %d: cannot append to %q: %s", lineno, name, err)
			}
			if f.inode.Stat().IsDir() {
				f.Close()
				return fmt.Errorf("line %d: cannot append to %q: is a directory", lineno, name)
			}
			// Map the stream offset/range coordinates to
			// block/offset/range coordinates and add
			// corresponding storedExtents to the filenode
			var pos int64
			for _, e := range extents {
				next := pos + int64(e.Len())
				if next < offset {
					pos = next
					continue
				}
				if pos > offset+length {
					break
				}
				var blkOff int
				if pos < offset {
					blkOff = int(offset - pos)
				}
				blkLen := e.Len() - blkOff
				if pos+int64(blkOff+blkLen) > offset+length {
					blkLen = int(offset + length - pos - int64(blkOff))
				}
				f.inode.(*filenode).appendExtent(storedExtent{
					kc:      dn.kc,
					locator: e.locator,
					size:    e.size,
					offset:  blkOff,
					length:  blkLen,
				})
				pos = next
			}
			f.Close()
			if pos < offset+length {
				return fmt.Errorf("line %d: invalid segment in %d-byte stream: %q", lineno, pos, token)
			}
		}
		if !anyFileTokens {
			return fmt.Errorf("line %d: no file segments", lineno)
		} else if len(extents) == 0 {
			return fmt.Errorf("line %d: no locators", lineno)
		} else if dirname == "" {
			return fmt.Errorf("line %d: no stream name", lineno)
		}
	}
	return nil
}

func (dn *dirnode) makeParentDirs(name string) (err error) {
	names := strings.Split(name, "/")
	for _, name := range names[:len(names)-1] {
		f, err := dn.OpenFile(name, os.O_CREATE, os.ModeDir|0755)
		if err != nil {
			return err
		}
		defer f.Close()
		var ok bool
		dn, ok = f.inode.(*dirnode)
		if !ok {
			return ErrFileExists
		}
	}
	return nil
}

func (dn *dirnode) mkdir(name string) (*file, error) {
	return dn.OpenFile(name, os.O_CREATE|os.O_EXCL, os.ModeDir|0755)
}

func (dn *dirnode) Mkdir(name string, perm os.FileMode) error {
	f, err := dn.mkdir(name)
	if err == nil {
		err = f.Close()
	}
	return err
}

func (dn *dirnode) Remove(name string) error {
	dirname, name := path.Split(name)
	if name == "" || name == "." || name == ".." {
		return ErrInvalidOperation
	}
	dn, ok := dn.lookupPath(dirname).(*dirnode)
	if !ok {
		return os.ErrNotExist
	}
	dn.Lock()
	defer dn.Unlock()
	switch node := dn.inodes[name].(type) {
	case nil:
		return os.ErrNotExist
	case *dirnode:
		node.RLock()
		defer node.RUnlock()
		if len(node.inodes) > 0 {
			return ErrDirectoryNotEmpty
		}
	}
	delete(dn.inodes, name)
	return nil
}

func (dn *dirnode) Parent() inode {
	dn.RLock()
	defer dn.RUnlock()
	return dn.parent
}

func (dn *dirnode) Readdir() (fi []os.FileInfo) {
	dn.RLock()
	defer dn.RUnlock()
	fi = make([]os.FileInfo, 0, len(dn.inodes))
	for _, inode := range dn.inodes {
		fi = append(fi, inode.Stat())
	}
	return
}

func (dn *dirnode) Read(p []byte, ptr filenodePtr) (int, filenodePtr, error) {
	return 0, ptr, ErrInvalidOperation
}

func (dn *dirnode) Write(p []byte, ptr filenodePtr) (int, filenodePtr, error) {
	return 0, ptr, ErrInvalidOperation
}

func (dn *dirnode) Size() int64 {
	dn.RLock()
	defer dn.RUnlock()
	return dn.fileinfo.Size()
}

func (dn *dirnode) Stat() os.FileInfo {
	dn.RLock()
	defer dn.RUnlock()
	return dn.fileinfo
}

func (dn *dirnode) Truncate(int64) error {
	return ErrInvalidOperation
}

// lookupPath returns the inode for the file/directory with the given
// name (which may contain "/" separators), along with its parent
// node. If no such file/directory exists, the returned node is nil.
func (dn *dirnode) lookupPath(path string) (node inode) {
	node = dn
	for _, name := range strings.Split(path, "/") {
		dn, ok := node.(*dirnode)
		if !ok {
			return nil
		}
		if name == "." || name == "" {
			continue
		}
		if name == ".." {
			node = node.Parent()
			continue
		}
		dn.RLock()
		node = dn.inodes[name]
		dn.RUnlock()
	}
	return
}

func (dn *dirnode) OpenFile(name string, flag int, perm os.FileMode) (*file, error) {
	if flag&os.O_SYNC != 0 {
		return nil, ErrSyncNotSupported
	}
	dirname, name := path.Split(name)
	dn, ok := dn.lookupPath(dirname).(*dirnode)
	if !ok {
		return nil, os.ErrNotExist
	}
	var readable, writable bool
	switch flag & (os.O_RDWR | os.O_RDONLY | os.O_WRONLY) {
	case os.O_RDWR:
		readable = true
		writable = true
	case os.O_RDONLY:
		readable = true
	case os.O_WRONLY:
		writable = true
	default:
		return nil, fmt.Errorf("invalid flags 0x%x", flag)
	}
	if !writable {
		// A directory can be opened via "foo/", "foo/.", or
		// "foo/..".
		switch name {
		case ".", "":
			return &file{inode: dn}, nil
		case "..":
			return &file{inode: dn.Parent()}, nil
		}
	}
	createMode := flag&os.O_CREATE != 0
	if createMode {
		dn.Lock()
		defer dn.Unlock()
	} else {
		dn.RLock()
		defer dn.RUnlock()
	}
	n, ok := dn.inodes[name]
	if !ok {
		if !createMode {
			return nil, os.ErrNotExist
		}
		if perm.IsDir() {
			n = &dirnode{
				parent: dn,
				client: dn.client,
				kc:     dn.kc,
				fileinfo: fileinfo{
					name: name,
					mode: os.ModeDir | 0755,
				},
			}
		} else {
			n = &filenode{
				parent: dn,
				fileinfo: fileinfo{
					name: name,
					mode: 0755,
				},
			}
		}
		if dn.inodes == nil {
			dn.inodes = make(map[string]inode)
		}
		dn.inodes[name] = n
		dn.fileinfo.size++
	} else if flag&os.O_EXCL != 0 {
		return nil, ErrFileExists
	}
	return &file{
		inode:    n,
		append:   flag&os.O_APPEND != 0,
		readable: readable,
		writable: writable,
	}, nil
}

type extent interface {
	io.ReaderAt
	Len() int
	// Return a new extent with a subsection of the data from this
	// one. length<0 means length=Len()-off.
	Slice(off int, length int) extent
}

type writableExtent interface {
	extent
	WriteAt(p []byte, off int)
	Truncate(n int)
}

type memExtent struct {
	buf []byte
}

func (me *memExtent) Len() int {
	return len(me.buf)
}

func (me *memExtent) Slice(off, length int) extent {
	if length < 0 {
		length = len(me.buf) - off
	}
	buf := make([]byte, length)
	copy(buf, me.buf[off:])
	return &memExtent{buf: buf}
}

func (me *memExtent) Truncate(n int) {
	if n > cap(me.buf) {
		newsize := 1024
		for newsize < n {
			newsize = newsize << 2
		}
		newbuf := make([]byte, n, newsize)
		copy(newbuf, me.buf)
		me.buf = newbuf
	} else {
		// Zero unused part when shrinking, in case we grow
		// and start using it again later.
		for i := n; i < len(me.buf); i++ {
			me.buf[i] = 0
		}
	}
	me.buf = me.buf[:n]
}

func (me *memExtent) WriteAt(p []byte, off int) {
	if off+len(p) > len(me.buf) {
		panic("overflowed extent")
	}
	copy(me.buf[off:], p)
}

func (me *memExtent) ReadAt(p []byte, off int64) (n int, err error) {
	if off > int64(me.Len()) {
		err = io.EOF
		return
	}
	n = copy(p, me.buf[int(off):])
	if n < len(p) {
		err = io.EOF
	}
	return
}

type storedExtent struct {
	kc      keepClient
	locator string
	size    int
	offset  int
	length  int
}

func (se storedExtent) Len() int {
	return se.length
}

func (se storedExtent) Slice(n, size int) extent {
	se.offset += n
	se.length -= n
	if size >= 0 && se.length > size {
		se.length = size
	}
	return se
}

func (se storedExtent) ReadAt(p []byte, off int64) (n int, err error) {
	if off > int64(se.length) {
		return 0, io.EOF
	}
	maxlen := se.length - int(off)
	if len(p) > maxlen {
		p = p[:maxlen]
		n, err = se.kc.ReadAt(se.locator, p, int(off)+se.offset)
		if err == nil {
			err = io.EOF
		}
		return
	}
	return se.kc.ReadAt(se.locator, p, int(off)+se.offset)
}

func canonicalName(name string) string {
	name = path.Clean("/" + name)
	if name == "/" || name == "./" {
		name = "."
	} else if strings.HasPrefix(name, "/") {
		name = "." + name
	}
	return name
}

var manifestEscapeSeq = regexp.MustCompile(`\\([0-9]{3}|\\)`)

func manifestUnescapeFunc(seq string) string {
	if seq == `\\` {
		return `\`
	}
	i, err := strconv.ParseUint(seq[1:], 8, 8)
	if err != nil {
		// Invalid escape sequence: can't unescape.
		return seq
	}
	return string([]byte{byte(i)})
}

func manifestUnescape(s string) string {
	return manifestEscapeSeq.ReplaceAllStringFunc(s, manifestUnescapeFunc)
}

var manifestEscapedChar = regexp.MustCompile(`[^\.\w/]`)

func manifestEscapeFunc(seq string) string {
	return fmt.Sprintf("\\%03o", byte(seq[0]))
}

func manifestEscape(s string) string {
	return manifestEscapedChar.ReplaceAllStringFunc(s, manifestEscapeFunc)
}
