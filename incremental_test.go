package participle_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/alecthomas/assert/v2"

	"github.com/alecthomas/participle/v2"
	"github.com/alecthomas/participle/v2/lexer"
)

// --- test grammar: a small line oriented configuration language ------------

type incConfig struct {
	Pos     lexer.Position
	Entries []*incEntry `{ @@ }`
}

type incEntry struct {
	Pos lexer.Position
	Key string `@Ident "="`
	Val string `@(Ident | Number | String)`
}

func incTestLexer() *lexer.StatefulDefinition {
	return lexer.MustSimple([]lexer.SimpleRule{
		{"comment", `#[^\n]*`},
		{"whitespace", `\s+`},
		{"String", `"(?:[^"\\]|\\.)*"`},
		{"Ident", `[a-zA-Z_][a-zA-Z0-9_]*`},
		{"Number", `[0-9]+`},
		{"Punct", `[=]`},
	})
}

func incTestParser(t *testing.T) *participle.Parser[incConfig] {
	t.Helper()
	return participle.MustBuild[incConfig](participle.Lexer(incTestLexer()))
}

func findNode(nodes []participle.NodeInfo, line, col int) (participle.NodeInfo, bool) {
	for _, n := range nodes {
		if n.Production == "incEntry" && n.Pos.Line == line && n.Pos.Column == col {
			return n, true
		}
	}
	return participle.NodeInfo{}, false
}

func diagnosticsSorted(t *testing.T, diags []participle.Diagnostic) {
	t.Helper()
	for i := 1; i < len(diags); i++ {
		assert.True(t, diags[i-1].Pos.Offset <= diags[i].Pos.Offset,
			"diagnostics not position sorted: %q before %q", diags[i-1], diags[i])
	}
}

func TestSessionInitialIsFullReparse(t *testing.T) {
	p := incTestParser(t)
	s := p.BeginSession()
	src := "a = 1\nb = 2\nc = 3\n"

	r, err := s.UpdateString("test", src)
	assert.NoError(t, err)
	assert.True(t, r.FullReparse)
	assert.False(t, r.Incremental)
	assert.True(t, r.Fallback != nil)
	assert.Equal(t, participle.FallbackInitial, r.Fallback.Code)
	assert.Equal(t, 0, len(r.Diagnostics))

	full, err := p.ParseString("test", src)
	assert.NoError(t, err)
	assert.True(t, reflect.DeepEqual(full, r.AST))
	assert.True(t, r.Version == 1)
}

func TestSessionPartialASTOnSyntaxError(t *testing.T) {
	p := incTestParser(t)
	s := p.BeginSession()
	// Line 2 is invalid; everything before it must remain available.
	src := "a = 1\nb = =\nc = 3\n"

	r, err := s.UpdateString("test", src)
	assert.NoError(t, err)
	assert.True(t, r.AST != nil)
	assert.True(t, len(r.Diagnostics) >= 1)
	diagnosticsSorted(t, r.Diagnostics)
	assert.Equal(t, participle.DiagnosticParse, r.Diagnostics[0].Category)
	assert.Equal(t, 2, r.Diagnostics[0].Pos.Line)

	// First entry survives untouched.
	assert.True(t, len(r.AST.Entries) >= 1)
	assert.Equal(t, "a", r.AST.Entries[0].Key)
	assert.Equal(t, "1", r.AST.Entries[0].Val)

	// Nodes are reported with stable identities and positions.
	a, ok := findNode(r.Nodes, 1, 1)
	assert.True(t, ok)
	assert.True(t, a.ID != 0)
}

func TestSessionMultipleDiagnosticsSorted(t *testing.T) {
	p := incTestParser(t)
	s := p.BeginSession()
	// Two invalid lines: recovery must resynchronise and report both.
	src := "a = =\nb = =\n"

	r, err := s.UpdateString("test", src)
	assert.NoError(t, err)
	assert.True(t, len(r.Diagnostics) >= 2, "expected at least two diagnostics, got %d: %v", len(r.Diagnostics), r.Diagnostics)
	diagnosticsSorted(t, r.Diagnostics)
	assert.Equal(t, 1, r.Diagnostics[0].Pos.Line)
	assert.Equal(t, 2, r.Diagnostics[len(r.Diagnostics)-1].Pos.Line)
}

