//go:build windows

package prompt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fileNameNormalized is GetFinalPathNameByHandle's FILE_NAME_NORMALIZED with VOLUME_NAME_DOS (both 0).
const fileNameNormalized = 0

// openCanonical opens path, following symlinks and junctions, and returns the file with its canonical
// path as Windows reports it for the open handle, so the path checked is the file that is read.
// filepath.EvalSymlinks is not used: since Go 1.23 it no longer resolves junctions (mount points).
func openCanonical(path string) (*os.File, string, error) {
	f, err := os.Open(path) //nolint:gosec // the caller checks the canonical path before reading
	if err != nil {
		return nil, "", fmt.Errorf("open %s: %w", path, err)
	}
	canon, err := finalPath(windows.Handle(f.Fd()))
	if err != nil {
		_ = f.Close()
		return nil, "", fmt.Errorf("canonical path of %s: %w", path, err)
	}
	return f, canon, nil
}

// finalPath is the canonical path of the open file h.
func finalPath(h windows.Handle) (string, error) {
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), fileNameNormalized)
	if err != nil || n == 0 || int(n) > len(buf) {
		return "", fmt.Errorf("final path: %w", err)
	}
	return stripExtendedPrefix(windows.UTF16ToString(buf[:n])), nil
}

// workdir is the working directory mentioned files are read from, by its canonical path.
type workdir struct {
	root string
}

// openWorkdir resolves the canonical path of the absolute working directory abs. No handle is kept:
// each file's canonical path comes from its own open handle, so the path checked is the file read.
func openWorkdir(abs string) (*workdir, error) {
	f, root, err := openCanonical(abs)
	if err != nil {
		return nil, fmt.Errorf("working directory: %w", err)
	}
	_ = f.Close()
	return &workdir{root: root}, nil
}

func (w *workdir) Close() error { return nil }

// open opens the file path names and returns it with its canonical path, which the caller checks
// against the working directory. The path is never handed to the kernel whole, which would follow
// links on its own: walk opens one name at a time relative to its parent's handle without following
// links, and follows symlinks and junctions itself from the reparse data of the handle it holds. A
// link swapped during the walk changes nothing already opened, and a link to a UNC or device path,
// or to another volume that is a network drive, is refused before anything opens it, so a link in
// the tree cannot make the open contact a network share.
func (w *workdir) open(path string) (*os.File, string, error) {
	h, err := walk(path)
	if err != nil {
		return nil, "", fmt.Errorf("open %s: %w", path, err)
	}
	f := os.NewFile(uintptr(h), path)
	canon, err := finalPath(h)
	if err != nil {
		_ = f.Close()
		return nil, "", fmt.Errorf("canonical path of %s: %w", path, err)
	}
	return f, canon, nil
}

// maxLinkHops bounds the links followed while resolving one path, so a link cycle ends.
const maxLinkHops = 63

// errNotFile is returned for a path that ends at a volume root or with `..`.
var errNotFile = errors.New("not a file")

// walk opens the absolute path for reading as open describes. The directories on the way stay open
// until the walk ends, so `..` returns to the handle it came from.
func walk(path string) (windows.Handle, error) {
	start := filepath.VolumeName(path)
	vol, names := splitVolume(path)
	root, err := openRoot(vol)
	if err != nil {
		return 0, err
	}
	dirs := []windows.Handle{root}
	defer func() { closeAll(dirs) }()
	for hops := 0; len(names) > 0; {
		name := names[0]
		names = names[1:]
		switch name {
		case ".":
			continue
		case "..":
			if len(dirs) > 1 {
				closeAll(dirs[len(dirs)-1:])
				dirs = dirs[:len(dirs)-1]
			}
			continue
		}
		parent := dirs[len(dirs)-1]
		last := len(names) == 0
		h, err := openAt(parent, name, last, windows.FILE_OPEN_REPARSE_POINT)
		if err != nil {
			return 0, err
		}
		tag, target, err := reparse(h)
		switch {
		case err != nil:
			_ = windows.CloseHandle(h)
			return 0, err
		case target != "":
			_ = windows.CloseHandle(h)
			hops++
			if hops > maxLinkHops {
				return 0, fmt.Errorf("%s: too many links", name)
			}
			var more []string
			if dirs, more, err = follow(dirs, target, start); err != nil {
				return 0, fmt.Errorf("%s: %w", name, err)
			}
			names = slices.Concat(more, names)
			continue
		case tag&nameSurrogate != 0:
			_ = windows.CloseHandle(h)
			return 0, fmt.Errorf("%s: unsupported link (reparse tag %#x)", name, tag)
		case tag != 0 && last:
			// a reparse point a filter serves, such as a cloud or deduplicated file, is read through it
			if h, err = reopen(parent, name, h); err != nil {
				return 0, err
			}
		}
		if last {
			return h, nil
		}
		dirs = append(dirs, h)
	}
	return 0, errNotFile
}

