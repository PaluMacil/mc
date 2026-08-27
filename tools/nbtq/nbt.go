package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
)

// Tag is an NBT tag type id, as used on the wire.
type Tag byte

// The NBT tag types, in wire order.
const (
	TagEnd Tag = iota
	TagByte
	TagShort
	TagInt
	TagLong
	TagFloat
	TagDouble
	TagByteArray
	TagString
	TagList
	TagCompound
	TagIntArray
	TagLongArray
)

// String names the tag type, e.g. "TAG_Compound".
func (t Tag) String() string {
	names := [...]string{
		"TAG_End", "TAG_Byte", "TAG_Short", "TAG_Int", "TAG_Long",
		"TAG_Float", "TAG_Double", "TAG_Byte_Array", "TAG_String",
		"TAG_List", "TAG_Compound", "TAG_Int_Array", "TAG_Long_Array",
	}
	if int(t) < len(names) {
		return names[t]
	}
	return fmt.Sprintf("TAG_Unknown(%d)", byte(t))
}

// ErrMalformed reports input that is not well-formed NBT. Callers can test for
// it with errors.Is; the wrapped message carries the byte offset.
var ErrMalformed = errors.New("malformed nbt")

// maxElements bounds list and array lengths so a corrupt length prefix cannot
// drive an enormous allocation before the read fails.
const maxElements = 1 << 26

// Node is one decoded tag. Scalar tags carry a rendered Scalar and no
// Children; TagList and TagCompound carry Children and an empty Scalar.
type Node struct {
	Type     Tag
	Name     string
	Scalar   string
	Children []*Node
}

// reader walks a byte slice, latching the first error it hits so the parse
// helpers can stay free of per-read error checks.
type reader struct {
	buf []byte
	pos int
	err error
}

func (r *reader) take(n int) []byte {
	if r.err != nil {
		return make([]byte, n)
	}
	if n < 0 || r.pos+n > len(r.buf) {
		r.err = fmt.Errorf("%w: want %d bytes at offset %d, have %d", ErrMalformed, n, r.pos, len(r.buf)-r.pos)
		return make([]byte, max(n, 0))
	}
	b := r.buf[r.pos : r.pos+n]
	r.pos += n
	return b
}

func (r *reader) u8() byte     { return r.take(1)[0] }
func (r *reader) i16() int16   { return int16(binary.BigEndian.Uint16(r.take(2))) }
func (r *reader) i32() int32   { return int32(binary.BigEndian.Uint32(r.take(4))) }
func (r *reader) i64() int64   { return int64(binary.BigEndian.Uint64(r.take(8))) }
func (r *reader) f32() float32 { return math.Float32frombits(binary.BigEndian.Uint32(r.take(4))) }
func (r *reader) f64() float64 { return math.Float64frombits(binary.BigEndian.Uint64(r.take(8))) }
func (r *reader) str() string  { return string(r.take(int(binary.BigEndian.Uint16(r.take(2))))) }

// count reads a 32-bit element count and rejects values that cannot be backed
// by the remaining input.
func (r *reader) count() int {
	n := int(r.i32())
	if r.err != nil {
		return 0
	}
	if n < 0 || n > maxElements {
		r.err = fmt.Errorf("%w: implausible element count %d at offset %d", ErrMalformed, n, r.pos)
		return 0
	}
	return n
}

// Parse reads a whole NBT document, transparently gunzipping it if needed, and
// returns the root tag.
func Parse(src io.Reader) (*Node, error) {
	br := bufio.NewReader(src)
	if magic, err := br.Peek(2); err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("opening gzip stream: %w", err)
		}
		defer zr.Close()
		return parseAll(zr)
	}
	return parseAll(br)
}

func parseAll(src io.Reader) (*Node, error) {
	raw, err := io.ReadAll(src)
	if err != nil {
		return nil, fmt.Errorf("reading nbt: %w", err)
	}
	r := &reader{buf: raw}
	t := Tag(r.u8())
	name := r.str()
	if r.err != nil {
		return nil, r.err
	}
	if t != TagCompound && t != TagList {
		return nil, fmt.Errorf("%w: root tag is %s, want a container", ErrMalformed, t)
	}
	root := r.payload(t, name)
	if r.err != nil {
		return nil, r.err
	}
	return root, nil
}

