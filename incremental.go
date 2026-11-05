package participle

import (
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/alecthomas/participle/v2/lexer"
)

// DiagnosticCategory identifies the processing stage that produced a Diagnostic.
type DiagnosticCategory string

const (
	// DiagnosticLex is a diagnostic produced while tokenising the input.
	DiagnosticLex DiagnosticCategory = "lex"
	// DiagnosticParse is a diagnostic produced while matching the grammar.
	DiagnosticParse DiagnosticCategory = "parse"
)

// A Diagnostic is a single lexing or parsing problem at a source position.
//
// Diagnostics never replace Go errors: they are reported through SessionResult
// so that editor-style callers can render problems alongside a partial AST
// without treating them as fatal.
type Diagnostic struct {
	// Category is the stage that produced the diagnostic.
	Category DiagnosticCategory
	// Pos is the position where the problem starts.
	Pos lexer.Position
	// End is the first position after the problematic span. It equals Pos when
	// no more precise span is available.
	End lexer.Position
	// Message is the unadorned diagnostic message.
	Message string
}

// String renders the diagnostic in the same "[<filename>:][<line>:<col>:] <msg>"
// form used by participle errors.
func (d Diagnostic) String() string {
	msg := ""
	if d.Pos.Filename != "" {
		msg += d.Pos.Filename + ":"
	}
	if d.Pos.Line != 0 || d.Pos.Column != 0 {
		msg += strconv.Itoa(d.Pos.Line) + ":" + strconv.Itoa(d.Pos.Column) + ":"
	}
	if msg != "" {
		return msg + " " + d.Message
	}
	return d.Message
}

// Fallback reason codes reported by an incremental Session.
const (
	// FallbackInitial is reported for the first version submitted to a session.
	FallbackInitial = "initial-parse"
	// FallbackFilenameChanged is reported when a new version uses a different filename.
	FallbackFilenameChanged = "filename-changed"
	// FallbackLexicalError is reported when the new input contains a lexical
	// error. The lexer is restarted at the next safe boundary and the whole
	// input is reprocessed; the offending position is carried on the reason.
	FallbackLexicalError = "lexical-error"
	// FallbackDirtyState is reported when the previous version itself could
	// only be lexed with error recovery, leaving no trustworthy state to reuse.
	FallbackDirtyState = "previous-lexical-error"
	// FallbackNoSafeBoundary is reported when no token boundary before the
	// change could be proven to re-lex into the same token stream as a fresh
	// lex of the complete input.
	FallbackNoSafeBoundary = "no-safe-boundary"
)

// FallbackReason explains why an incremental update degraded to a complete
// re-lex and re-parse.
type FallbackReason struct {
	// Code is one of the Fallback* constants.
	Code string
	// Message is a human readable explanation.
	Message string
	// Pos is the position that motivated the fallback, when known.
	Pos lexer.Position
}

func (f *FallbackReason) Error() string {
	if f == nil {
		return ""
	}
	prefix := f.Code + ": "
	if f.Pos.Filename != "" || f.Pos.Line != 0 || f.Pos.Column != 0 {
		return f.Pos.String() + ": " + prefix + f.Message
	}
	return prefix + f.Message
}

// NodeID is a stable identifier for an AST node within a Session.
//
// A node keeps the same NodeID across updates as long as it starts outside of
// the edited region. Nodes introduced by an edit receive fresh IDs. IDs are not
// meaningful across different sessions or grammars.
type NodeID uint64

// NodeInfo describes an AST node independently of the concrete grammar struct:
// its production name, source range and stable identity.
//
// NodeInfo slices are ordered by source position.
type NodeInfo struct {
	ID         NodeID
	Production string
	Pos        lexer.Position
	End        lexer.Position
}

// Edit is a single byte-offset replacement in the previous source text.
type Edit struct {
	// Start is the inclusive byte offset where the replacement begins.
	Start int
	// End is the exclusive byte offset where the replacement ends.
	End int
	// Text replaces src[Start:End].
	Text string
}

