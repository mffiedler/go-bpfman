package source

// Pos is a single point in source: file identity plus 1-based line
// and column. Col is a byte offset within the line, not a rune
// offset. The zero value means "unknown location".
type Pos struct {
	File string
	Line int
	Col  int
}

// Span is a half-open source range. Pos is the start (inclusive);
// End is one past the last byte of the spanned region (exclusive).
// End == Pos{} means the End field is unset and only the start is
// meaningful.
type Span struct {
	Pos
	End Pos
}
