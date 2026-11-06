package participle_test

import (
	"errors"
	"strconv"
	"testing"
	"text/scanner"

	"github.com/alecthomas/assert/v2"

	"github.com/alecthomas/participle/v2"
	"github.com/alecthomas/participle/v2/lexer"
)

func mustBuildPresence[G any](t *testing.T, options ...participle.Option) *participle.Parser[G] {
	t.Helper()
	p, err := participle.Build[G](options...)
	assert.NoError(t, err)
	return p
}

// presenceBool is a custom Capture type whose explicit zero value is false.
type presenceBool bool

func (b *presenceBool) Capture(values []string) error {
	*b = presenceBool(values[0] == "on")
	return nil
}

// Explicit zero values (empty string, 0, false) are reported present and stay
// distinguishable from fields that were absent entirely.
func TestFieldPresenceExplicitZeroValues(t *testing.T) {
	type grammar struct {
		Empty string       `@String`
		Zero  int          `@Int`
		Flag  presenceBool `(@"on" | @"off")?`
	}
	p := mustBuildPresence[grammar](t, participle.Unquote())
	fp := participle.NewFieldPresence()

	ast, err := p.ParseString("", `"" 0 off`, participle.TrackFieldPresence(fp))
	assert.NoError(t, err)
	assert.Equal(t, "", ast.Empty)
	assert.Equal(t, 0, ast.Zero)
	assert.Equal(t, presenceBool(false), ast.Flag)
	assert.True(t, fp.Present("Empty"))
	assert.True(t, fp.Present("Zero"))
	assert.True(t, fp.Present("Flag"))
	assert.Equal(t, []string{"Empty", "Flag", "Zero"}, fp.PresentFields())

	// Sparse input: Flag is absent, Empty and Zero are still present even
	// though their values are zero values.
	fp2 := participle.NewFieldPresence()
	ast2, err := p.ParseString("", `"" 0`, participle.TrackFieldPresence(fp2))
	assert.NoError(t, err)
	assert.True(t, fp2.Present("Empty"), "explicit empty string is present")
	assert.True(t, fp2.Present("Zero"), "explicit zero is present")
	assert.False(t, fp2.Present("Flag"), "uncaptured field is absent")
	assert.False(t, fp2.Present("DoesNotExist"))
	_ = ast2
}

// An explicitly matched, but empty, nested node is present while its absent
// children and wholly missing nodes are not.
func TestFieldPresenceExplicitEmptyNode(t *testing.T) {
	type node struct {
		Name *string `@Ident?`
	}
	type grammar struct {
		Node node ` "{" @@ "}"`
	}
	p := mustBuildPresence[grammar](t)

	fp := participle.NewFieldPresence()
	ast, err := p.ParseString("", `{ }`, participle.TrackFieldPresence(fp))
	assert.NoError(t, err)
	assert.True(t, fp.Present("Node"), "matched empty node is present")
	assert.False(t, fp.Present("Node.Name"), "missing child of an empty node is absent")
	assert.True(t, ast.Node.Name == nil)

	fp2 := participle.NewFieldPresence()
	ast2, err := p.ParseString("", `{ hello }`, participle.TrackFieldPresence(fp2))
	assert.NoError(t, err)
	assert.True(t, fp2.Present("Node"))
	assert.True(t, fp2.Present("Node.Name"))
	assert.Equal(t, "hello", *ast2.Node.Name)
}

// Explicit empty lists ("[]") are distinguishable from completely missing
// nodes: the enclosing node is present while its repeated field is not.
func TestFieldPresenceEmptyListVsMissing(t *testing.T) {
	type listNode struct {
		Items []string `@Ident*`
	}
	type grammar struct {
		Empty *listNode `("[" @@ "]" |`
		Name  string    ` @Ident)`
	}
	p := mustBuildPresence[grammar](t)

	fp := participle.NewFieldPresence()
	ast, err := p.ParseString("", `[]`, participle.TrackFieldPresence(fp))
	assert.NoError(t, err)
	assert.True(t, fp.Present("Empty"), "explicit empty list node is present")
	assert.False(t, fp.Present("Empty.Items"), "zero-element list field is not reported hit")
	assert.False(t, fp.Present("Name"), "untaken alternative is absent")
	assert.Equal(t, []string(nil), ast.Empty.Items)

	fp2 := participle.NewFieldPresence()
	ast2, err := p.ParseString("", `[ a b ]`, participle.TrackFieldPresence(fp2))
	assert.NoError(t, err)
	assert.True(t, fp2.Present("Empty"))
	assert.True(t, fp2.Present("Empty.Items"))
	assert.Equal(t, []string{"a", "b"}, ast2.Empty.Items)

	fp3 := participle.NewFieldPresence()
	ast3, err := p.ParseString("", `thing`, participle.TrackFieldPresence(fp3))
	assert.NoError(t, err)
	assert.True(t, fp3.Present("Name"))
	assert.False(t, fp3.Present("Empty"), "missing node is absent")
	assert.Equal(t, (*listNode)(nil), ast3.Empty)
}