// SessionResult is the outcome of submitting one source version to a Session.
type SessionResult[G any] struct {
	// Version counts distinct submitted texts, starting at 1. Resubmitting an
	// identical text does not increase it.
	Version int
	// AST is the best available parse tree. It is non-nil even when
	// Diagnostics is non-empty; fields captured before the first problem are
	// populated.
	AST *G
	// Diagnostics is the complete, position-sorted set of lexing and parsing
	// problems found in this version.
	Diagnostics []Diagnostic
	// Nodes describes the productions captured in AST with stable identities.
	Nodes []NodeInfo
	// Incremental is true when lexer state from the previous version was
	// safely reused.
	Incremental bool
	// FullReparse is true when the session could not safely reuse prior state
	// and instead lexed and parsed the complete input.
	FullReparse bool
	// Fallback describes why a full re-parse was performed. It is nil when
	// incremental state was reused.
	Fallback *FallbackReason
}

// Session is a stateful, reusable incremental parsing session for grammar G.
//
// Submit successive versions of a source text with UpdateString, or local
// changes with ApplyEdits. Each submission returns a best-effort AST, stable
// node identities and position-sorted diagnostics. When input is valid the
// result converges to exactly what Parser.ParseString would return.
//
// A Session is not safe for concurrent use.
type Session[G any] struct {
	p *Parser[G]

	filename string
	src      string
	hash     uint64
	version  int

	// tokens from the most recent lex (always a complete, fresh tokenisation,
	// including the EOF token).
	tokens []lexer.Token
	// cleanTokens is true when tokens required no lexical error recovery.
	cleanTokens bool

	// persistent node identities keyed by production and start offset in the
	// current src. Entries survive versions where a node is temporarily hidden
	// behind a syntax error.
	nodeIDs map[nodeIdentity]NodeID
	lastID  NodeID

	last *SessionResult[G]
}

// BeginSession starts a new incremental parsing session.
func (p *Parser[G]) BeginSession() *Session[G] {
	return &Session[G]{p: p}
}

// LastResult returns the most recent SessionResult, or nil before the first
// successful submission.
func (s *Session[G]) LastResult() *SessionResult[G] { return s.last }

// Version returns the current source version counter.
func (s *Session[G]) Version() int { return s.version }

// UpdateString submits a complete source text. The session derives the changed
// region by comparing the new text with the previous version, so node identity
// is preserved for nodes outside of the differing span.
func (s *Session[G]) UpdateString(filename, src string) (*SessionResult[G], error) {
	return s.submit(filename, src, nil)
}

// ApplyEdits submits a new source version produced by applying one or more
// non-overlapping, offset-ordered edits to the previous version.
func (s *Session[G]) ApplyEdits(filename string, edits ...Edit) (*SessionResult[G], error) {
	return s.submit(filename, s.src, edits)
}

func (s *Session[G]) submit(filename, src string, edits []Edit) (*SessionResult[G], error) {
	if len(edits) > 0 {
		next, err := applyEdits(s.src, edits)
		if err != nil {
			return nil, err
		}
		src = next
	}

	hash := sourceHash(filename, src)
	if s.last != nil && hash == s.hash && filename == s.filename {
		// Identical version: converge on the cached result without recomputing.
		return s.last, nil
	}

	span, haveSpan := diffSpan(s.src, src)
	mapNodes := haveSpan && filename == s.filename

	fresh, lexDiags, clean := lexFull(s.p.lex, filename, src)

	fallback := s.chooseFallback(filename, clean, lexDiags)
	tokens := fresh
	reused := false
	if fallback == nil {
		if spliced := s.tryReuseTokens(filename, s.src, src, span, fresh); spliced != nil {
			reused = true
			tokens = spliced
		} else {
			fallback = &FallbackReason{
				Code:    FallbackNoSafeBoundary,
				Message: "no token boundary before the change could be proven to re-lex consistently with a full lex",
				Pos:     positionAt(filename, s.src, span.oldStart),
			}
		}
	}

	ast, parseDiags, events, parseTokens := s.recoverParse(tokens)

	diags := append(append([]Diagnostic{}, lexDiags...), parseDiags...)
	sortDiagnostics(diags)

	nodes := s.assignNodes(events, parseTokens, span, mapNodes)

	s.version++
	res := &SessionResult[G]{
		Version:     s.version,
		AST:         ast,
		Diagnostics: diags,
		Nodes:       nodes,
		Incremental: reused,
		FullReparse: !reused,
		Fallback:    fallback,
	}

	s.filename = filename
	s.src = src
	s.hash = hash
	// Store the fresh tokenisation; when lexical recovery was involved it is
	// flagged dirty and will not be reused by the next submission.
	s.tokens = fresh
	s.cleanTokens = clean
	s.last = res
	return res, nil
}

