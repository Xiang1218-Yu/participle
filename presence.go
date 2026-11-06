package participle

import (
	"sort"

	"github.com/alecthomas/participle/v2/lexer"
)

// FieldPresence records which fields of a grammar value were actually captured
// from the input during a parse.
//
// FieldPresence is opt-in per parse via the TrackFieldPresence ParseOption.
// When tracking is not enabled the parser behaves exactly as it does without
// a FieldPresence value.
//
// A field is "present" when input drives a capture that produces a value for
// it. Presence is recorded independently of that value, so a hit field stays
// present even when the captured value is its zero value, such as "", false
// or 0: merging code can trust the presence signal instead of inspecting the
// value. A field that was never hit is reported absent.
//
// A nested production captured with @@ is present whenever the input matched
// it, even when it matched without capturing anything itself; an explicitly
// matched empty node ("{}") therefore has a present node path with absent
// child paths, which distinguishes it from a node that never matched. A
// repeated or slice field is present when at least one repetition captured; a
// zero-element list inside an explicitly matched node shows the node path
// present with the list path absent.
//
// Fields are identified by dot-separated paths of Go field names relative to
// the root grammar struct, eg. "Server.Addr". Fields promoted from anonymous
// (embedded) structs use their promoted name, exactly as a Go selector would.
// Fields reached through pointers use the same path as non-pointer fields.
// Repeated fields and slices use the plain field name without an index.
// Fields holding union or custom (ParseTypeWith) / Parseable productions are
// present when the input matched the production; fields inside the matched
// union member are reported relative to that field.
type FieldPresence struct {
	hits map[string]presenceHit
}

// presenceHit is a single recorded field path.
//
// origin identifies the grammar field that produced the path, allowing
// genuinely repeated captures of one field to be distinguished from two
// distinct fields that happen to share a path (eg. fields promoted from
// multiple anonymous structs).
type presenceHit struct {
	pos    lexer.Position
	origin fieldOrigin
}

// NewFieldPresence returns an empty FieldPresence tracker.
//
// The same tracker may be passed to multiple parses; each parse clears the
// state recorded by the previous parse.
func NewFieldPresence() *FieldPresence {
	return &FieldPresence{}
}

// TrackFieldPresence is a ParseOption that enables field presence tracking for
// a single parse, recording results in "presence".
//
// Any state left in "presence" from a previous parse is cleared before parsing
// begins, including state from a failed parse, so reusing the same tracker
// never reports stale fields.
//
// Tracking requires the root production to be a struct or a union of struct
// productions. Root productions parsed through ParseTypeWith or that implement
// Parseable are opaque to the parser and cannot be tracked; the parse returns
// an Error locating the unsupported production in that case.
func TrackFieldPresence(presence *FieldPresence) ParseOption {
	return func(p *parseContext) {
		if presence != nil {
			p.presence = &presenceState{tracker: presence}
		}
	}
}

// Present reports whether the field at "path" was captured from the input.
//
// "path" is a dot-separated path of Go field names relative to the root
// grammar struct, eg. "Server.Addr". A nil or unused FieldPresence reports
// every field as absent.
func (f *FieldPresence) Present(path string) bool {
	if f == nil {
		return false
	}
	_, ok := f.hits[path]
	return ok
}

// PresentFields returns the paths of all fields captured from the input, in
// sorted order.
//
// A path is included whenever a field was hit, even if its captured value is
// a zero value; combining a path with the parsed value distinguishes "",
// false and 0 from fields that were entirely absent. An explicitly matched
// empty node shows its own path without its children's paths.
func (f *FieldPresence) PresentFields() []string {
	if f == nil || len(f.hits) == 0 {
		return nil
	}
	out := make([]string, 0, len(f.hits))
	for path := range f.hits {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// reset clears all recorded hits. Called at the start of every tracked parse so
// that reusing a FieldPresence value never retains state from a previous parse.
func (f *FieldPresence) reset() {
	f.hits = map[string]presenceHit{}
}

// commit replaces the recorded hits with "hits". Called once, after a tracked
// parse has finished (successfully or not).
func (f *FieldPresence) commit(hits map[string]presenceHit) {
	f.hits = hits
}