// follow moves the walk in dirs, which it closes from as needed, to where the link target leads,
// and returns the open directories and the names still to walk. A target on another volume starts
// from that volume's root, unless it is a UNC or device path, or a network drive other than start.
func follow(dirs []windows.Handle, target, start string) ([]windows.Handle, []string, error) {
	if remotePath(target) {
		return dirs, nil, errors.New("a link leads to a UNC or device path")
	}
	if t := stripExtendedPrefix(target); filepath.VolumeName(t) != "" {
		target = t // `C:\x`; a volume GUID path keeps its prefix
	}
	vol, names := splitVolume(target)
	switch {
	case vol != `\`:
		if !strings.EqualFold(filepath.VolumeName(target), start) && !localDrive(target) {
			return dirs, nil, errors.New("a link leads to a network drive or a volume that is not a local disk")
		}
		root, err := openRoot(vol)
		if err != nil {
			return dirs, nil, err
		}
		closeAll(dirs)
		return []windows.Handle{root}, names, nil
	case strings.HasPrefix(target, `\`):
		// rooted: from the root of the current volume
		closeAll(dirs[1:])
		return dirs[:1], names, nil
	default:
		return dirs, names, nil
	}
}

// openRoot opens the volume root vol (`C:\`, `\\?\Volume{...}\`, `\\srv\share\`).
func openRoot(vol string) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(vol)
	if err != nil {
		return 0, fmt.Errorf("volume %s: %w", vol, err)
	}
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0, fmt.Errorf("volume %s: %w", vol, err)
	}
	return h, nil
}

// openAt opens the single name in the directory parent with the NtCreateFile options given. Only
// the last name is opened for reading data; the directories on the way need only their attributes.
// No handle shares delete access, so a name held open cannot be renamed or replaced.
func openAt(parent windows.Handle, name string, last bool, options uint32) (windows.Handle, error) {
	us, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: parent, ObjectName: us, Attributes: windows.OBJ_CASE_INSENSITIVE}
	oa.Length = uint32(unsafe.Sizeof(oa))
	access := uint32(windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE)
	if last {
		access |= windows.GENERIC_READ
	}
	var h windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	if err := windows.NtCreateFile(&h, access, &oa, &iosb, nil, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		windows.FILE_OPEN, options|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0); err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return h, nil
}

// reopen opens name in parent again with its reparse point acting, and fails unless that is the file
// h holds, which pins the name while it is open. h is closed either way.
func reopen(parent windows.Handle, name string, h windows.Handle) (windows.Handle, error) {
	defer windows.CloseHandle(h)
	served, err := openAt(parent, name, true, 0)
	if err != nil {
		return 0, err
	}
	var a, b windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(h, &a) == nil && windows.GetFileInformationByHandle(served, &b) == nil &&
		a.VolumeSerialNumber == b.VolumeSerialNumber && a.FileIndexHigh == b.FileIndexHigh && a.FileIndexLow == b.FileIndexLow {
		return served, nil
	}
	_ = windows.CloseHandle(served)
	return 0, fmt.Errorf("%s: changed while opening", name)
}

// nameSurrogate marks the reparse tags of points that name another file, like symlinks and junctions.
const nameSurrogate = 0x20000000

// reparse returns the reparse tag of the open file h, zero when it is not a reparse point, and for a
// symlink or junction its target as parseReparse returns it.
func reparse(h windows.Handle) (uint32, string, error) {
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		return 0, "", fmt.Errorf("file information: %w", err)
	}
	if fi.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		return 0, "", nil
	}
	buf := make([]byte, windows.MAXIMUM_REPARSE_DATA_BUFFER_SIZE)
	var n uint32
	if err := windows.DeviceIoControl(h, windows.FSCTL_GET_REPARSE_POINT, nil, 0, &buf[0], uint32(len(buf)), &n, nil); err != nil {
		return 0, "", fmt.Errorf("reparse point: %w", err)
	}
	return parseReparse(buf[:n])
}

// symlinkRelative is SYMLINK_FLAG_RELATIVE.
const symlinkRelative = 1

// parseReparse decodes a REPARSE_DATA_BUFFER into its tag and, for a symlink or junction, the target
// it substitutes: a relative symlink's as stored, an absolute one's as a `\\?\` path. An absolute
// target outside the `\??\` namespace, such as an NT device path, is refused.
func parseReparse(b []byte) (uint32, string, error) {
	if len(b) < 8 {
		return 0, "", errors.New("short reparse point")
	}
	tag := binary.LittleEndian.Uint32(b)
	var header int
	switch tag {
	case windows.IO_REPARSE_TAG_SYMLINK:
		header = 20 // tag, data length, reserved, two name offset/length pairs, flags
	case windows.IO_REPARSE_TAG_MOUNT_POINT:
		header = 16
	default:
		return tag, "", nil
	}
	if len(b) < header {
		return 0, "", errors.New("short reparse point")
	}
	off, size := int(binary.LittleEndian.Uint16(b[8:])), int(binary.LittleEndian.Uint16(b[10:]))
	names := b[header:]
	if off%2 != 0 || size%2 != 0 || size == 0 || off+size > len(names) {
		return 0, "", errors.New("malformed reparse point")
	}
	name := make([]uint16, size/2)
	for i := range name {
		name[i] = binary.LittleEndian.Uint16(names[off+2*i:])
	}
	target := windows.UTF16ToString(name)
	if tag == windows.IO_REPARSE_TAG_SYMLINK && binary.LittleEndian.Uint32(b[16:])&symlinkRelative != 0 {
		return tag, target, nil
	}
	if rest, ok := strings.CutPrefix(target, `\??\`); ok {
		return tag, `\\?\` + rest, nil
	}
	return 0, "", fmt.Errorf("unsupported link target %s", target)
}

// localDrive reports whether the volume of the absolute path p is a local disk. A network drive,
// and a volume whose type is unknown or that has no root, are not.
func localDrive(p string) bool {
	root, err := windows.UTF16PtrFromString(filepath.VolumeName(p) + `\`)
	if err != nil {
		return false
	}
	switch windows.GetDriveType(root) {
	case windows.DRIVE_FIXED, windows.DRIVE_REMOVABLE, windows.DRIVE_CDROM, windows.DRIVE_RAMDISK:
		return true
	}
	return false
}

func closeAll(hs []windows.Handle) {
	for _, h := range hs {
		_ = windows.CloseHandle(h)
	}
}

// splitVolume splits the path p into its volume root and the names below it.
func splitVolume(p string) (string, []string) {
	vol := filepath.VolumeName(p)
	return vol + `\`, strings.FieldsFunc(p[len(vol):], func(r rune) bool { return r == '\\' || r == '/' })
}

// remotePath reports whether the link target p is a UNC (`\\srv\share`, `\\?\UNC\srv\share`) or
// device (`\\.\pipe\x`, `\\?\pipe\x`, `\??\COM1`) path. Behind an extended prefix only a drive
// letter (`\\?\C:\`) or a volume GUID (`\\?\Volume{...}\`) is local; any other name is a device.
func remotePath(p string) bool {
	for _, prefix := range []string{`\\?\`, `\??\`} {
		if rest, ok := strings.CutPrefix(p, prefix); ok {
			return !driveLetter(rest) && !strings.HasPrefix(strings.ToUpper(rest), `VOLUME{`)
		}
	}
	return strings.HasPrefix(p, `\\`)
}

// driveLetter reports whether p starts with a drive letter and a colon (`C:`).
func driveLetter(p string) bool {
	if len(p) < 2 || p[1] != ':' {
		return false
	}
	c := p[0] | 0x20
	return 'a' <= c && c <= 'z'
}

// stripExtendedPrefix turns `\\?\C:\x` into `C:\x` and `\\?\UNC\srv\share` into `\\srv\share`.
func stripExtendedPrefix(p string) string {
	if rest, ok := strings.CutPrefix(p, `\\?\UNC\`); ok {
		return `\\` + rest
	}
	return strings.TrimPrefix(p, `\\?\`)
}

// samePathKey folds case: Windows paths are case-insensitive.
func samePathKey(p string) string { return strings.ToLower(p) }
