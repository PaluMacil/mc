package main

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// build assembles an NBT document body from tag-encoding helpers. The helpers
// return raw payload bytes so tests can express malformed input as easily as
// well-formed input.
func build(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func nbtString(s string) []byte {
	b := binary.BigEndian.AppendUint16(nil, uint16(len(s)))
	return append(b, s...)
}

// named encodes a named tag header: type byte followed by the name.
func named(t Tag, name string) []byte {
	return append([]byte{byte(t)}, nbtString(name)...)
}

// compound wraps children in a named TAG_Compound terminated by TAG_End.
func compound(name string, children ...[]byte) []byte {
	return build(named(TagCompound, name), build(children...), []byte{byte(TagEnd)})
}

func intTag(name string, v int32) []byte {
	return append(named(TagInt, name), binary.BigEndian.AppendUint32(nil, uint32(v))...)
}

func stringTag(name, v string) []byte {
	return append(named(TagString, name), nbtString(v)...)
}

func intArrayTag(name string, vs ...int32) []byte {
	b := append(named(TagIntArray, name), binary.BigEndian.AppendUint32(nil, uint32(len(vs)))...)
	for _, v := range vs {
		b = binary.BigEndian.AppendUint32(b, uint32(v))
	}
	return b
}

// playerish mimics the shape this tool is actually pointed at: a list of item
// stacks, each with an id and a component compound.
func playerish() []byte {
	item := func(idx int, id string, uuid ...int32) []byte {
		// List elements are bare payloads: no per-element type byte.
		return build(
			intTag("count", int32(idx+1)),
			compound("components", intArrayTag("storage_uuid", uuid...)),
			stringTag("id", id),
			[]byte{byte(TagEnd)},
		)
	}
	list := append(named(TagList, "Inventory"), byte(TagCompound))
	list = binary.BigEndian.AppendUint32(list, 2)
	list = append(list, item(0, "sophisticatedbackpacks:netherite_backpack", 652535079, 76958689)...)
	list = append(list, item(1, "minecraft:obsidian", 1, 2)...)
	return compound("", list)
}

func TestParse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input []byte
		want  []string
	}{
		{
			name:  "scalars render with type suffixes",
			input: compound("", intTag("Slot", 7), stringTag("id", "minecraft:stone")),
			want:  []string{"Slot = 7", `id = "minecraft:stone"`},
		},
		{
			name:  "int array renders in SNBT form",
			input: compound("", intArrayTag("uuid", 652535079, -675931517)),
			want:  []string{"uuid = [I; 652535079, -675931517]"},
		},
		{
			name:  "nested compound reports child count",
			input: compound("", compound("components", intTag("a", 1), intTag("b", 2))),
			want:  []string{"components {} (2)", "a = 1", "b = 2"},
		},
		{
			name:  "list reports element count and indexes children",
			input: playerish(),
			want:  []string{"Inventory [] (2)", "[0] {} (3)", "[1] {} (3)"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root, err := Parse(bytes.NewReader(tc.input))
			require.NoError(t, err)

			var out bytes.Buffer
			require.NoError(t, root.Write(&out, ""))
			for _, want := range tc.want {
				assert.Contains(t, out.String(), want)
			}
		})
	}
}

func TestParseGzip(t *testing.T) {
	t.Parallel()

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, err := zw.Write(compound("", intTag("Slot", 3)))
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	root, err := Parse(bytes.NewReader(gz.Bytes()))
	require.NoError(t, err)

	var out bytes.Buffer
	require.NoError(t, root.Write(&out, ""))
	assert.Contains(t, out.String(), "Slot = 3")
}

func TestParseRejects(t *testing.T) {
	t.Parallel()

	hugeCount := append(named(TagList, "big"), byte(TagCompound))
	hugeCount = binary.BigEndian.AppendUint32(hugeCount, 0x7fffffff)

	tests := []struct {
		name  string
		input []byte
	}{
		{name: "empty input", input: nil},
		{name: "scalar root", input: intTag("x", 1)},
		{name: "truncated mid-payload", input: compound("", intTag("Slot", 7))[:8]},
		{name: "unterminated compound", input: build(named(TagCompound, ""), intTag("a", 1))},
		{name: "unknown tag id", input: build(named(TagCompound, ""), named(Tag(99), "x"))},
		{name: "implausible list count", input: build(named(TagCompound, ""), hugeCount)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(bytes.NewReader(tc.input))
			assert.Error(t, err)
		})
	}
}

func TestWriteGrep(t *testing.T) {
	t.Parallel()

	root, err := Parse(bytes.NewReader(playerish()))
	require.NoError(t, err)

	var out bytes.Buffer
	require.NoError(t, root.Write(&out, "netherite"))
	got := out.String()

	// A match pulls in the whole enclosing item, including sibling fields that
	// do not themselves match.
	assert.Contains(t, got, "netherite_backpack")
	assert.Contains(t, got, "storage_uuid = [I; 652535079, 76958689]")
	assert.NotContains(t, got, "minecraft:obsidian")
	assert.Equal(t, 1, strings.Count(got, "Inventory [] (2)"))
}

func FuzzParse(f *testing.F) {
	f.Add(compound("", intTag("Slot", 7)))
	f.Add(playerish())
	f.Add([]byte{byte(TagCompound), 0, 0})

	// Parse must reject or accept arbitrary bytes, never panic or hang.
	f.Fuzz(func(t *testing.T, data []byte) {
		root, err := Parse(bytes.NewReader(data))
		if err != nil {
			return
		}
		require.NoError(t, root.Write(&bytes.Buffer{}, ""))
	})
}