func TestSessionConvergesAfterFix(t *testing.T) {
	p := incTestParser(t)
	s := p.BeginSession()
	src := "a = 1\nb = 2\nc = 3\n"

	r1, err := s.UpdateString("test", src)
	assert.NoError(t, err)

	// Introduce, then fix a syntax error.
	_, err = s.UpdateString("test", "a = 1\nb = =\nc = 3\n")
	assert.NoError(t, err)

	r3, err := s.UpdateString("test", src)
	assert.NoError(t, err)

	// Convergence: zero diagnostics and a result identical to a full parse.
	assert.Equal(t, 0, len(r3.Diagnostics))
	full, err := p.ParseString("test", src)
	assert.NoError(t, err)
	assert.True(t, reflect.DeepEqual(full, r3.AST))

	// Stable identities: every entry node keeps the identity it had before
	// the error was introduced.
	for _, want := range r1.Nodes {
		if want.Production != "incEntry" {
			continue
		}
		got, ok := findNode(r3.Nodes, want.Pos.Line, want.Pos.Column)
		assert.True(t, ok)
		assert.Equal(t, want.ID, got.ID)
	}
}

func TestSessionSameVersionIsCached(t *testing.T) {
	p := incTestParser(t)
	s := p.BeginSession()
	src := "a = 1\n"

	r1, err := s.UpdateString("test", src)
	assert.NoError(t, err)
	r2, err := s.UpdateString("test", src)
	assert.NoError(t, err)

	assert.Equal(t, r1, r2) // identical pointer: no recomputation
	assert.Equal(t, r1.Version, r2.Version)
}

func TestSessionIncrementalEditReuse(t *testing.T) {
	p := incTestParser(t)
	s := p.BeginSession()
	src := "a = 1\nb = 2\nc = 3\n"

	r1, err := s.UpdateString("test", src)
	assert.NoError(t, err)

	// Insert a new entry on a new line between b and c.
	insertAt := len("a = 1\nb = 2\n")
	r2, err := s.ApplyEdits("test", participle.Edit{
		Start: insertAt,
		End:   insertAt,
		Text:  "d = 4\n",
	})
	assert.NoError(t, err)

	assert.True(t, r2.Incremental)
	assert.False(t, r2.FullReparse)
	assert.True(t, r2.Fallback == nil)
	assert.Equal(t, 0, len(r2.Diagnostics))

	want := "a = 1\nb = 2\nd = 4\nc = 3\n"
	full, err := p.ParseString("test", want)
	assert.NoError(t, err)
	assert.True(t, reflect.DeepEqual(full, r2.AST))

	// Existing entries keep their identity (c shifts down one line).
	a1, _ := findNode(r1.Nodes, 1, 1)
	b1, _ := findNode(r1.Nodes, 2, 1)
	c1, _ := findNode(r1.Nodes, 3, 1)
	a2, ok := findNode(r2.Nodes, 1, 1)
	assert.True(t, ok)
	b2, ok := findNode(r2.Nodes, 2, 1)
	assert.True(t, ok)
	d2, ok := findNode(r2.Nodes, 3, 1)
	assert.True(t, ok)
	c2, ok := findNode(r2.Nodes, 4, 1)
	assert.True(t, ok)
	assert.Equal(t, a1.ID, a2.ID)
	assert.Equal(t, b1.ID, b2.ID)
	assert.Equal(t, c1.ID, c2.ID)
	assert.True(t, d2.ID != a1.ID && d2.ID != b1.ID && d2.ID != c1.ID)

	// End position of the inserted node and shifted c are correct.
	assert.Equal(t, 4, d2.End.Line)
	assert.Equal(t, 4, c2.Pos.Line)
}

