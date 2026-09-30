package prompt

import (
	"bytes"
	"net/http"
	"strings"
	"unicode/utf8"
)

// SniffLen is the prefix length used to detect binary input by its signature.
const SniffLen = 8 << 10

// TypeUnknown is the media type of binary input without a known signature.
const TypeUnknown = "unknown"

// Detect reports whether data is binary and, if so, its media type.
// A known signature (http.DetectContentType) names the type first; otherwise NUL bytes or invalid
// UTF-8 anywhere in data mean binary of type TypeUnknown. When complete is false, data is a prefix of
// a longer input, so a character cut at its end is still text.
func Detect(data []byte, complete bool) (binary bool, mediaType string) {
	if t := signature(data); t != "" {
		return true, t
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return true, TypeUnknown
	}
	if !complete {
		data = trimCutRune(data)
	}
	if !utf8.Valid(data) {
		return true, TypeUnknown
	}
	return false, ""
}

// signature returns the media type named by data's magic bytes, or "" for text and unrecognized data.
func signature(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	t, _, _ := strings.Cut(http.DetectContentType(data), ";")
	if strings.HasPrefix(t, "text/") || t == "application/octet-stream" {
		return ""
	}
	return t
}

// trimCutRune drops an incomplete but so far valid UTF-8 sequence at the end of data.
func trimCutRune(data []byte) []byte {
	for i := 1; i < utf8.UTFMax && i <= len(data); i++ {
		b := data[len(data)-i]
		if utf8.RuneStart(b) {
			if !utf8.FullRune(data[len(data)-i:]) {
				return data[:len(data)-i]
			}
			return data
		}
	}
	return data
}
