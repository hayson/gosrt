package poll

// #cgo LDFLAGS: -lsrt
// #include <srt/srt.h>
import "C"
import (
	"io"
	"syscall"
	"unsafe"

	"github.com/openfresh/gosrt/def"
	"github.com/openfresh/gosrt/util"
)

// Single-word zero for use when we need a valid pointer to 0 bytes.
var _zero uintptr

// FD is a file descriptor. The net and os packages use this type as a
// field of a larger type representing a network connection or OS file.
type FD struct {
	// System file descriptor. Immutable until Close.
	Sysfd int

	// I/O poller.
	pd pollDesc
}

// Init initializes the FD. The Sysfd field should already be set.
// This can be called multiple times on a single FD.
// The net argument is a network name from the net package (e.g., "tcp"),
// or "file".
// Set pollable to true if fd should be managed by runtime netpoll.
func (fd *FD) Init(net string, pollable bool) error {
	return fd.pd.init(fd)
}

// Destroy closes the file descriptor. This is called when there are
// no remaining references.
func (fd *FD) destroy() error {
	// Poller may want to unregister fd in readiness notification mechanism,
	// so this must be executed before CloseFunc.
	fd.pd.close()
	err := CloseFunc(fd.Sysfd)
	fd.Sysfd = -1
	return err
}

// Close closes the FD. The underlying file descriptor is closed by the
// destroy method when there are no remaining references.
func (fd *FD) Close() error {
	// Unblock any I/O.  Once it all unblocks and returns,
	// so that it cannot be referring to fd.sysfd anymore,
	// the final decref will close fd.sysfd. This should happen
	// fairly quickly, since all the I/O is non-blocking, and any
	// attempts to block in the pollDesc will return errClosing(fd.isFile).
	fd.pd.evict()

	return nil
}

// Darwin and FreeBSD can't read or write 2GB+ files at a time,
// even on 64-bit systems.
// The same is true of socket implementations on many systems.
// See golang.org/issue/7812 and golang.org/issue/16266.
// Use 1GB instead of, say, 2GB-1, to keep subsequent reads aligned.
const maxRW = 1 << 30

// Read implements io.Reader.
func (fd *FD) Read(p []byte) (int, error) {
	if err := fd.readLock(); err != nil {
		return 0, err
	}
	defer fd.readUnlock()
	if len(p) == 0 {
		// If the caller wanted a zero byte read, return immediately
		// without trying (but after acquiring the readLock).
		// Otherwise syscall.Read returns 0, nil which looks like
		// io.EOF.
		// TODO(bradfitz): make it wait for readability? (Issue 15735)
		return 0, nil
	}
	if err := fd.pd.prepareRead(); err != nil {
		return 0, err
	}
	if len(p) > maxRW {
		p = p[:maxRW]
	}
	for {
		n, err := syscall.Read(fd.Sysfd, p)
		if err != nil {
			n = 0
			if err == syscall.EAGAIN && fd.pd.pollable() {
				if err = fd.pd.waitRead(); err == nil {
					continue
				}
			}
		}
		err = fd.eofError(n, err)
		return n, err
	}
}

// Write implements io.Writer.
func (fd *FD) Write(p []byte) (int, error) {
	if err := fd.writeLock(); err != nil {
		return 0, err
	}
	defer fd.writeUnlock()
	if err := fd.pd.prepareWrite(); err != nil {
		return 0, err
	}
	var nn int
	for {
		max := len(p)
		if max-nn > maxRW {
			max = nn + maxRW
		}
		n, err := syscall.Write(fd.Sysfd, p[nn:max])
		if n > 0 {
			nn += n
		}
		if nn == len(p) {
			return nn, err
		}
		if err == syscall.EAGAIN && fd.pd.pollable() {
			if err = fd.pd.waitWrite(); err == nil {
				continue
			}
		}
		if err != nil {
			return nn, err
		}
		if n == 0 {
			return nn, io.ErrUnexpectedEOF
		}
	}
}

// Accept wraps the accept network call.
func (fd *FD) Accept() (int, *syscall.RawSockaddrAny, string, error) {
	if err := fd.readLock(); err != nil {
		return -1, nil, "", err
	}
	defer fd.readUnlock()

	if err := fd.pd.prepareRead(); err != nil {
		return -1, nil, "", err
	}
	for {
		s, rsa, errcall, err := accept(fd.Sysfd)
		if err == nil {
			return s, rsa, "", err
		}
		switch err {
		case syscall.EAGAIN:
			if fd.pd.pollable() {
				if err = fd.pd.waitRead(); err == nil {
					continue
				}
			}
		case syscall.ECONNABORTED:
			// This means that a socket on the listen
			// queue was closed before we Accept()ed it;
			// it's a silly error, so try again.
			continue
		}
		return -1, nil, errcall, err
	}
}

func (fd *FD) read(p []byte) (n int, err error) {
	var _p0 *byte
	if len(p) > 0 {
		_p0 = &p[0]
	}
	n = int(C.srt_recv(C.SRTSOCKET(fd.Sysfd), (*C.char)(unsafe.Pointer(_p0)), C.int(len(p))))
	if n == def.SRT_ERROR {
		return 0, util.GetLastError("srt_recv")
	}
	return n, nil
}

func (fd *FD) write(p []byte) (nn int, err error) {
	var _p0 unsafe.Pointer
	if len(p) > 0 {
		_p0 = unsafe.Pointer(&p[0])
	} else {
		_p0 = unsafe.Pointer(&_zero)
	}
	nn = int(C.srt_send(C.SRTSOCKET(fd.Sysfd), (*C.char)(_p0), C.int(len(p))))
	if nn == def.SRT_ERROR {
		return 0, util.GetLastError("srt_send")
	}
	return nn, nil

}