func TestSessionLengthChangingEditShiftsSuffix(t *testing.T) {
	p := incTestParser(t)
	s := p.BeginSession()
	src := "a = 1\nb = 2\nc = 3\n"

	r1, err := s.UpdateString("test", src)
	assert.NoError(t, err)

	// Grow b's value in place: "2" -> "200".
	off := len("a = 1\nb = ")
	r2, err := s.ApplyEdits("test", participle.Edit{Start: off, End: off + 1, Text: "200"})
	assert.NoError(t, err)
	assert.True(t, r2.Incremental)
	assert.Equal(t, 0, len(r2.Diagnostics))

	want := "a = 1\nb = 200\nc = 3\n"
	full, err := p.ParseString("test", want)
	assert.NoError(t, err)
	assert.True(t, reflect.DeepEqual(full, r2.AST))

	a1, _ := findNode(r1.Nodes, 1, 1)
	b1, _ := findNode(r1.Nodes, 2, 1)
	c1, _ := findNode(r1.Nodes, 3, 1)
	a2, _ := findNode(r2.Nodes, 1, 1)
	b2, _ := findNode(r2.Nodes, 2, 1)
	c2, _ := findNode(r2.Nodes, 3, 1)
	assert.Equal(t, a1.ID, a2.ID)
	assert.Equal(t, b1.ID, b2.ID)
	assert.Equal(t, c1.ID, c2.ID)
}

func TestSessionLexicalErrorFallsBack(t *testing.T) {
	p := incTestParser(t)
	s := p.BeginSession()

	// Establish a clean version first so incremental state exists.
	clean := "a = 1\nb = \"ok\"\nc = 3\n"
	_, err := s.UpdateString("test", clean)
	assert.NoError(t, err)

	// Unterminated string: the Simple lexer cannot tokenise past the quote.
	src := "a = 1\nb = \"oops\nc = 3\n"
	r, err := s.UpdateString("test", src)
	assert.NoError(t, err)
	assert.True(t, r.FullReparse)
	assert.True(t, r.Fallback != nil)
	assert.Equal(t, participle.FallbackLexicalError, r.Fallback.Code)
	assert.True(t, r.AST != nil)
	assert.True(t, len(r.Diagnostics) >= 1)
	assert.Equal(t, participle.DiagnosticLex, r.Diagnostics[0].Category)
	assert.Equal(t, 2, r.Diagnostics[0].Pos.Line)

	// Fixing the input converges to a clean full parse.
	fixed := "a = 1\nb = \"oops\"\nc = 3\n"
	r2, err := s.UpdateString("test", fixed)
	assert.NoError(t, err)
	assert.Equal(t, 0, len(r2.Diagnostics))
	full, err := p.ParseString("test", fixed)
	assert.NoError(t, err)
	assert.True(t, reflect.DeepEqual(full, r2.AST))

	// Reintroduce a lexical error: recovery still reports an explicit reason.
	r3, err := s.UpdateString("test", "a = 1\nb = \"oopsX\nc = 3\n")
	assert.NoError(t, err)
	assert.True(t, r3.FullReparse)
	assert.Equal(t, participle.FallbackLexicalError, r3.Fallback.Code)
}

func TestSessionDirtyStateFallback(t *testing.T) {
	p := incTestParser(t)
	s := p.BeginSession()
	good := "a = 1\nb = 2\n"
	_, err := s.UpdateString("test", good)
	assert.NoError(t, err)
	// Introduce a lexical error; stored state is now flagged dirty.
	_, err = s.UpdateString("test", "a = 1\nb = \"oops\n")
	assert.NoError(t, err)
	// A further edit cannot reuse the dirty lexer state.
	r, err := s.UpdateString("test", "a = 1\nb = \"oops2\n")
	assert.NoError(t, err)
	assert.True(t, r.FullReparse)
	assert.Equal(t, participle.FallbackDirtyState, r.Fallback.Code)
}