func (s *Session[G]) chooseFallback(filename string, clean bool, lexDiags []Diagnostic) *FallbackReason {
	switch {
	case s.tokens == nil:
		return &FallbackReason{Code: FallbackInitial, Message: "first version submitted to the session; no prior state to reuse"}
	case filename != s.filename:
		return &FallbackReason{Code: FallbackFilenameChanged, Message: "filename changed; positional state cannot be reused"}
	case !s.cleanTokens:
		return &FallbackReason{Code: FallbackDirtyState, Message: "previous version contained lexical errors; its lexer state is not trustworthy"}
	case !clean:
		d := lexDiags[0]
		return &FallbackReason{Code: FallbackLexicalError, Pos: d.Pos, Message: "input could not be lexed at " + d.Pos.String() + "; restarted the lexer from the next safe boundary (" + d.Message + ")"}
	default:
		return nil
	}
}

// --- incremental lexing -----------------------------------------------------

// editSpan describes a single replacement between two source versions.
type editSpan struct {
	oldStart int
	oldEnd   int
	newEnd   int
}

// tryReuseTokens reconstructs the new token stream by re-lexing from the
// nearest safe token boundary before the change and reusing the unaffected
// prefix and suffix. A candidate boundary is accepted only when the complete
// spliced stream is byte-for-byte (type, value and position) identical to a
// fresh lex of the whole input; otherwise nil is returned.
func (s *Session[G]) tryReuseTokens(filename, oldSrc, newSrc string, span editSpan, fresh []lexer.Token) []lexer.Token {
	old := s.tokens
	// Candidate boundaries, nearest first, start of input last.
	candidates := make([]int, 0, 32)
	for k := len(old) - 1; k >= 0; k-- {
		if old[k].Pos.Offset <= span.oldStart {
			candidates = append(candidates, k)
			if old[k].Pos.Offset == 0 {
				break
			}
		}
	}
	if len(candidates) > maxReuseCandidates {
		candidates = candidates[:maxReuseCandidates]
	}

	endOld := positionAt(filename, oldSrc, span.oldEnd)
	endNew := positionAt(filename, newSrc, span.newEnd)
	lineShift := endNew.Line - endOld.Line
	colShift := endNew.Column - endOld.Column
	byteShift := span.newEnd - span.oldEnd

	for _, k := range candidates {
		boundary := old[k].Pos.Offset
		base := old[k].Pos

		lx, err := lexSubstring(s.p.lex, filename, newSrc[boundary:])
		if err != nil {
			continue
		}
		relexed, ok := consumeAllTokens(lx)
		if !ok {
			continue
		}

		// First re-lexed token at or past the end of the edit, and first old
		// token at or past the end of the replaced span.
		q := 0
		for q < len(relexed) && boundary+relexed[q].Pos.Offset < span.newEnd {
			q++
		}
		k2 := 0
		for k2 < len(old) && old[k2].Pos.Offset < span.oldEnd {
			k2++
		}
		if q >= len(relexed) || k2 >= len(old) {
			continue
		}
		join := relexed[q]
		join.Pos = base.Add(join.Pos)
		if !sameTokenKind(join, old[k2]) {
			continue
		}

		spliced := make([]lexer.Token, 0, len(old)+len(relexed)+1)
		spliced = append(spliced, old[:k]...)
		for i := 0; i < q; i++ {
			t := relexed[i]
			t.Pos = base.Add(t.Pos)
			spliced = append(spliced, t)
		}
		for i := k2; i < len(old); i++ {
			t := old[i]
			t.Pos = shiftPosition(t.Pos, endOld, lineShift, colShift, byteShift)
			spliced = append(spliced, t)
		}

		if tokensEqual(spliced, fresh) {
			return spliced
		}
	}
	return nil
}

const maxReuseCandidates = 256

func sameTokenKind(a, b lexer.Token) bool {
	return a.Type == b.Type && a.Value == b.Value
}

// shiftPosition moves a suffix token's position across an edit. Tokens that
// shared a line with the end of the replaced span also receive the column
// shift; tokens on later lines keep their column.
func shiftPosition(p lexer.Position, endOld lexer.Position, lineShift, colShift, byteShift int) lexer.Position {
	p.Offset += byteShift
	if p.Line == endOld.Line {
		p.Column += colShift
	}
	p.Line += lineShift
	return p
}

func tokensEqual(a, b []lexer.Token) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Type != b[i].Type || a[i].Value != b[i].Value || a[i].Pos != b[i].Pos {
			return false
		}
	}
	return true
}

