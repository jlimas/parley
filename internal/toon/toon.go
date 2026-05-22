// Package toon emits TOON (Token-Oriented Object Notation) output suitable
// for consumption by AI agents. See https://toonformat.dev.
//
// This is a minimal encoder tailored to what parley prints: key-value pairs,
// indented sections, tabular arrays of homogeneous records, and help blocks.
// It is not a full TOON conformance implementation.
package toon

import (
	"fmt"
	"io"
	"strings"
)

// Writer composes TOON output. All methods are chainable; underlying write
// errors are swallowed (output is typically os.Stdout where they are fatal
// anyway).
type Writer struct {
	w io.Writer
}

func New(w io.Writer) *Writer { return &Writer{w: w} }

// KV writes a "key: value" line at the top level (no indent).
func (w *Writer) KV(key string, value any) *Writer {
	fmt.Fprintf(w.w, "%s: %s\n", key, Scalar(value))
	return w
}

// Section opens an indented block under "name:" and invokes fn so the caller
// can add nested key-value lines.
func (w *Writer) Section(name string, fn func(s *Section)) *Writer {
	fmt.Fprintf(w.w, "%s:\n", name)
	fn(&Section{w: w.w, indent: "  "})
	return w
}

// Table writes a tabular array header `name[N]{field1,field2,...}:` followed
// by N rows at one indent level. rows must have len(fields) values each.
func (w *Writer) Table(name string, fields []string, rows [][]any) *Writer {
	fmt.Fprintf(w.w, "%s[%d]{%s}:\n", name, len(rows), strings.Join(fields, ","))
	for _, row := range rows {
		cells := make([]string, len(row))
		for i, v := range row {
			cells[i] = Scalar(v)
		}
		fmt.Fprintf(w.w, "  %s\n", strings.Join(cells, ","))
	}
	return w
}

// HelpLine writes a single-line "help: ..." entry.
func (w *Writer) HelpLine(text string) *Writer {
	fmt.Fprintf(w.w, "help: %s\n", text)
	return w
}

// Help writes a multi-line "help[N]:" block. No-op when lines is empty.
func (w *Writer) Help(lines ...string) *Writer {
	if len(lines) == 0 {
		return w
	}
	if len(lines) == 1 {
		return w.HelpLine(lines[0])
	}
	fmt.Fprintf(w.w, "help[%d]:\n", len(lines))
	for _, line := range lines {
		fmt.Fprintf(w.w, "  %s\n", line)
	}
	return w
}

// Error writes an "error: ..." line. Any extra strings are emitted as help
// (single-line or block depending on count).
func (w *Writer) Error(msg string, help ...string) *Writer {
	fmt.Fprintf(w.w, "error: %s\n", msg)
	return w.Help(help...)
}

// Section represents an indented block opened by Writer.Section.
type Section struct {
	w      io.Writer
	indent string
}

// KV writes "<indent>key: value".
func (s *Section) KV(key string, value any) *Section {
	fmt.Fprintf(s.w, "%s%s: %s\n", s.indent, key, Scalar(value))
	return s
}

// Raw writes a pre-formatted line at the section's indent. The caller is
// responsible for the line's contents; use sparingly.
func (s *Section) Raw(line string) *Section {
	fmt.Fprintf(s.w, "%s%s\n", s.indent, line)
	return s
}

// Table writes a tabular array nested under this section.
func (s *Section) Table(name string, fields []string, rows [][]any) *Section {
	fmt.Fprintf(s.w, "%s%s[%d]{%s}:\n", s.indent, name, len(rows), strings.Join(fields, ","))
	row := s.indent + "  "
	for _, r := range rows {
		cells := make([]string, len(r))
		for i, v := range r {
			cells[i] = Scalar(v)
		}
		fmt.Fprintf(s.w, "%s%s\n", row, strings.Join(cells, ","))
	}
	return s
}

// Scalar renders a Go value as a TOON scalar with quoting where necessary.
func Scalar(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		if x {
			return "true"
		}
		return "false"
	case string:
		return quote(x)
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return fmt.Sprintf("%v", x)
	default:
		return quote(fmt.Sprintf("%v", x))
	}
}

func quote(s string) string {
	if needsQuoting(s) {
		return escapeQuoted(s)
	}
	return s
}

func needsQuoting(s string) bool {
	if s == "" {
		return true
	}
	for _, r := range s {
		switch r {
		case ',', '\t', '|', ':', '"', '\\', '\n', '\r':
			return true
		}
		if r < 0x20 {
			return true
		}
	}
	return false
}

func escapeQuoted(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