func TestSessionFilenameChangedFallback(t *testing.T) {
	p := incTestParser(t)
	s := p.BeginSession()
	_, err := s.UpdateString("a.conf", "a = 1\n")
	assert.NoError(t, err)
	r, err := s.UpdateString("b.conf", "a = 1\n")
	assert.NoError(t, err)
	assert.True(t, r.FullReparse)
	assert.Equal(t, participle.FallbackFilenameChanged, r.Fallback.Code)
}

func TestSessionWholeLineDeletion(t *testing.T) {
	p := incTestParser(t)
	s := p.BeginSession()
	src := "a = 1\nd = 4\nb = 2\nc = 3\n"

	r1, err := s.UpdateString("test", src)
	assert.NoError(t, err)

	// Delete the "d = 4\n" line: suffix moves up a line and loses bytes.
	start := len("a = 1\n")
	r2, err := s.ApplyEdits("test", participle.Edit{Start: start, End: start + len("d = 4\n"), Text: ""})
	assert.NoError(t, err)
	assert.True(t, r2.Incremental)
	assert.Equal(t, 0, len(r2.Diagnostics))

	want := "a = 1\nb = 2\nc = 3\n"
	full, err := p.ParseString("test", want)
	assert.NoError(t, err)
	assert.True(t, reflect.DeepEqual(full, r2.AST))

	d1, _ := findNode(r1.Nodes, 2, 1)
	b1, _ := findNode(r1.Nodes, 3, 1)
	c1, _ := findNode(r1.Nodes, 4, 1)
	assert.True(t, d1.ID != 0)
	b2, ok := findNode(r2.Nodes, 2, 1)
	assert.True(t, ok)
	c2, ok := findNode(r2.Nodes, 3, 1)
	assert.True(t, ok)
	assert.Equal(t, b1.ID, b2.ID)
	assert.Equal(t, c1.ID, c2.ID)
	for _, n := range r2.Nodes {
		assert.True(t, n.Production != "incEntry" || n.ID != d1.ID, "deleted node identity must be dropped")
	}
}

func TestSessionMultipleEditsAndUnicode(t *testing.T) {
	// Unicode-capable identifier rule.
	uniDef := lexer.MustSimple([]lexer.SimpleRule{
		{"whitespace", `\s+`},
		{"String", `"(?:[^"\\]|\\.)*"`},
		{"Ident", `[\pL_][\pL0-9_]*`},
		{"Number", `[0-9]+`},
		{"Punct", `[=]`},
	})
	p := participle.MustBuild[incConfig](participle.Lexer(uniDef))
	s := p.BeginSession()
	src := "a = 1\nb = 2\n"

	_, err := s.UpdateString("test", src)
	assert.NoError(t, err)

	// Replace both values; non-ASCII text in edits must not corrupt offsets.
	i1 := len("a = ")
	i2 := len("a = 1\nb = ")
	r2, err := s.ApplyEdits("test",
		participle.Edit{Start: i1, End: i1 + 1, Text: "ααα"},
		participle.Edit{Start: i2, End: i2 + 1, Text: "zzz"},
	)
	assert.NoError(t, err)
	assert.True(t, r2.Incremental)
	assert.Equal(t, 0, len(r2.Diagnostics))

	want := "a = ααα\nb = zzz\n"
	full, err := p.ParseString("test", want)
	assert.NoError(t, err)
	assert.True(t, reflect.DeepEqual(full, r2.AST))
	assert.Equal(t, "ααα", r2.AST.Entries[0].Val)
}

func TestSessionEmptyInput(t *testing.T) {
	p := incTestParser(t)
	s := p.BeginSession()
	r, err := s.UpdateString("test", "")
	assert.NoError(t, err)
	assert.True(t, r.AST != nil)
	assert.Equal(t, 0, len(r.Diagnostics))
	assert.Equal(t, 0, len(r.AST.Entries))
}

