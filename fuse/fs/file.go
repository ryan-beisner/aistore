// Package fs implements an AIStore file system.
/*
 * Copyright (c) 2019, NVIDIA CORPORATION. All rights reserved.
 */
package fs

import (
	"context"
	"io"
	"path"
	"syscall"

	"github.com/jacobsa/fuse"
	"github.com/jacobsa/fuse/fuseops"
)

// OpenFile creates a file handle to be used in subsequent file operations
// that provide a valid handle ID (also generated here).
func (fs *aisfs) OpenFile(ctx context.Context, req *fuseops.OpenFileOp) (err error) {
	fs.mu.Lock()
	file := fs.lookupFileMustExist(req.Inode)
	req.Handle = fs.allocateFileHandle(file)
	fs.mu.Unlock()
	return
}

func (fs *aisfs) CreateFile(ctx context.Context, req *fuseops.CreateFileOp) (err error) {
	var newFile Inode

	fs.mu.RLock()
	parent := fs.lookupDirMustExist(req.Parent)
	fs.mu.RUnlock()

	fileName := path.Join(parent.Path(), req.Name)
	object, err := parent.LinkNewFile(fileName)
	if err != nil {
		return fs.handleIOError(err)
	}

	// Allocate an inodeID for this file inode
	inodeID := fs.nextInodeID()

	parent.Lock()
	parent.NewFileEntry(req.Name, inodeID, object.Size)
	parent.Unlock()

	fs.mu.Lock()
	newFile = fs.createFileInode(inodeID, parent, object, req.Mode)
	req.Handle = fs.allocateFileHandle(newFile.(*FileInode))

	// Locking this inode with parent already locked doesn't break
	// the valid locking order since (currently) child inodes
	// have higher ID than their respective parent inodes.
	newFile.RLock()
	req.Entry = newFile.AsChildEntry()
	newFile.RUnlock()
	newFile.IncLookupCount()
	fs.mu.Unlock()
	return
}

func (fs *aisfs) ReadFile(ctx context.Context, req *fuseops.ReadFileOp) (err error) {
	fs.mu.RLock()
	fhandle := fs.lookupFhandleMustExist(req.Handle)
	fs.mu.RUnlock()

	req.BytesRead, err = fhandle.readChunk(req.Dst, req.Offset)

	// As required by FUSE, io.EOF should not be reported as an error.
	if err == io.EOF {
		err = nil
	}

	if err != nil {
		return fs.handleIOError(err)
	}
	return
}

func (fs *aisfs) WriteFile(ctx context.Context, req *fuseops.WriteFileOp) (err error) {
	fs.mu.RLock()
	handle := fs.lookupFhandleMustExist(req.Handle)
	fs.mu.RUnlock()

	err = handle.writeChunk(req.Data, uint64(req.Offset), fs.cfg.MaxWriteBufSize)
	if err != nil {
		return fs.handleIOError(err)
	}
	return
}

func (fs *aisfs) FlushFile(ctx context.Context, req *fuseops.FlushFileOp) (err error) {
	fs.mu.RLock()
	handle := fs.lookupFhandleMustExist(req.Handle)
	fs.mu.RUnlock()

	if err = handle.flush(); err != nil {
		return fs.handleIOError(err)
	}
	return
}

func (fs *aisfs) ReleaseFileHandle(ctx context.Context, req *fuseops.ReleaseFileHandleOp) (err error) {
	fs.mu.Lock()

	// Lookup and release the handle's resources.
	fhandle := fs.lookupFhandleMustExist(req.Handle)
	fhandle.destroy()

	// Remove the handle from the file handles table.
	delete(fs.fileHandles, req.Handle)

	fs.mu.Unlock()
	return
}

func (fs *aisfs) Unlink(ctx context.Context, req *fuseops.UnlinkOp) (err error) {
	fs.mu.RLock()
	parent := fs.lookupDirMustExist(req.Parent)
	fs.mu.RUnlock()

	result := parent.LookupEntry(req.Name)
	if result.NoEntry() || result.NoInode() {
		return fuse.ENOENT
	}

	if result.IsDir() {
		fs.logf("tried to unlink directory: %q in %d", req.Name, req.Parent)
		return syscall.EISDIR
	}

	err = parent.UnlinkEntry(req.Name)
	if err != nil {
		return fs.handleIOError(err)
	}
	return
}
