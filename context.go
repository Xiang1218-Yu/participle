package participle

import (
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/alecthomas/participle/v2/lexer"
)

type contextFieldSet struct {
	tokens     []lexer.Token
	strct      reflect.Value
	field      structLexerField
	fieldValue []reflect.Value
}

// capturePresence carries field presence data for a deferred capture. It is
// kept separate from contextFieldSet so parses with tracking disabled do not
// pay for the extra fields on every capture.
type capturePresence struct {
	// path is the fully qualified field path of the capture's field.
	path string
	// origin identifies the grammar field the capture came from.
	origin fieldOrigin
}

// fieldOrigin identifies a single grammar field captured into a concrete
// enclosing struct value. It is used to distinguish repeated captures of one
// field from captures of distinct fields that resolve to the same field path.
type fieldOrigin struct {
	// owner is the address of the reflect-allocated struct value the capture
	// applies to. Hits on different values (eg. repeated struct elements or
	// different union members) are repetitions of one path; hits of different
	// fields on the same owner are a genuine path collision.
	owner uintptr
	// typ is the type of the struct lexically containing the field.
	typ reflect.Type
	// name is the Go name of the field.
	name string
	// index is the field's flattened index path within typ.
	index []int
}

// conflictsWith reports whether two origins represent distinct fields of the
// same struct value, the only case that makes a presence path ambiguous.
// Hits on different values (repeated struct elements, different union
// members) are repetitions of a single path.
func (f fieldOrigin) conflictsWith(other fieldOrigin) bool {
	if f.owner != other.owner {
		return false
	}
	if len(f.index) != len(other.index) {
		return true
	}
	for i := range f.index {
		if f.index[i] != other.index[i] {
			return true
		}
	}
	return false
}

func (f fieldOrigin) String() string {
	parts := make([]string, len(f.index))
	for i, n := range f.index {
		parts[i] = strconv.Itoa(n)
	}
	return fmt.Sprintf("field %s.%s (index [%s])", f.typ, f.name, strings.Join(parts, " "))
}

// Context for a single parse.
type parseContext struct {
	lexer.PeekingLexer
	depth             int
	trace             io.Writer
	deepestError      error
	deepestErrorDepth int
	lookahead         int
	caseInsensitive   map[lexer.TokenType]bool
	symbols           map[lexer.TokenType]string
	apply             []*contextFieldSet
	allowTrailing     bool
	// presence holds the per-parse field presence state. It is nil unless
	// tracking was requested, keeping the hot parseContext compact when the
	// feature is disabled.
	presence *presenceState
}

// presenceState holds all parse-scoped field presence data. A fresh value is
// allocated for the root parse context when tracking is enabled, and for each
// speculative branch that needs private presence state.
type presenceState struct {
	// tracker receives the committed hits once the parse finishes.
	tracker *FieldPresence
	// applyMeta parallels parseContext.apply entry for entry.
	applyMeta []capturePresence
	// fieldPath is the stack of enclosing captured production field names,
	// outermost first.
	fieldPath []string
	// hits holds the field paths accepted on this context. It is private to a
	// context so speculative branches can be discarded without poisoning the
	// enclosing parse's presence information.
	hits map[string]presenceHit
}

func newParseContext(lex *lexer.PeekingLexer, def lexer.Definition, lookahead int, caseInsensitive map[lexer.TokenType]bool) parseContext {
	return parseContext{
		PeekingLexer:    *lex,
		caseInsensitive: caseInsensitive,
		symbols:         lexer.SymbolsByRune(def),
		lookahead:       lookahead,
	}
}

// tokenTypeName returns the symbolic name of the given token type - in the
// same `<lower-symbol>` form used elsewhere for grammar references - or ""
// if the token type has no symbolic name (eg. literal tokens).
func (p *parseContext) tokenTypeName(t lexer.TokenType) string {
	symbol := p.symbols[t]
	if symbol == "" {
		return ""
	}
	return "<" + strings.ToLower(symbol) + ">"
}

func (p *parseContext) DeepestError(err error) error {
	if p.PeekingLexer.Cursor() >= p.deepestErrorDepth {
		return err
	}
	if p.deepestError != nil {
		return p.deepestError
	}
	return err
}

// Defer adds a function to be applied once a branch has been picked.
func (p *parseContext) Defer(tokens []lexer.Token, strct reflect.Value, field structLexerField, fieldValue []reflect.Value) {
	set := &contextFieldSet{
		tokens:     tokens,
		strct:      strct,
		field:      field,
		fieldValue: fieldValue,
	}
	p.apply = append(p.apply, set)
	if s := p.presence; s != nil {
		var owner uintptr
		if strct.CanAddr() {
			owner = strct.Addr().Pointer()
		}
		s.applyMeta = append(s.applyMeta, capturePresence{
			path: p.qualifyFieldPath(field.Name),
			origin: fieldOrigin{
				owner: owner,
				typ:   strct.Type(),
				name:  field.Name,
				index: append([]int(nil), field.Index...),
			},
		})
	}
}