// lexFull tokenises the complete input. On lexical error it records a
// diagnostic, skips the offending rune and restarts tokenisation from the root
// lexer state, so that input following the error still contributes to the
// partial AST.
func lexFull(def lexer.Definition, filename, src string) (tokens []lexer.Token, diags []Diagnostic, clean bool) {
	tokens = make([]lexer.Token, 0, 64)
	base := lexer.Position{Filename: filename, Line: 1, Column: 1}
	off := 0
	for {
		lx, err := lexSubstring(def, filename, src[off:])
		if err != nil {
			diags = append(diags, Diagnostic{Category: DiagnosticLex, Pos: base, End: base, Message: err.Error()})
			break
		}
		seg, derr := consumeUntilError(lx)
		for _, t := range seg.tokens {
			t.Pos = base.Add(t.Pos)
			tokens = append(tokens, t)
		}
		if derr == nil {
			eof := seg.eof
			eof.Pos = base.Add(eof.Pos)
			tokens = append(tokens, eof)
			return tokens, diags, len(diags) == 0
		}

		absPos := base.Add(derr.Position())
		end := absPos
		if absPos.Offset < len(src) {
			_, size := utf8.DecodeRuneInString(src[absPos.Offset:])
			end.Advance(src[absPos.Offset : absPos.Offset+size])
		}
		diags = append(diags, Diagnostic{Category: DiagnosticLex, Pos: absPos, End: end, Message: derr.Message()})

		if absPos.Offset >= len(src) {
			tokens = append(tokens, lexer.EOFToken(absPos))
			return tokens, diags, false
		}
		_, size := utf8.DecodeRuneInString(src[absPos.Offset:])
		base = end
		off = absPos.Offset + size
	}
	// Construction error fallback: still return an EOF so parsing can proceed.
	tokens = append(tokens, lexer.EOFToken(positionAt(filename, src, len(src))))
	return tokens, diags, false
}

type lexSegment struct {
	tokens []lexer.Token
	eof    lexer.Token
}

type positionalMessage interface {
	Position() lexer.Position
	Message() string
}

func consumeUntilError(lx lexer.Lexer) (lexSegment, positionalMessage) {
	var tokens []lexer.Token
	for {
		t, err := lx.Next()
		if err != nil {
			if pm, ok := err.(positionalMessage); ok {
				return lexSegment{tokens: tokens}, pm
			}
			return lexSegment{tokens: tokens}, plainMessage{msg: err.Error()}
		}
		if t.EOF() {
			return lexSegment{tokens: tokens, eof: t}, nil
		}
		tokens = append(tokens, t)
	}
}

func consumeAllTokens(lx lexer.Lexer) ([]lexer.Token, bool) {
	var tokens []lexer.Token
	for {
		t, err := lx.Next()
		if err != nil {
			return nil, false
		}
		tokens = append(tokens, t)
		if t.EOF() {
			return tokens, true
		}
	}
}

type plainMessage struct{ msg string }

func (p plainMessage) Position() lexer.Position { return lexer.Position{} }
func (p plainMessage) Message() string          { return p.msg }

func lexSubstring(def lexer.Definition, filename, s string) (lexer.Lexer, error) {
	if sl, ok := def.(lexer.StringDefinition); ok {
		return sl.LexString(filename, s)
	}
	return def.Lex(filename, strings.NewReader(s))
}

// --- parse error recovery ---------------------------------------------------

// sliceLexer replays a fixed token slice.
type sliceLexer struct {
	tokens []lexer.Token
	i      int
}

func (s *sliceLexer) Next() (lexer.Token, error) {
	t := s.tokens[s.i]
	if s.i < len(s.tokens)-1 {
		s.i++
	}
	return t, nil
}

