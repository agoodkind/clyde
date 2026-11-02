package codestore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	lockFileSuffix  = ".lock"
	lockPollMinimum = time.Millisecond
	lockPollMaximum = 20 * time.Millisecond
)

// collectionLockPath returns the kernel lock file path beside the collection directory.
func collectionLockPath(dir string) string {
	return dir + lockFileSuffix
}

// lockCollectionDirectory takes the exclusive kernel lock on the collection lock file.
// It returns an error that wraps the context error if the request context ends
// during the wait.
func lockCollectionDirectory(ctx context.Context, dir string) (*os.File, error) {
	return lockCollectionFile(ctx, dir, syscall.LOCK_EX)
}

// lockCollectionFile checks the request context before it opens the collection lock
// file. It tries a nonblocking kernel lock in the given mode and waits between
// attempts until it owns the lock or the context ends. It checks the context again
// after it has the lock. If the context ends, it closes any file it opened and
// returns nil with an error that wraps the context error.
// The caller owns the lock until it closes the returned file.
func lockCollectionFile(ctx context.Context, dir string, mode int) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, failed("wait for the collection lock in "+dir, err)
	}
	file, err := os.OpenFile(collectionLockPath(dir), os.O_CREATE|os.O_RDWR, fileMode)
	if err != nil {
		return nil, failed("open the collection lock file for "+dir, err)
	}
	delay := lockPollMinimum
	for {
		lockErr := syscall.Flock(int(file.Fd()), mode|syscall.LOCK_NB)
		if lockErr == nil {
			if err := ctx.Err(); err != nil {
				_ = file.Close()
				return nil, failed("wait for the collection lock in "+dir, err)
			}
			return file, nil
		}
		if !errors.Is(lockErr, syscall.EWOULDBLOCK) && !errors.Is(lockErr, syscall.EINTR) {
			_ = file.Close()
			return nil, failed("lock the collection in "+dir, lockErr)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, failed("wait for the collection lock in "+dir, ctx.Err())
		case <-time.After(delay):
		}
		delay = min(delay*2, lockPollMaximum)
	}
}

// recordHeaderIdentity stores the current header's file identity in headerInfo.
// If [os.Stat] fails, it sets headerInfo to nil and returns the error.
func (stored *codeCollection) recordHeaderIdentity() error {
	info, err := os.Stat(filepath.Join(stored.dir, headerFileName))
	if err != nil {
		stored.headerInfo = nil
		return failed("inspect the collection header in "+stored.dir, err)
	}
	stored.headerInfo = info
	return nil
}

// lockForWrite takes the exclusive kernel lock before the collection mutex.
// A request that owns the kernel lock waits for the mutex only while Close
// closes the handle. If Close has closed the handle, lockForWrite returns
// ErrStoreClosed. Otherwise, it applies changes from other stores. The caller
// writes at the current end of each file. After the write, the returned release function
// releases the mutex first and the kernel lock second.
func (stored *codeCollection) lockForWrite(ctx context.Context) (func(), error) {
	lock, err := lockCollectionDirectory(ctx, stored.dir)
	if err != nil {
		return nil, err
	}
	stored.mutex.Lock()
	unlock := func() {
		stored.mutex.Unlock()
		_ = lock.Close()
	}
	if stored.closed {
		unlock()
		return nil, ErrStoreClosed
	}
	if err := stored.syncWithDisk(); err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

// lockForRead takes a shared kernel lock before taking the collection read lock.
// It returns ErrStoreClosed if Close has closed the handle. A request that owns
// the kernel lock waits for the mutex only while Close closes the handle.
// If the files differ from memory, it releases both locks and calls lockForWrite
// to apply the changes under exclusive locks. The caller reads with both locks
// and calls the returned function to release the mutex before the kernel lock.
func (stored *codeCollection) lockForRead(ctx context.Context) (func(), error) {
	lock, err := lockCollectionFile(ctx, stored.dir, syscall.LOCK_SH)
	if err != nil {
		return nil, err
	}
	stored.mutex.RLock()
	unlock := func() {
		stored.mutex.RUnlock()
		_ = lock.Close()
	}
	if stored.closed {
		unlock()
		return nil, ErrStoreClosed
	}
	changed, err := stored.diskChanged()
	if err != nil {
		unlock()
		return nil, err
	}
	if !changed {
		return unlock, nil
	}
	unlock()
	return stored.lockForWrite(ctx)
}

// diskChanged reports whether the header file or row log differs from the state
// in memory.
func (stored *codeCollection) diskChanged() (bool, error) {
	if stored.headerInfo == nil {
		return true, nil
	}
	replaced, err := stored.filesReplaced()
	if err != nil || replaced {
		return replaced, err
	}
	info, err := stored.rowsFile.Stat()
	if err != nil {
		return false, failed("inspect the row log in "+stored.dir, err)
	}
	return info.Size() != stored.logSize, nil
}

// filesReplaced reports whether another file replaced the header or row log,
// or whether the row log path is missing. A header replacement can change the
// generation or declared columns.
func (stored *codeCollection) filesReplaced() (bool, error) {
	header, err := os.Stat(filepath.Join(stored.dir, headerFileName))
	if err != nil {
		return false, failed("inspect the collection header in "+stored.dir, err)
	}
	if !os.SameFile(stored.headerInfo, header) || !stored.headerInfo.ModTime().Equal(header.ModTime()) {
		return true, nil
	}
	current, err := os.Stat(stored.rowsPath(stored.header.Generation))
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, failed("inspect the row log in "+stored.dir, err)
	}
	opened, err := stored.rowsFile.Stat()
	if err != nil {
		return false, failed("inspect the row log in "+stored.dir, err)
	}
	return !os.SameFile(opened, current), nil
}

// syncWithDisk applies changes from other stores. The caller owns the collection
// mutex and kernel lock. syncWithDisk applies new row log records in order.
// If another file replaced the header or row log, or the row log became shorter,
// syncWithDisk rebuilds the collection state from the files.
func (stored *codeCollection) syncWithDisk() error {
	if stored.headerInfo == nil {
		return stored.reload()
	}
	replaced, err := stored.filesReplaced()
	if err != nil {
		return err
	}
	if replaced {
		return stored.reload()
	}
	info, err := stored.rowsFile.Stat()
	if err != nil {
		return failed("inspect the row log in "+stored.dir, err)
	}
	if info.Size() < stored.logSize {
		return stored.reload()
	}
	data, err := stored.dataFile.Stat()
	if err != nil {
		return failed("inspect the data file in "+stored.dir, err)
	}
	stored.dataSize = data.Size()
	if info.Size() == stored.logSize {
		return nil
	}
	return stored.replay()
}

// reload rebuilds the collection state from the current header and generation files.
func (stored *codeCollection) reload() error {
	fresh, err := loadCollection(stored.dir)
	if err != nil {
		return err
	}
	stored.closeFiles()
	stored.header = fresh.header
	stored.headerInfo = fresh.headerInfo
	stored.columnIndex = fresh.columnIndex
	stored.dictionaries = fresh.dictionaries
	stored.rows = fresh.rows
	stored.codes = fresh.codes
	stored.byID = fresh.byID
	stored.rowsFile = fresh.rowsFile
	stored.dataFile = fresh.dataFile
	stored.dataSize = fresh.dataSize
	stored.logSize = fresh.logSize
	stored.dead = fresh.dead
	return nil
}
