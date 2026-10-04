package prompt

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"unicode/utf8"
)

// Jev's limits and agrouter's shares of them, in estimated tokens.
const (
	bytesPerToken = 3

	stateLimit      = 30_000 // state plus the longest question (Jev's 32k, less a 2k margin)
	totalLimit      = 64_000 // state plus all questions
	AnchorTokens    = 4_000  // the anchor repeated in every chunk request
	attachmentShare = 1_000  // the anchor's share for attachments
	MinStateTokens  = 2_000  // questions leaving less than this for the state are a config error
)

// ErrQuestionsOverBudget means the serialized questions leave too little room for the state.
var ErrQuestionsOverBudget = errors.New("questions over budget")

// Tokens estimates the tokens of n UTF-8 bytes: n ÷ 3, rounded up.
func Tokens(n int) int {
	return (n + bytesPerToken - 1) / bytesPerToken
}

// Questions are the serialized sizes, in bytes, of the questions that go beside the state.
type Questions struct {
	Route      int // the single request's route question, catalog included
	ChunkRoute int // a chunk request's route question, catalog included
	Relevance  int // a chunk request's relevance question
}

// Budget is how many tokens the state may take in each kind of request.
type Budget struct {
	State int // the single request's state
	Chunk int // a chunk request's state: anchor, chunk envelope and chunk text
}

// NewBudget derives the state budgets from the question sizes: the single state gets 30k minus the
// route question; a chunk state gets 30k minus the longer chunk question, and no more than leaves
// the state plus both questions within 64k. It returns ErrQuestionsOverBudget when the single state,
// or a chunk state beside a maximal anchor, would get less than MinStateTokens.
func NewBudget(q Questions) (Budget, error) {
	b := Budget{
		State: stateLimit - Tokens(q.Route),
		Chunk: min(stateLimit-Tokens(max(q.ChunkRoute, q.Relevance)), totalLimit-Tokens(q.ChunkRoute)-Tokens(q.Relevance)),
	}
	var errs []error
	if b.State < MinStateTokens {
		errs = append(errs, fmt.Errorf("%w: question leaves %d tokens for the state, need %d", ErrQuestionsOverBudget, b.State, MinStateTokens))
	}
	if left := b.Chunk - AnchorTokens; left < MinStateTokens {
		errs = append(errs, fmt.Errorf("%w: chunk_question and relevance leave %d tokens beside the anchor, need %d", ErrQuestionsOverBudget, left, MinStateTokens))
	}
	return b, errors.Join(errs...)
}

// indexDigits is the decimal width reserved for a chunk's index and for its count: the widest int,
// so any number of chunks, renumbered after any re-split, fits the envelope.
var indexDigits = len(strconv.Itoa(math.MaxInt))

// chunkRoom is the escaped chunk text, in bytes, that fits in a chunk state beside an anchor of
// anchorLen serialized bytes.
func chunkRoom(b Budget, anchorLen int) int {
	// {"anchor":<anchor>,"chunk":{"field":"prompt","index":N,"of":N,"text":"<text>"}}
	envelope := len(`{"anchor":,"chunk":{"field":"prompt","index":,"of":,"text":""}}`) + 2*indexDigits
	return b.Chunk*bytesPerToken - anchorLen - envelope
}

// jsonLen is the length of s once encoded as a JSON string by encoding/json, without the quotes.
func jsonLen(s string) int {
	n := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		n += escapedLen(r, size)
		i += size
	}
	return n
}

// escapedLen is the encoded length of one rune taking size bytes of input, following encoding/json
// with HTML escaping on. An invalid byte counts as the escaped `\ufffd` that encoding/json v1 writes;
// the v2-based encoder writes the 3-byte rune, so the estimate is an upper bound for both.
func escapedLen(r rune, size int) int {
	switch {
	case r == utf8.RuneError && size == 1:
		return len(`\ufffd`)
	case r == '"' || r == '\\' || r == '\n' || r == '\r' || r == '\t' || r == '\b' || r == '\f':
		return 2
	case r < 0x20 || r == '<' || r == '>' || r == '&' || r == '\u2028' || r == '\u2029':
		return len(`\u0000`)
	default:
		return size
	}
}