// Pointer nested productions get fully qualified paths, including when absent.
func TestFieldPresenceNestedPointers(t *testing.T) {
	type inner struct {
		B string `@"one"`
		C string `@"two"`
	}
	type grammar struct {
		A *inner `( @@ | "none" )`
	}
	p := mustBuildPresence[grammar](t)

	fp := participle.NewFieldPresence()
	ast, err := p.ParseString("", `one two`, participle.TrackFieldPresence(fp))
	assert.NoError(t, err)
	assert.True(t, fp.Present("A"))
	assert.True(t, fp.Present("A.B"))
	assert.True(t, fp.Present("A.C"))
	assert.Equal(t, &inner{B: "one", C: "two"}, ast.A)

	fp2 := participle.NewFieldPresence()
	ast2, err := p.ParseString("", `none`, participle.TrackFieldPresence(fp2))
	assert.NoError(t, err)
	assert.False(t, fp2.Present("A"))
	assert.False(t, fp2.Present("A.B"))
	assert.True(t, ast2.A == nil)
}

// Fields promoted from anonymous (embedded) structs use promoted paths.
func TestFieldPresenceAnonymousEmbedded(t *testing.T) {
	type mixin struct {
		A string `@Ident`
		B string `@Ident`
	}
	type grammar struct {
		mixin
		C *string `@("c" @Ident)?`
	}
	p := mustBuildPresence[grammar](t)

	fp := participle.NewFieldPresence()
	_, err := p.ParseString("", `one two c three`, participle.TrackFieldPresence(fp))
	assert.NoError(t, err)
	assert.True(t, fp.Present("A"))
	assert.True(t, fp.Present("B"))
	assert.True(t, fp.Present("C"))

	fp2 := participle.NewFieldPresence()
	ast2, err := p.ParseString("", `one two`, participle.TrackFieldPresence(fp2))
	assert.NoError(t, err)
	assert.True(t, fp2.Present("A"))
	assert.True(t, fp2.Present("B"))
	assert.False(t, fp2.Present("C"))
	assert.True(t, ast2.C == nil)
}

// Repeated scalar fields and repeated nested struct fields aggregate under a
// single, index-free field path.
func TestFieldPresenceRepeatedFields(t *testing.T) {
	type item struct {
		Name string `@Ident`
	}
	type grammar struct {
		Tags  []string `@Ident+ "."`
		Items []item   `"[" [ @@ ("," @@)* ] "]"`
	}
	p := mustBuildPresence[grammar](t)

	fp := participle.NewFieldPresence()
	ast, err := p.ParseString("", `a b . [ x , y ]`, participle.TrackFieldPresence(fp))
	assert.NoError(t, err)
	assert.True(t, fp.Present("Tags"))
	assert.True(t, fp.Present("Items"))
	assert.True(t, fp.Present("Items.Name"))
	assert.Equal(t, []string{"a", "b"}, ast.Tags)
	assert.Equal(t, []item{{Name: "x"}, {Name: "y"}}, ast.Items)

	fp2 := participle.NewFieldPresence()
	ast2, err := p.ParseString("", `z . [ ]`, participle.TrackFieldPresence(fp2))
	assert.NoError(t, err)
	assert.True(t, fp2.Present("Tags"))
	assert.False(t, fp2.Present("Items"), "empty repetition is not a hit")
	assert.False(t, fp2.Present("Items.Name"))
	assert.Equal(t, []item(nil), ast2.Items)
}