// recoverParse parses the token stream, collecting diagnostics instead of
// stopping at the first syntax error. On error the offending span is discarded
// up to the first token on a following line (a safe resynchronisation point
// for line oriented configuration/DSL input) and parsing restarts; the
// loop always terminates because each iteration removes at least one token.
// The returned token slice is the one the final parse ran over.
func (s *Session[G]) recoverParse(tokens []lexer.Token) (*G, []Diagnostic, []nodeEvent, []lexer.Token) {
	var (
		diags    []Diagnostic
		events   []nodeEvent
		ast      *G
		attempt  = tokens
		seen     = map[string]bool{}
		maxIters = len(tokens) + 1
	)
	for iter := 0; ; iter++ {
		peeker, err := lexer.Upgrade(&sliceLexer{tokens: attempt}, s.p.getElidedTypes()...)
		if err != nil {
			// sliceLexer never errors; keep this purely defensive.
			diags = append(diags, Diagnostic{Category: DiagnosticParse, Message: err.Error()})
			return ast, diags, events, attempt
		}
		var captured []nodeEvent
		v, perr := s.p.ParseFromLexer(peeker, func(pc *parseContext) { pc.eventSink = &captured })
		events = captured
		ast = v
		if perr == nil {
			return ast, diags, events, attempt
		}

		pos, msg := errorPosAndMessage(perr)
		diag := Diagnostic{Category: DiagnosticParse, Pos: pos, End: pos, Message: msg}
		if idx := tokenIndexAt(attempt, pos); idx >= 0 {
			diag.End = tokenEnd(attempt[idx])
		}
		key := string(diag.Category) + "@" + strconv.Itoa(diag.Pos.Offset) + ":" + diag.Message
		if !seen[key] {
			seen[key] = true
			diags = append(diags, diag)
		}

		idx := tokenIndexAt(attempt, pos)
		if idx < 0 || idx >= len(attempt)-1 {
			// Error at EOF or outside the token stream: keep the partial AST.
			return ast, diags, events, attempt
		}

		// Resynchronise at the first token on a strictly later line. If no
		// such token exists, dropping the single offending token still
		// guarantees progress.
		syncEnd := idx + 1
		for syncEnd < len(attempt)-1 && attempt[syncEnd].Pos.Line <= attempt[idx].Pos.Line {
			syncEnd++
		}
		if syncEnd == len(attempt)-1 && attempt[syncEnd].Pos.Line <= attempt[idx].Pos.Line {
			syncEnd = idx + 1
		}

		next := make([]lexer.Token, 0, len(attempt)-(syncEnd-idx))
		next = append(next, attempt[:idx]...)
		next = append(next, attempt[syncEnd:]...)
		attempt = next

		if iter >= maxIters {
			return ast, diags, events, attempt
		}
	}
}

func tokenIndexAt(tokens []lexer.Token, pos lexer.Position) int {
	fallback := -1
	for i := range tokens {
		if tokens[i].Pos.Offset == pos.Offset {
			return i
		}
		if fallback < 0 && tokens[i].Pos.Offset > pos.Offset {
			fallback = i
		}
	}
	return fallback
}

func tokenEnd(t lexer.Token) lexer.Position {
	if t.EOF() {
		return t.Pos
	}
	end := t.Pos
	end.Advance(t.Value)
	return end
}

func errorPosAndMessage(err error) (lexer.Position, string) {
	pos := lexer.Position{}
	msg := err.Error()
	if pm, ok := err.(positionalMessage); ok {
		pos = pm.Position()
		msg = pm.Message()
	}
	return pos, msg
}

