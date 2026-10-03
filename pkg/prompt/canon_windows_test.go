//go:build windows

package prompt

import (
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestRemotePath(t *testing.T) {
	tests := []struct {
		p    string
		want bool
	}{
		{p: `C:\x`, want: false},
		{p: `x\y`, want: false},
		{p: `\x`, want: false},
		{p: `\\?\C:\x`, want: false},
		{p: `\??\C:\x`, want: false},
		{p: `\\?\Volume{0a1b}\x`, want: false},
		{p: `\\srv\share\x`, want: true},
		{p: `\\?\UNC\srv\share`, want: true},
		{p: `\??\unc\srv\share`, want: true},
		{p: `\\.\pipe\x`, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.p, func(t *testing.T) {
			assert.Equal(t, tc.want, remotePath(tc.p))
		})
	}
}

// openText opens path through a workdir on cwd and returns its content and canonical path.
func openText(t *testing.T, cwd, path string) (string, string, error) {
	t.Helper()
	wd, err := openWorkdir(cwd)
	require.NoError(t, err)
	f, canon, err := wd.open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	require.NoError(t, err)
	return string(b), canon, nil
}

func TestWorkdirOpen_Junctions(t *testing.T) {
	cwd := tree(t, map[string]string{"cwd2/secret.txt": "sibling", "cwd/dir/in.txt": "inside"})
	tmp := filepath.Dir(cwd)
	link(t, filepath.Join(tmp, "cwd2"), filepath.Join(cwd, "out"))
	link(t, filepath.Join(cwd, "dir"), filepath.Join(cwd, "in"))
	link(t, filepath.Join(cwd, "in"), filepath.Join(cwd, "in2"))
	wd, err := openWorkdir(cwd)
	require.NoError(t, err)

	tests := []struct {
		path, want, canon string
	}{
		{path: filepath.Join(cwd, "dir", "in.txt"), want: "inside", canon: filepath.Join(wd.root, "dir", "in.txt")},
		{path: filepath.Join(cwd, "in", "in.txt"), want: "inside", canon: filepath.Join(wd.root, "dir", "in.txt")},
		{path: filepath.Join(cwd, "in2", "in.txt"), want: "inside", canon: filepath.Join(wd.root, "dir", "in.txt")},
		{path: filepath.Join(cwd, "IN", "In.TXT"), want: "inside", canon: filepath.Join(wd.root, "dir", "in.txt")},
		{path: filepath.Join(cwd, "out", "secret.txt"), want: "sibling", canon: filepath.Join(filepath.Dir(wd.root), "cwd2", "secret.txt")},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			got, canon, oerr := openText(t, cwd, tc.path)
			require.NoError(t, oerr)
			assert.Equal(t, tc.want, got)
			assert.True(t, strings.EqualFold(tc.canon, canon), canon)
		})
	}

	for _, p := range []string{filepath.Join(cwd, "missing", "x.txt"), filepath.Join(cwd, "dir", "nope.txt"), filepath.VolumeName(cwd) + `\`} {
		_, _, oerr := openText(t, cwd, p)
		require.Error(t, oerr, p)
	}
	f, _, err := wd.open(filepath.Join(cwd, "in"))
	require.NoError(t, err, "a directory opens; the caller rejects it as not regular")
	defer f.Close()
	fi, err := f.Stat()
	require.NoError(t, err)
	assert.True(t, fi.IsDir())
}

func TestWorkdirOpen_Symlinks(t *testing.T) {
	cwd := tree(t, map[string]string{"cwd/dir/in.txt": "inside", "cwd2/out.txt": "outside"})
	if err := os.Symlink(`dir\in.txt`, filepath.Join(cwd, "rel.txt")); err != nil {
		t.Skipf("symlinks need developer mode or elevation: %v", err)
	}
	require.NoError(t, os.Symlink(`..\cwd2\out.txt`, filepath.Join(cwd, "dir", "up.txt")))
	require.NoError(t, os.Symlink(`..\..\cwd2\out.txt`, filepath.Join(cwd, "dir", "up2.txt")))
	require.NoError(t, os.Symlink(filepath.Join(cwd, "dir"), filepath.Join(cwd, "abs")))
	require.NoError(t, os.Symlink(cwd[len(filepath.VolumeName(cwd)):]+`\dir\in.txt`, filepath.Join(cwd, "rooted.txt")))
	require.NoError(t, os.Symlink(`\\127.0.0.1\share\x.txt`, filepath.Join(cwd, "unc.txt")))
	require.NoError(t, os.Symlink(`\\127.0.0.1\share`, filepath.Join(cwd, "uncdir")))
	require.NoError(t, os.Symlink(filepath.Join(cwd, "uncdir"), filepath.Join(cwd, "via")))
	require.NoError(t, os.Symlink(filepath.Join(cwd, "loop2"), filepath.Join(cwd, "loop1")))
	require.NoError(t, os.Symlink(filepath.Join(cwd, "loop1"), filepath.Join(cwd, "loop2")))

	for path, want := range map[string]string{
		"rel.txt":        "inside",
		`abs\in.txt`:     "inside",
		"rooted.txt":     "inside",
		`dir\up2.txt`:    "outside",
		`abs\up2.txt`:    "outside",
	} {
		got, _, err := openText(t, cwd, filepath.Join(cwd, path))
		require.NoError(t, err, path)
		assert.Equal(t, want, got, path)
	}
	_, _, err := openText(t, cwd, filepath.Join(cwd, "dir", "up.txt"))
	require.Error(t, err, `..\cwd2 from dir is cwd\cwd2, which does not exist`)

	for path, msg := range map[string]string{
		"unc.txt":            "UNC or device path",
		`uncdir\x.txt`:       "UNC or device path",
		`via\x.txt`:          "UNC or device path",
		"loop1":              "too many links",
	} {
		_, _, err := openText(t, cwd, filepath.Join(cwd, path))
		require.Error(t, err, path)
		assert.Contains(t, err.Error(), msg, path)
	}

	res := mention(t, t.Context(), cwd, "unc.txt via/x.txt rel.txt", testLimit)
	require.NoError(t, res.Undecidable)
	assert.Equal(t, []string{"inside"}, res.Files)
}

// reparseBuffer encodes a REPARSE_DATA_BUFFER for tag with the substitute name sub.
func reparseBuffer(tag uint32, sub string, flags uint32) []byte {
	name := utf16.Encode([]rune(sub))
	b := binary.LittleEndian.AppendUint32(nil, tag)
	b = binary.LittleEndian.AppendUint16(b, 0)
	b = binary.LittleEndian.AppendUint16(b, 0)
	b = binary.LittleEndian.AppendUint16(b, 0)                     // substitute offset
	b = binary.LittleEndian.AppendUint16(b, uint16(2*len(name)))
	b = binary.LittleEndian.AppendUint16(b, uint16(2*len(name))) // print name follows
	b = binary.LittleEndian.AppendUint16(b, 0)
	if tag == windows.IO_REPARSE_TAG_SYMLINK {
		b = binary.LittleEndian.AppendUint32(b, flags)
	}
	for _, c := range name {
		b = binary.LittleEndian.AppendUint16(b, c)
	}
	return b
}

func TestParseReparse(t *testing.T) {
	tests := []struct {
		name    string
		b       []byte
		tag     uint32
		target  string
		wantErr bool
	}{
		{name: "relative symlink", b: reparseBuffer(windows.IO_REPARSE_TAG_SYMLINK, `..\x`, symlinkRelative),
			tag: windows.IO_REPARSE_TAG_SYMLINK, target: `..\x`},
		{name: "absolute symlink", b: reparseBuffer(windows.IO_REPARSE_TAG_SYMLINK, `\??\C:\x`, 0),
			tag: windows.IO_REPARSE_TAG_SYMLINK, target: `\\?\C:\x`},
		{name: "unc symlink", b: reparseBuffer(windows.IO_REPARSE_TAG_SYMLINK, `\??\UNC\srv\share`, 0),
			tag: windows.IO_REPARSE_TAG_SYMLINK, target: `\\?\UNC\srv\share`},
		{name: "junction", b: reparseBuffer(windows.IO_REPARSE_TAG_MOUNT_POINT, `\??\Volume{0a1b}\`, 0),
			tag: windows.IO_REPARSE_TAG_MOUNT_POINT, target: `\\?\Volume{0a1b}\`},
		{name: "nt device path", b: reparseBuffer(windows.IO_REPARSE_TAG_SYMLINK, `\Device\Mup\srv\share`, 0), wantErr: true},
		{name: "other tag", b: reparseBuffer(0x9000001A, "", 0), tag: 0x9000001A},
		{name: "short", b: []byte{1, 2, 3}, wantErr: true},
		{name: "short symlink", b: reparseBuffer(windows.IO_REPARSE_TAG_SYMLINK, "", 0)[:12], wantErr: true},
		{name: "empty name", b: reparseBuffer(windows.IO_REPARSE_TAG_MOUNT_POINT, "", 0), wantErr: true},
		{name: "name past the end", b: reparseBuffer(windows.IO_REPARSE_TAG_MOUNT_POINT, `\??\C:\x`, 0)[:20], wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tag, target, err := parseReparse(tc.b)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.tag, tag)
			assert.Equal(t, tc.target, target)
		})
	}
}
