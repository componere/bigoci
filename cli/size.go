package main

import (
	"errors"
	"flag"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"

	"github.com/imgoci/bigoci"
)

const (
	// kib is how many bytes one of the K and KiB units is.
	kib = 1 << 10
	// mib is how many bytes one of the M and MiB units is.
	mib = 1 << 20
	// gib is how many bytes one of the G and GiB units is.
	gib = 1 << 30
)

// Compile-time proof that the -part-size flag holds a value the standard
// library knows how to parse a command line into.
var _ flag.Value = (*partSizeValue)(nil)

// partSizeValue is the value behind the -part-size flag. It parses the part-size
// grammar and renders it back, which keeps that grammar in exactly one place.
type partSizeValue bigoci.PartSize

// String renders the size the way a caller would have typed it, and renders the
// unset zero as nothing at all.
//
// That empty string is what stops [flag.FlagSet.PrintDefaults] from printing
// "(default 0)" beside a flag whose real default lives in the library: the
// standard library omits a default that equals the type's zero value, and this
// type's zero value renders the same way.
func (v *partSizeValue) String() string {
	if v == nil || *v == 0 {
		return ""
	}

	return formatSize(bigoci.PartSize(*v))
}

// Set parses one -part-size argument.
func (v *partSizeValue) Set(text string) error {
	size, err := parseSize(text)
	if err != nil {
		return err
	}
	*v = partSizeValue(size)

	return nil
}

// parseSize turns a part-size argument into a [bigoci.PartSize].
//
// The grammar is narrow on purpose: decimal digits, then an optional binary unit
// of B, K, KiB, M, MiB, G, or GiB, matched without regard to ASCII case. Nothing
// else is accepted — no spaces, no sign, no fraction, no digit separator, and no
// decimal SI unit. A unit that quietly means something other than what the
// caller thought would change the part size, and the part size is part of what
// the manifest digest describes.
func parseSize(text string) (bigoci.PartSize, error) {
	if text == "" {
		return 0, errors.New("part size is empty; write a byte count, or a size such as 512MiB")
	}
	if strings.ContainsFunc(text, unicode.IsSpace) {
		return 0, fmt.Errorf("part size %q: spaces are not allowed; write the unit against the number", text)
	}
	if strings.ContainsAny(text, ".,") {
		return 0, fmt.Errorf(
			"part size %q: fractions are not supported; write a whole number of a smaller unit "+
				"(1.5MiB is 1536KiB)", text,
		)
	}

	digits, unit := splitSize(text)
	if digits == "" {
		return 0, fmt.Errorf("part size %q: no digits; write a byte count, or a size such as 512MiB", text)
	}

	multiplier, err := unitMultiplier(text, digits, unit)
	if err != nil {
		return 0, err
	}

	count, err := strconv.ParseUint(digits, 10, 63)
	if err != nil {
		return 0, fmt.Errorf("part size %q: too large for a byte count", text)
	}
	if count == 0 {
		return 0, fmt.Errorf("part size %q: must be positive", text)
	}
	if count > uint64(math.MaxInt64)/multiplier {
		return 0, fmt.Errorf("part size %q: too large for a byte count", text)
	}

	//nolint:gosec // The multiply is bounded against MaxInt64 on the line above, so it fits.
	return bigoci.PartSize(count * multiplier), nil
}

// formatSize renders size with the largest binary unit that divides it exactly,
// which is the shape a caller would have typed.
//
// Every positive size it produces parses back to the same value, so the help
// text, the preflight line, and the flag all spell a size the same way. No
// caller holds a zero or negative size — the parser refuses them and the
// library default is positive — so those render as bare byte counts nothing
// re-parses.
func formatSize(size bigoci.PartSize) string {
	count := int64(size)
	switch {
	case count >= gib && count%gib == 0:
		return strconv.FormatInt(count/gib, 10) + "GiB"
	case count >= mib && count%mib == 0:
		return strconv.FormatInt(count/mib, 10) + "MiB"
	case count >= kib && count%kib == 0:
		return strconv.FormatInt(count/kib, 10) + "KiB"
	default:
		return strconv.FormatInt(count, 10) + "B"
	}
}

// splitSize cuts text at the end of its leading ASCII decimal digits and returns
// the digits and whatever followed them.
func splitSize(text string) (string, string) {
	end := 0
	for end < len(text) && text[end] >= '0' && text[end] <= '9' {
		end++
	}

	return text[:end], text[end:]
}

// unitMultiplier returns how many bytes one of unit is, or an error that teaches
// what to write instead.
//
// The rejection that carries the most weight is the decimal SI family, which a
// caller reaches for out of habit and which quietly means something other than
// the binary unit beside it. The grammar tops out at GiB: a larger unit has no
// realistic part size to spell, so it stays an unknown unit rather than a
// special case with a story.
func unitMultiplier(text, digits, unit string) (uint64, error) {
	switch strings.ToLower(unit) {
	case "", "b":
		return 1, nil
	case "k", "kib":
		return kib, nil
	case "m", "mib":
		return mib, nil
	case "g", "gib":
		return gib, nil
	case "kb":
		return 0, decimalUnitError(text, digits, "KiB", kib)
	case "mb":
		return 0, decimalUnitError(text, digits, "MiB", mib)
	case "gb":
		return 0, decimalUnitError(text, digits, "GiB", gib)
	default:
		return 0, fmt.Errorf("part size %q: unknown unit %q; use B, K or KiB, M or MiB, G or GiB", text, unit)
	}
}

// decimalUnitError explains that a decimal SI unit was refused and names the
// binary unit to write instead, with the byte count that makes the difference
// concrete.
func decimalUnitError(text, digits, unit string, size int64) error {
	return fmt.Errorf(
		"part size %q: decimal SI units are not supported; write %s%s (1 %s = %d bytes) or a plain byte count",
		text, digits, unit, unit, size,
	)
}
