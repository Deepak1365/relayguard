package changelog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// FieldDiff is one column whose value differs between two row images.
// Before/After hold the compacted JSON value, or nil if the column is absent
// on that side (every column is absent on one side of an insert or delete).
type FieldDiff struct {
	Field  string
	Before []byte
	After  []byte
}

// Diff compares two row images column by column and returns the columns that
// differ, sorted by name so output is deterministic. Either image may be nil
// (insert or delete). Values are compared as compacted JSON text: jsonb
// output is already canonical, so equal values have equal text.
//
// Pure (no database), so preview and admin output can be unit tested.
func Diff(before, after []byte) ([]FieldDiff, error) {
	b, err := decodeImage(before)
	if err != nil {
		return nil, fmt.Errorf("before image: %w", err)
	}
	a, err := decodeImage(after)
	if err != nil {
		return nil, fmt.Errorf("after image: %w", err)
	}
	fields := make(map[string]struct{}, len(a)+len(b))
	for k := range b {
		fields[k] = struct{}{}
	}
	for k := range a {
		fields[k] = struct{}{}
	}
	names := make([]string, 0, len(fields))
	for k := range fields {
		names = append(names, k)
	}
	sort.Strings(names)

	var out []FieldDiff
	for _, k := range names {
		bv, inB := b[k]
		av, inA := a[k]
		if inB && inA && bytes.Equal(bv, av) {
			continue
		}
		out = append(out, FieldDiff{Field: k, Before: bv, After: av})
	}
	return out, nil
}

// Format renders a change as one line, e.g.
//
//	update customers 1: credit_balance 0 -> 20, row_version 1 -> 2
//
// "(none)" marks a column that does not exist on that side. An update with no
// differing columns prints "(no column changed)"; with the row_version
// trigger that should not happen for real updates, so it is worth seeing.
func Format(c Change) (string, error) {
	diffs, err := Diff(c.Before, c.After)
	if err != nil {
		return "", fmt.Errorf("format %s %s %d: %w", c.Op, c.Table, c.RowID, err)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s %d: ", c.Op, c.Table, c.RowID)
	if len(diffs) == 0 {
		sb.WriteString("(no column changed)")
		return sb.String(), nil
	}
	for i, d := range diffs {
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "%s %s -> %s", d.Field, valueText(d.Before), valueText(d.After))
	}
	return sb.String(), nil
}

func valueText(v []byte) string {
	if v == nil {
		return "(none)"
	}
	return string(v)
}

// decodeImage splits a row image into compacted per-column JSON values.
// A nil image is an empty row.
func decodeImage(img []byte) (map[string][]byte, error) {
	if img == nil {
		return nil, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(img, &raw); err != nil {
		return nil, err
	}
	if raw == nil {
		// The JSON literal null is not a row image.
		return nil, errors.New("image is JSON null, want an object")
	}
	out := make(map[string][]byte, len(raw))
	for k, v := range raw {
		var buf bytes.Buffer
		if err := json.Compact(&buf, v); err != nil {
			return nil, fmt.Errorf("column %s: %w", k, err)
		}
		out[k] = buf.Bytes()
	}
	return out, nil
}
