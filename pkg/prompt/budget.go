package prompt

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"unicode/utf8"
)

// Shares of a request, in estimated tokens.
const (
	AnchorTokens    = 4_000 // the anchor repeated in every chunk request
	attachmentShare = 1_000 // the anchor's share for attachments
	MinStateTokens  = 2_000 // questions leaving less than this for the state are a config error
)

// ErrQuestionsOverBudget means the serialized questions leave too little room for the state.
var ErrQuestionsOverBudget = errors.New("questions over budget")

// Policy is how agrouter sizes routing requests: the estimator ratio and the allowance per request.
type Policy struct {
	BytesPerToken float64 // UTF-8 bytes estimated per token
	RequestTokens int     // the whole serialized input of one request: state plus its questions
}

// Tokens estimates the tokens of n UTF-8 bytes: n ÷ BytesPerToken, rounded up. It saturates at
// math.MaxInt and is never negative.
func (p Policy) Tokens(n int) int {
	if n <= 0 {
		return 0
	}
	return saturate(math.Ceil(float64(n) / p.BytesPerToken))
}

// Bytes is the UTF-8 bytes that tokens estimate to: tokens × BytesPerToken, rounded down. It
// saturates at math.MaxInt and is never negative.
func (p Policy) Bytes(tokens int) int {
	if tokens <= 0 {
		return 0
	}
	return saturate(math.Floor(float64(tokens) * p.BytesPerToken))
}

// saturate converts f to an int, clamping to [0, math.MaxInt]; NaN and +Inf saturate high.
func saturate(f float64) int {
	switch {
	case f <= 0:
		return 0
	case !(f < math.MaxInt): // 2^63 is the first float past MaxInt; also true for NaN and +Inf
		return math.MaxInt
	}
	return int(f)
}

// Questions are the serialized sizes, in bytes, of the questions that go beside the state.
type Questions struct {
	Route      int // the single request's route question, catalog included
	ChunkRoute int // a chunk request's route question, catalog included
	Relevance  int // a chunk request's relevance question
	Complexity int // a doc request's complexity question
	Evidence   int // a doc request's evidence question
}

// Budget is how many tokens the state may take in each kind of request, under its policy.
type Budget struct {
	Policy
	State int // the single request's state
	Chunk int // a chunk request's state: anchor, chunk envelope and chunk text
	Doc   int // a doc request's state: the docs whole, or one doc chunk in its envelope
}

// NewBudget derives the state budgets from the question sizes: each request's state gets
// RequestTokens less the request's own questions. The single state gets it less the route question;
// a chunk state less the chunk route and relevance questions; a doc state less the complexity and
// evidence questions. It returns ErrQuestionsOverBudget when the single state, a chunk state beside a
// maximal anchor, or a doc state would get less than MinStateTokens.
func NewBudget(p Policy, q Questions) (Budget, error) {
	b := Budget{
		Policy: p,
		State:  less(p.RequestTokens, p.Tokens(q.Route)),
		Chunk:  less(less(p.RequestTokens, p.Tokens(q.ChunkRoute)), p.Tokens(q.Relevance)),
		Doc:    less(less(p.RequestTokens, p.Tokens(q.Complexity)), p.Tokens(q.Evidence)),
	}
	var errs []error
	if b.State < MinStateTokens {
		errs = append(errs, fmt.Errorf("%w: request_tokens %d less the routing_policy question leaves %d tokens for the state, need %d",
			ErrQuestionsOverBudget, p.RequestTokens, b.State, MinStateTokens))
	}
	if left := b.Chunk - AnchorTokens; left < MinStateTokens {
		errs = append(errs, fmt.Errorf("%w: request_tokens %d less the routing_policy chunk questions leaves %d tokens beside the anchor in a chunk request, need %d",
			ErrQuestionsOverBudget, p.RequestTokens, left, MinStateTokens))
	}
	if b.Doc < MinStateTokens {
		errs = append(errs, fmt.Errorf("%w: request_tokens %d less the complexity_policy questions leaves %d tokens for the doc state, need %d",
			ErrQuestionsOverBudget, p.RequestTokens, b.Doc, MinStateTokens))
	}
	return b, errors.Join(errs...)
}

// less is tokens minus n, a question estimate that is never negative, saturating at math.MinInt
// instead of wrapping around to a positive.
func less(tokens, n int) int {
	if tokens < math.MinInt+n {
		return math.MinInt
	}
	return tokens - n
}

// indexDigits is the decimal width reserved for a chunk's index and for its count: the widest int,
// so any number of chunks fits the envelope.
var indexDigits = len(strconv.Itoa(math.MaxInt))

// chunkRoom is the escaped chunk text, in bytes, that fits in a chunk state beside an anchor of
// anchorLen serialized bytes.
func chunkRoom(b Budget, anchorLen int) int {
	// {"anchor":<anchor>,"chunk":{"field":"prompt","index":N,"of":N,"text":"<text>"}}
	envelope := len(`{"anchor":,"chunk":{"field":"prompt","index":,"of":,"text":""}}`) + 2*indexDigits
	return b.Bytes(b.Chunk) - anchorLen - envelope
}

// docRoom is the escaped doc text, in bytes, that fits in a doc chunk state of tokens.
func docRoom(p Policy, tokens int) int {
	// {"doc":{"index":N,"of":N,"text":"<text>"}}
	envelope := len(`{"doc":{"index":,"of":,"text":""}}`) + 2*indexDigits
	return p.Bytes(tokens) - envelope
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