func sortDiagnostics(diags []Diagnostic) {
	sort.SliceStable(diags, func(i, j int) bool {
		a, b := diags[i].Pos, diags[j].Pos
		if a.Offset != b.Offset {
			return a.Offset < b.Offset
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Column != b.Column {
			return a.Column < b.Column
		}
		if diags[i].Category != diags[j].Category {
			return diags[i].Category < diags[j].Category
		}
		return diags[i].Message < diags[j].Message
	})
	// Deduplicate identical diagnostics.
	if len(diags) < 2 {
		return
	}
	out := diags[:1]
	for _, d := range diags[1:] {
		last := out[len(out)-1]
		if d.Category == last.Category && d.Pos == last.Pos && d.Message == last.Message {
			continue
		}
		out = append(out, d)
	}
}

// --- stable node identity ---------------------------------------------------

type nodeSpan struct {
	production string
	start      int
	end        int
	pos        lexer.Position
	endPos     lexer.Position
}

type nodeIdentity struct {
	production string
	offset     int
}

type nodeKey struct {
	production string
	start      int
	end        int
}

// translateIdentities shifts every persistent identity across an edit.
// Identities whose node starts inside the replaced region are dropped;
// identities in the unchanged prefix and suffix are retained, so a node
// temporarily hidden by a syntax error keeps its ID after the error is fixed.
func translateIdentities(ids map[nodeIdentity]NodeID, span editSpan) map[nodeIdentity]NodeID {
	out := make(map[nodeIdentity]NodeID, len(ids))
	for id, nid := range ids {
		off := mapToNewOffset(id.offset, span)
		if off < 0 {
			continue
		}
		id.offset = off
		out[id] = nid
	}
	return out
}

func (s *Session[G]) assignNodes(events []nodeEvent, tokens []lexer.Token, span editSpan, mapNodes bool) []NodeInfo {
	if mapNodes {
		s.nodeIDs = translateIdentities(s.nodeIDs, span)
	} else {
		s.nodeIDs = map[nodeIdentity]NodeID{}
	}

	spans := make([]nodeSpan, 0, len(events))
	seen := map[nodeKey]bool{}
	for _, e := range events {
		if e.end <= e.start || int(e.end) > len(tokens) {
			continue
		}
		startTok, endTok := tokens[e.start], tokens[e.end]
		key := nodeKey{e.production, startTok.Pos.Offset, endTok.Pos.Offset}
		if seen[key] {
			continue
		}
		seen[key] = true
		spans = append(spans, nodeSpan{
			production: e.production,
			start:      startTok.Pos.Offset,
			end:        endTok.Pos.Offset,
			pos:        startTok.Pos,
			endPos:     endTok.Pos,
		})
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		if spans[i].end != spans[j].end {
			return spans[i].end > spans[j].end
		}
		return spans[i].production < spans[j].production
	})

	out := make([]NodeInfo, 0, len(spans))
	for _, sp := range spans {
		id := s.nodeIDs[nodeIdentity{sp.production, sp.start}]
		if id == 0 {
			s.lastID++
			id = s.lastID
			s.nodeIDs[nodeIdentity{sp.production, sp.start}] = id
		}
		out = append(out, NodeInfo{ID: id, Production: sp.production, Pos: sp.pos, End: sp.endPos})
	}
	return out
}

func mapToNewOffset(oldOffset int, span editSpan) int {
	switch {
	case oldOffset < span.oldStart:
		return oldOffset
	case oldOffset >= span.oldEnd:
		return oldOffset + (span.newEnd - span.oldEnd)
	default:
		return -1
	}
}

// --- source bookkeeping -----------------------------------------------------

func applyEdits(src string, edits []Edit) (string, error) {
	for i, e := range edits {
		if e.Start < 0 || e.End < e.Start || e.End > len(src) {
			return "", &FallbackReason{
				Code:    "invalid-edit",
				Message: "edit " + strconv.Itoa(i) + " range [" + strconv.Itoa(e.Start) + "," + strconv.Itoa(e.End) + ") is outside of the source (length " + strconv.Itoa(len(src)) + ")",
				Pos:     lexer.Position{Offset: e.Start},
			}
		}
		if i > 0 && e.Start < edits[i-1].End {
			return "", &FallbackReason{
				Code:    "invalid-edit",
				Message: "edit " + strconv.Itoa(i) + " overlaps a previous edit",
				Pos:     lexer.Position{Offset: e.Start},
			}
		}
	}
	var b strings.Builder
	prev := 0
	for _, e := range edits {
		b.WriteString(src[prev:e.Start])
		b.WriteString(e.Text)
		prev = e.End
	}
	b.WriteString(src[prev:])
	return b.String(), nil
}

// diffSpan returns the single minimal [start,end) span that differs between two
// texts, expressed in old and new coordinates. Boundaries never split UTF-8
// runes. Identical input yields a zero-length span.
func diffSpan(old, new_ string) (span editSpan, ok bool) {
	start := 0
	for start < len(old) && start < len(new_) && old[start] == new_[start] {
		start++
	}
	for start > 0 && !utf8.RuneStart(old[start-1]) {
		start--
	}
	oldEnd, newEnd := len(old), len(new_)
	for oldEnd > start && newEnd > start && old[oldEnd-1] == new_[newEnd-1] {
		oldEnd--
		newEnd--
	}
	return editSpan{oldStart: start, oldEnd: oldEnd, newEnd: newEnd}, true
}

func positionAt(filename, src string, offset int) lexer.Position {
	if offset < 0 {
		offset = 0
	}
	if offset > len(src) {
		offset = len(src)
	}
	pos := lexer.Position{Filename: filename, Line: 1, Column: 1}
	pos.Advance(src[:offset])
	return pos
}

func sourceHash(filename, src string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(filename))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(src))
	return h.Sum64()
}