func TestSessionLastResultAndVersion(t *testing.T) {
	p := incTestParser(t)
	s := p.BeginSession()
	assert.True(t, s.LastResult() == nil)
	assert.Equal(t, 0, s.Version())

	r1, err := s.UpdateString("test", "a = 1\n")
	assert.NoError(t, err)
	assert.Equal(t, 1, s.Version())
	assert.Equal(t, r1, s.LastResult())

	_, err = s.UpdateString("test", "a = 2\n")
	assert.NoError(t, err)
	assert.Equal(t, 2, s.Version())
}

func TestSessionInvalidEdit(t *testing.T) {
	p := incTestParser(t)
	s := p.BeginSession()
	_, err := s.UpdateString("test", "a = 1\n")
	assert.NoError(t, err)

	_, err = s.ApplyEdits("test", participle.Edit{Start: 100, End: 200, Text: "x"})
	assert.Error(t, err)

	_, err = s.ApplyEdits("test",
		participle.Edit{Start: 0, End: 1, Text: "x"},
		participle.Edit{Start: 0, End: 1, Text: "y"},
	)
	assert.Error(t, err)
}

func TestSessionOneShotParsingStillCompatible(t *testing.T) {
	p := incTestParser(t)

	v, err := p.ParseString("test", "a = 1\n")
	assert.NoError(t, err)
	assert.Equal(t, "a", v.Entries[0].Key)

	_, err = p.ParseString("test", "a = =\n")
	assert.Error(t, err)
	var pe participle.Error
	assert.True(t, errors.As(err, &pe))
	assert.Equal(t, 1, pe.Position().Line)

	// Lexing API still surfaces lexer errors.
	tokens, err := p.Lex("test", strings.NewReader("a = \"oops\n"))
	assert.Error(t, err)
	assert.True(t, tokens == nil)
}

// --- multiline comment: edits inside lexer states reuse a safe outer boundary

type incCommentConfig struct {
	Pos     lexer.Position
	Entries []*incCommentEntry `{ @@ }`
}

type incCommentEntry struct {
	Pos lexer.Position
	Key string `@Ident "="`
	Val string `@(Ident | Number)`
}

func TestSessionEditInsideMultilineComment(t *testing.T) {
	def := lexer.MustSimple([]lexer.SimpleRule{
		{"comment", `/\*[\s\S]*?\*/`},
		{"whitespace", `\s+`},
		{"Ident", `[a-zA-Z_][a-zA-Z0-9_]*`},
		{"Number", `[0-9]+`},
		{"Punct", `[=]`},
	})
	p := participle.MustBuild[incCommentConfig](participle.Lexer(def))
	s := p.BeginSession()
	src := "a = 1\n/* first\n second */\nb = 2\n"

	r1, err := s.UpdateString("test", src)
	assert.NoError(t, err)

	// Edit text strictly inside the comment and add a line to it.
	off := len("a = 1\n/* first\n ")
	r2, err := s.ApplyEdits("test", participle.Edit{Start: off, End: off + len("second"), Text: "second\n third"})
	assert.NoError(t, err)

	want := "a = 1\n/* first\n second\n third */\nb = 2\n"
	full, err := p.ParseString("test", want)
	assert.NoError(t, err)
	assert.True(t, reflect.DeepEqual(full, r2.AST))
	assert.Equal(t, 0, len(r2.Diagnostics))

	// b moved from line 4 to line 5; identity is preserved.
	var b1, b2 participle.NodeInfo
	for _, n := range r1.Nodes {
		if n.Production == "incCommentEntry" && n.Pos.Line == 4 {
			b1 = n
		}
	}
	for _, n := range r2.Nodes {
		if n.Production == "incCommentEntry" && n.Pos.Line == 5 {
			b2 = n
		}
	}
	assert.True(t, b1.ID != 0)
	assert.Equal(t, b1.ID, b2.ID)
}