type (
	presenceUnion interface{ presenceUnionTag() }

	presenceText struct {
		Text string `@Ident`
	}
	presenceNum struct {
		Num float64 `@Int | @Float`
	}
)

func (presenceText) presenceUnionTag() {}
func (presenceNum) presenceUnionTag()  {}

// Only fields of the union member that actually matched are recorded; failed
// speculative branches leave no presence behind.
func TestFieldPresenceUnion(t *testing.T) {
	type grammar struct {
		Value presenceUnion `@@`
	}
	p := mustBuildPresence[grammar](t,
		participle.UseLookahead(10),
		participle.Union[presenceUnion](presenceText{}, presenceNum{}))

	fp := participle.NewFieldPresence()
	ast, err := p.ParseString("", `hello`, participle.TrackFieldPresence(fp))
	assert.NoError(t, err)
	assert.Equal(t, presenceUnion(presenceText{Text: "hello"}), ast.Value)
	assert.True(t, fp.Present("Value"))
	assert.True(t, fp.Present("Value.Text"))
	assert.False(t, fp.Present("Value.Num"))

	fp2 := participle.NewFieldPresence()
	ast2, err := p.ParseString("", `42`, participle.TrackFieldPresence(fp2))
	assert.NoError(t, err)
	assert.Equal(t, presenceUnion(presenceNum{Num: 42}), ast2.Value)
	assert.True(t, fp2.Present("Value"))
	assert.True(t, fp2.Present("Value.Num"))
	assert.False(t, fp2.Present("Value.Text"), "losing union member leaves no trace")
}

// A repeated union field may resolve to different member types within one
// parse; their member fields share a path but must not be treated as a
// collision.
func TestFieldPresenceRepeatedHeterogeneousUnion(t *testing.T) {
	type grammar struct {
		Items []presenceUnion `"(" @@ ("," @@)* ")"`
	}
	p := mustBuildPresence[grammar](t,
		participle.UseLookahead(10),
		participle.Union[presenceUnion](presenceText{}, presenceNum{}))

	fp := participle.NewFieldPresence()
	ast, err := p.ParseString("", `( hello , 42 )`, participle.TrackFieldPresence(fp))
	assert.NoError(t, err)
	assert.Equal(t, []presenceUnion{
		presenceText{Text: "hello"},
		presenceNum{Num: 42},
	}, ast.Items)
	assert.True(t, fp.Present("Items"))
	assert.True(t, fp.Present("Items.Text"))
	assert.True(t, fp.Present("Items.Num"))
	assert.Equal(t, []string{"Items", "Items.Num", "Items.Text"}, fp.PresentFields())
}

type (
	presenceCustom interface{ presenceCustomTag() }

	presenceCustomNumber float64
	presenceCustomBool   bool
)

func (presenceCustomNumber) presenceCustomTag() {}
func (presenceCustomBool) presenceCustomTag()   {}

// ParseTypeWith productions are opaque internally; the capturing field is
// still reported, including for zero values they produce.
func TestFieldPresenceCustomProduction(t *testing.T) {
	type grammar struct {
		Value presenceCustom `@@`
	}
	p := mustBuildPresence[grammar](t, participle.ParseTypeWith(func(lex *lexer.PeekingLexer) (presenceCustom, error) {
		switch lex.Peek().Type {
		case scanner.Int, scanner.Float:
			n, err := strconv.ParseFloat(lex.Next().Value, 64)
			if err != nil {
				return nil, err
			}
			return presenceCustomNumber(n), nil
		case scanner.Ident:
			value := lex.Next().Value
			if value != "true" && value != "false" {
				return nil, participle.NextMatch
			}
			return presenceCustomBool(value == "true"), nil
		default:
			return nil, participle.NextMatch
		}
	}))

	for input, expected := range map[string]presenceCustom{
		"false": presenceCustomBool(false),
		"0.5":   presenceCustomNumber(0.5),
	} {
		fp := participle.NewFieldPresence()
		ast, err := p.ParseString("", input, participle.TrackFieldPresence(fp))
		assert.NoError(t, err)
		assert.Equal(t, expected, ast.Value)
		assert.True(t, fp.Present("Value"), "custom production capture for %q", input)
		assert.Equal(t, []string{"Value"}, fp.PresentFields(), "custom production internals are opaque")
	}
}