// payload decodes the body of a tag whose type and name are already known.
func (r *reader) payload(t Tag, name string) *Node {
	n := &Node{Type: t, Name: name}
	if r.err != nil {
		return n
	}
	switch t {
	case TagByte:
		n.Scalar = fmt.Sprintf("%db", int8(r.u8()))
	case TagShort:
		n.Scalar = fmt.Sprintf("%ds", r.i16())
	case TagInt:
		n.Scalar = fmt.Sprintf("%d", r.i32())
	case TagLong:
		n.Scalar = fmt.Sprintf("%dL", r.i64())
	case TagFloat:
		n.Scalar = fmt.Sprintf("%gf", r.f32())
	case TagDouble:
		n.Scalar = fmt.Sprintf("%gd", r.f64())
	case TagString:
		n.Scalar = fmt.Sprintf("%q", r.str())
	case TagByteArray:
		c := r.count()
		r.take(c)
		n.Scalar = fmt.Sprintf("[B; %d bytes]", c)
	case TagIntArray:
		n.Scalar = r.numArray("I", r.count(), func() string { return fmt.Sprintf("%d", r.i32()) })
	case TagLongArray:
		n.Scalar = r.numArray("L", r.count(), func() string { return fmt.Sprintf("%d", r.i64()) })
	case TagList:
		et := Tag(r.u8())
		c := r.count()
		// A list of TAG_End has no payload per element; guard against a huge
		// count turning into a huge slice of empty nodes.
		if et == TagEnd {
			break
		}
		for i := range c {
			if r.err != nil {
				break
			}
			n.Children = append(n.Children, r.payload(et, fmt.Sprintf("[%d]", i)))
		}
	case TagCompound:
		for r.err == nil {
			ct := Tag(r.u8())
			if ct == TagEnd || r.err != nil {
				break
			}
			n.Children = append(n.Children, r.payload(ct, r.str()))
		}
	default:
		r.err = fmt.Errorf("%w: unknown tag %d at offset %d", ErrMalformed, byte(t), r.pos)
	}
	return n
}

func (r *reader) numArray(prefix string, c int, next func() string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s;", prefix)
	for i := range c {
		if r.err != nil {
			break
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte(' ')
		b.WriteString(next())
	}
	b.WriteByte(']')
	return b.String()
}

// Path is a node's dotted address from the root, with list indices appended
// directly, e.g. "Inventory[0].components".
func (n *Node) Path(parent string) string {
	switch {
	case n.Name == "":
		return parent
	case strings.HasPrefix(n.Name, "["):
		return parent + n.Name
	case parent == "":
		return n.Name
	default:
		return parent + "." + n.Name
	}
}

// Write renders the tree to w as an indented outline. When grep is non-empty,
// only subtrees with a matching path or scalar value are printed; matching is
// case-insensitive and the caller is expected to have lowercased grep.
func (n *Node) Write(w io.Writer, grep string) error {
	var b bytes.Buffer
	n.render(&b, "", "", grep)
	_, err := w.Write(b.Bytes())
	return err
}

func (n *Node) render(b *bytes.Buffer, indent, parent, grep string) {
	path := n.Path(parent)
	label := n.Name
	if label == "" {
		label = "(root)"
	}
	if n.Children == nil && n.Type != TagCompound && n.Type != TagList {
		if grep == "" || matches(path, n.Scalar, grep) {
			fmt.Fprintf(b, "%s%s = %s\n", indent, label, n.Scalar)
		}
		return
	}
	if grep != "" && !n.subtreeMatches(parent, grep) {
		return
	}
	brackets := "{}"
	if n.Type == TagList {
		brackets = "[]"
	}
	fmt.Fprintf(b, "%s%s %s (%d)\n", indent, label, brackets, len(n.Children))
	// Keep filtering on the way down until we reach the node that actually
	// matched; from there print the whole subtree, so a hit on an item id
	// shows that item's full component tree and not just the matching line.
	child := grep
	if matches(path, n.Scalar, grep) || n.hasMatchingField(path, grep) {
		child = ""
	}
	for _, c := range n.Children {
		c.render(b, indent+"  ", path, child)
	}
}

// hasMatchingField reports whether a direct scalar child matches. That makes
// the enclosing container the unit of output: grepping an item id prints the
// whole item stack, which is what forensics on player data actually wants.
func (n *Node) hasMatchingField(path, grep string) bool {
	for _, c := range n.Children {
		if c.Children == nil && c.Type != TagCompound && c.Type != TagList &&
			matches(c.Path(path), c.Scalar, grep) {
			return true
		}
	}
	return false
}

func matches(path, scalar, grep string) bool {
	return strings.Contains(strings.ToLower(path+scalar), grep)
}

func (n *Node) subtreeMatches(parent, grep string) bool {
	path := n.Path(parent)
	if matches(path, n.Scalar, grep) {
		return true
	}
	for _, c := range n.Children {
		if c.subtreeMatches(path, grep) {
			return true
		}
	}
	return false
}
