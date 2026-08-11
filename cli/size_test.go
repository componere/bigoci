package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/imgoci/bigoci"
)

// TestParseSizeAccepts checks the whole accepted grammar, unit by unit, so a
// change to any multiplier shows up as a wrong number rather than a wrong shape.
func TestParseSizeAccepts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want bigoci.PartSize
	}{
		{name: "one byte, no unit", text: "1", want: 1},
		{name: "kibibyte as a byte count", text: "1024", want: 1024},
		{name: "explicit bytes", text: "1B", want: 1},
		{name: "short kibibytes", text: "4K", want: 4 * kib},
		{name: "long kibibytes", text: "4KiB", want: 4 * kib},
		{name: "lowercase kibibytes", text: "4kib", want: 4 * kib},
		{name: "short mebibytes", text: "4M", want: 4 * mib},
		{name: "long mebibytes", text: "4MiB", want: 4 * mib},
		{name: "the library default spelled short", text: "512M", want: 512 * mib},
		{name: "short gibibytes", text: "1G", want: gib},
		{name: "long gibibytes", text: "1GiB", want: gib},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseSize(tt.text)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestParseSizeRejects checks every refusal, and checks that the refusals a
// caller is most likely to hit teach what to write instead.
func TestParseSizeRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		text     string
		wants    []string
		notWants []string
	}{
		{name: "empty", text: "", wants: []string{"empty"}},
		{name: "zero", text: "0", wants: []string{"must be positive"}},
		{name: "negative", text: "-1", wants: []string{"no digits"}},
		{name: "fraction", text: "1.5MiB", wants: []string{"fractions are not supported", "1536KiB"}},
		{name: "space before the unit", text: "4 MiB", wants: []string{"spaces are not allowed"}},
		{name: "decimal si unit", text: "4MB", wants: []string{"decimal SI units", "4MiB", "1048576"}},
		{
			name:     "tebibytes are an unknown unit, not a special case",
			text:     "4TiB",
			wants:    []string{`unknown unit "TiB"`, "use B, K or KiB, M or MiB, G or GiB"},
			notWants: []string{"4096"},
		},
		{name: "unit typo", text: "4KiBB", wants: []string{"unknown unit"}},
		{name: "unit with no number", text: "MiB", wants: []string{"no digits"}},
		{name: "hexadecimal", text: "0x10", wants: []string{"unknown unit"}},
		{name: "wider than a byte count", text: "99999999999999999999999", wants: []string{"too large"}},
		{name: "overflows on the multiply", text: "9223372036854775807G", wants: []string{"too large"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseSize(tt.text)
			require.Error(t, err)
			assert.Zero(t, got)
			for _, want := range tt.wants {
				assert.Contains(t, err.Error(), want)
			}
			for _, notWant := range tt.notWants {
				assert.NotContains(t, err.Error(), notWant)
			}
		})
	}
}

// TestFormatSize checks that a size renders with the largest unit that divides
// it exactly, and that what it renders parses back to the same value.
func TestFormatSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		size bigoci.PartSize
		want string
	}{
		{name: "no whole unit divides it", size: 1000, want: "1000B"},
		{name: "one byte", size: 1, want: "1B"},
		{name: "kibibytes", size: 4 * kib, want: "4KiB"},
		{name: "mebibytes", size: 512 * mib, want: "512MiB"},
		{name: "gibibytes", size: 3 * gib, want: "3GiB"},
		{name: "prefers the larger unit", size: 1024 * kib, want: "1MiB"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, formatSize(tt.size))

			roundTripped, err := parseSize(tt.want)
			require.NoError(t, err)
			assert.Equal(t, tt.size, roundTripped)
		})
	}
}

// TestFormatSizeOfLibraryDefault is a tripwire. The help text and the preflight
// line both render the library's default part size, so a change to it is a change
// to what the CLI says; this test is where that change gets noticed.
func TestFormatSizeOfLibraryDefault(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "512MiB", formatSize(bigoci.DefaultPartSize))
}

// TestPartSizeValue checks the flag value itself: the unset zero must render as
// nothing, so the standard library prints no default beside a flag whose real
// default lives in the library.
func TestPartSizeValue(t *testing.T) {
	t.Parallel()

	var value partSizeValue
	assert.Empty(t, value.String())

	require.NoError(t, value.Set("4MiB"))
	assert.Equal(t, bigoci.PartSize(4*mib), bigoci.PartSize(value))
	assert.Equal(t, "4MiB", value.String())

	require.Error(t, value.Set("4MB"))
	assert.Equal(t, "4MiB", value.String(), "a refused argument must leave the value alone")
}