// ApplyFrom applies deferred functions from the given offset onwards, leaving
// earlier ones queued for the enclosing struct.
func (p *parseContext) ApplyFrom(start int) error {
	metas := p.presence
	for i, apply := range p.apply[start:] {
		if err := setField(apply.tokens, apply.strct, apply.field, apply.fieldValue); err != nil {
			return err
		}
		if metas != nil {
			meta := metas.applyMeta[start+i]
			// A capture that produced no value (an optional or repeated
			// expression that matched nothing) was not hit by any input.
			if len(apply.fieldValue) == 0 {
				continue
			}
			var pos lexer.Position
			if len(apply.tokens) > 0 {
				pos = apply.tokens[0].Pos
			}
			if err := p.recordPresence(meta.path, meta.origin, pos); err != nil {
				return err
			}
		}
	}
	p.apply = p.apply[:start]
	if metas != nil {
		metas.applyMeta = metas.applyMeta[:start]
	}
	return nil
}

// Branch accepts the branch as the correct branch.
func (p *parseContext) Accept(branch *parseContext) error {
	p.apply = append(p.apply, branch.apply...)
	p.PeekingLexer = branch.PeekingLexer
	if ps := p.presence; ps != nil {
		bs := branch.presence
		ps.applyMeta = append(ps.applyMeta, bs.applyMeta...)
		// Merge in deterministic (path) order so ambiguity errors are stable.
		for _, path := range branch.sortedPresencePaths() {
			hit := bs.hits[path]
			if err := p.recordPresence(path, hit.origin, hit.pos); err != nil {
				return err
			}
		}
	}
	if branch.deepestErrorDepth >= p.deepestErrorDepth {
		p.deepestErrorDepth = branch.deepestErrorDepth
		p.deepestError = branch.deepestError
	}
	return nil
}

// Branch starts a new lookahead branch.
func (p *parseContext) Branch() *parseContext {
	branch := &parseContext{}
	*branch = *p
	branch.apply = nil
	if ps := p.presence; ps != nil {
		// Presence state is private to the branch until it is accepted; the
		// field path stack is copied on every push, so sharing it here is
		// safe and avoids allocation.
		branch.presence = &presenceState{
			tracker:   ps.tracker,
			fieldPath: ps.fieldPath,
			hits:      map[string]presenceHit{},
		}
	}
	return branch
}

func (p *parseContext) MaybeUpdateError(err error) {
	if p.PeekingLexer.Cursor() >= p.deepestErrorDepth {
		p.deepestError = err
		p.deepestErrorDepth = p.PeekingLexer.Cursor()
	}
}

// Stop returns true if parsing should terminate after the given "branch" failed to match.
//
// Additionally, track the deepest error in the branch - the deeper the error, the more useful it usually is.
// It could already be the deepest error in the branch (only if deeper than current parent context deepest),
// or it could be "err", the latest error on the branch (even if same depth; the lexer holds the position).
func (p *parseContext) Stop(err error, branch *parseContext) (bool, error) {
	if branch.deepestErrorDepth > p.deepestErrorDepth {
		p.deepestError = branch.deepestError
		p.deepestErrorDepth = branch.deepestErrorDepth
	} else if branch.PeekingLexer.Cursor() >= p.deepestErrorDepth {
		p.deepestError = err
		p.deepestErrorDepth = maxInt(branch.PeekingLexer.Cursor(), branch.deepestErrorDepth)
	}
	if !p.hasInfiniteLookahead() && branch.PeekingLexer.Cursor() > p.PeekingLexer.Cursor()+p.lookahead {
		return true, p.Accept(branch)
	}
	return false, nil
}

func (p *parseContext) hasInfiniteLookahead() bool { return p.lookahead < 0 }

// enterField pushes an enclosing captured production field name onto the
// presence path stack. The previous stack is returned for leaveField.
func (p *parseContext) enterField(name string) []string {
	s := p.presence
	prev := s.fieldPath
	path := make([]string, 0, len(prev)+1)
	path = append(path, prev...)
	s.fieldPath = append(path, name)
	return prev
}

// leaveField restores a presence path stack returned by enterField.
func (p *parseContext) leaveField(prev []string) {
	p.presence.fieldPath = prev
}

// qualifyFieldPath returns the full presence path of "name" below the current
// stack of enclosing captured production fields.
func (p *parseContext) qualifyFieldPath(name string) string {
	stack := p.presence.fieldPath
	if len(stack) == 0 {
		return name
	}
	return strings.Join(stack, ".") + "." + name
}

// recordPresence records "path" as hit. A repeated hit on the same grammar
// field (eg. repetitions and slices) is accepted; a hit on a different field
// that resolves to the same path is an ambiguity and yields a positioned
// error, since presence for such a path could not be attributed uniquely.
func (p *parseContext) recordPresence(path string, origin fieldOrigin, pos lexer.Position) error {
	hits := p.presence.hits
	if existing, ok := hits[path]; ok && existing.origin.conflictsWith(origin) {
		return Errorf(pos, "ambiguous field presence path %q: produced by both %s and %s; rename the fields or avoid promoting them from anonymous structs so presence can be attributed to a unique field", path, existing.origin, origin)
	}
	hits[path] = presenceHit{pos: pos, origin: origin}
	return nil
}

func (p *parseContext) sortedPresencePaths() []string {
	hits := p.presence.hits
	if len(hits) == 0 {
		return nil
	}
	out := make([]string, 0, len(hits))
	for path := range hits {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

func (p *parseContext) printTrace(n node) func() {
	if p.trace != nil {
		tok := p.PeekingLexer.Peek()
		fmt.Fprintf(p.trace, "%s%q %s\n", strings.Repeat(" ", p.depth*2), tok, n.GoString())
		p.depth++
		return func() { p.depth-- }
	}
	return func() {}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