// A nested Parseable production is reported on its capturing field even
// though the parser cannot see inside it.
func TestFieldPresenceNestedParseable(t *testing.T) {
	type grammar struct {
		Inner *parseableStruct `@@`
	}
	p := mustBuildPresence[grammar](t)

	fp := participle.NewFieldPresence()
	_, err := p.ParseString("", `hello 123 world`, participle.TrackFieldPresence(fp))
	assert.NoError(t, err)
	assert.True(t, fp.Present("Inner"))
	assert.False(t, fp.Present("Inner.Tokens"))
}

type presenceOpaqueRoot struct{}

func (*presenceOpaqueRoot) Parse(lex *lexer.PeekingLexer) error {
	for !lex.Peek().EOF() {
		lex.Next()
	}
	return nil
}

// Root productions that parse themselves cannot be tracked and yield a
// positioned diagnostic.
func TestFieldPresenceUnsupportedRootProduction(t *testing.T) {
	p := mustBuildPresence[presenceOpaqueRoot](t)

	fp := participle.NewFieldPresence()
	_, err := p.ParseString("input.txt", `anything`, participle.TrackFieldPresence(fp))
	assert.Error(t, err)
	var perr participle.Error
	assert.True(t, errors.As(err, &perr), "expected a positioned participle.Error, got %v", err)
	assert.Equal(t, "input.txt", perr.Position().Filename)
	assert.Contains(t, err.Error(), "field presence tracking is not supported")
	// Failed enablement still clears previous-round state.
	assert.False(t, fp.Present("Anything"))

	// Without tracking, the same parser works normally.
	_, err = p.ParseString("", `anything`)
	assert.NoError(t, err)
}

// Reusing a tracker across parses clears the previous parse's state, including
// state left by a failed parse.
func TestFieldPresenceReuseResets(t *testing.T) {
	type grammar struct {
		A string  `@Ident`
		B *string `@Ident?`
	}
	p := mustBuildPresence[grammar](t)
	fp := participle.NewFieldPresence()

	_, err := p.ParseString("", `one two`, participle.TrackFieldPresence(fp))
	assert.NoError(t, err)
	assert.True(t, fp.Present("A"))
	assert.True(t, fp.Present("B"))

	_, err = p.ParseString("", `one`, participle.TrackFieldPresence(fp))
	assert.NoError(t, err)
	assert.True(t, fp.Present("A"))
	assert.False(t, fp.Present("B"), "previous parse's hit must be cleared")
	assert.Equal(t, []string{"A"}, fp.PresentFields())

	_, err = p.ParseString("", ``, participle.TrackFieldPresence(fp))
	assert.Error(t, err)
	assert.Equal(t, []string(nil), fp.PresentFields(), "failed parse must not retain earlier state")

	_, err = p.ParseString("", `one two`, participle.TrackFieldPresence(fp))
	assert.NoError(t, err)
	assert.True(t, fp.Present("A"))
	assert.True(t, fp.Present("B"))
}

// Distinct embedded fields that resolve to the same promoted path produce a
// locatable ambiguity error only when both are actually hit; dormant
// ambiguity and tracking disabled both keep working.
func TestFieldPresenceAmbiguousEmbeddedPath(t *testing.T) {
	type ambigLeft struct {
		X *string `@Ident?`
	}
	type ambigRight struct {
		X *string `@Ident?`
	}
	type grammar struct {
		ambigLeft
		ambigRight
	}
	p := mustBuildPresence[grammar](t)

	// No ambiguity when only one side is hit.
	fp := participle.NewFieldPresence()
	ast, err := p.ParseString("ambiguous", `a`, participle.TrackFieldPresence(fp))
	assert.NoError(t, err)
	assert.True(t, fp.Present("X"))
	assert.Equal(t, "a", *ast.ambigLeft.X)
	assert.True(t, ast.ambigRight.X == nil)

	// No ambiguity when neither side is hit.
	fp2 := participle.NewFieldPresence()
	_, err = p.ParseString("ambiguous", ``, participle.TrackFieldPresence(fp2))
	assert.NoError(t, err)
	assert.False(t, fp2.Present("X"))

	// Both sides hit: the path cannot be attributed uniquely.
	fp3 := participle.NewFieldPresence()
	_, err = p.ParseString("ambiguous", `a b`, participle.TrackFieldPresence(fp3))
	assert.Error(t, err)
	var perr participle.Error
	assert.True(t, errors.As(err, &perr), "expected positioned error, got %v", err)
	assert.Equal(t, 3, perr.Position().Column, "diagnostic locates the conflicting capture")
	assert.Contains(t, err.Error(), `ambiguous field presence path "X"`)

	// Tracking disabled: identical input parses unchanged.
	ast4, err := p.ParseString("ambiguous", `a b`)
	assert.NoError(t, err)
	assert.Equal(t, "a", *ast4.ambigLeft.X)
	assert.Equal(t, "b", *ast4.ambigRight.X)
}

// Tracking never changes parse results: the same inputs produce identical
// ASTs with and without the tracker.
func TestFieldPresenceLeavesResultsUntouched(t *testing.T) {
	type item struct {
		Name string `@Ident`
	}
	type repeatGrammar struct {
		Tags  []string `@Ident+ "."`
		Items []item   `"[" [ @@ ("," @@)* ] "]"`
	}
	rp := mustBuildPresence[repeatGrammar](t)
	for _, input := range []string{`a b . [ x , y ]`, `z . [ ]`} {
		untracked, err := rp.ParseString("", input)
		assert.NoError(t, err)
		tracked, err := rp.ParseString("", input, participle.TrackFieldPresence(participle.NewFieldPresence()))
		assert.NoError(t, err)
		assert.Equal(t, untracked, tracked)
	}

	type unionGrammar struct {
		Value presenceUnion `@@`
	}
	up := mustBuildPresence[unionGrammar](t,
		participle.UseLookahead(10),
		participle.Union[presenceUnion](presenceText{}, presenceNum{}))
	for _, input := range []string{`hello`, `42`} {
		untracked, err := up.ParseString("", input)
		assert.NoError(t, err)
		tracked, err := up.ParseString("", input, participle.TrackFieldPresence(participle.NewFieldPresence()))
		assert.NoError(t, err)
		assert.Equal(t, untracked, tracked)
	}
}

// The motivating scenario: merge parsed overlays onto defaults without zero
// values clobbering defaults unless they were explicitly provided.
func TestFieldPresenceConfigMergeScenario(t *testing.T) {
	type settings struct {
		Host    string       `("host" "=" @Ident)?`
		Retries int          `("retries" "=" @Int)?`
		Enabled presenceBool `("enabled" "=" (@"on" | @"off"))?`
	}
	p := mustBuildPresence[settings](t)

	defaults := settings{Host: "localhost", Retries: 3, Enabled: true}
	merge := func(input string) settings {
		fp := participle.NewFieldPresence()
		overlay, err := p.ParseString("config", input, participle.TrackFieldPresence(fp))
		assert.NoError(t, err)
		merged := defaults
		if fp.Present("Host") {
			merged.Host = overlay.Host
		}
		if fp.Present("Retries") {
			merged.Retries = overlay.Retries
		}
		if fp.Present("Enabled") {
			merged.Enabled = overlay.Enabled
		}
		return merged
	}

	// Completely missing input keeps every default.
	assert.Equal(t, defaults, merge(``))

	// Explicit zero must win over the default, but untouched fields default.
	merged := merge(`retries = 0`)
	assert.Equal(t, "localhost", merged.Host)
	assert.Equal(t, 0, merged.Retries, "explicit 0 overrides default 3")
	assert.Equal(t, presenceBool(true), merged.Enabled)

	// Explicit empty-string-style value and explicit false override too.
	merged = merge(`host = example enabled = off`)
	assert.Equal(t, "example", merged.Host)
	assert.Equal(t, 3, merged.Retries)
	assert.Equal(t, presenceBool(false), merged.Enabled, "explicit false overrides default true")
}

func TestFieldPresenceNilSafe(t *testing.T) {
	var fp *participle.FieldPresence
	assert.False(t, fp.Present("X"))
	assert.Equal(t, []string(nil), fp.PresentFields())

	type grammar struct {
		A string `@Ident`
	}
	p := mustBuildPresence[grammar](t)
	// A nil tracker option is a no-op.
	ast, err := p.ParseString("", `x`, participle.TrackFieldPresence(nil))
	assert.NoError(t, err)
	assert.Equal(t, "x", ast.A)
}
