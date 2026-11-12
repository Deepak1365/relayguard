package changelog

import (
	"reflect"
	"testing"
)

func TestDiffUpdate(t *testing.T) {
	before := []byte(`{"id": 1, "credit_balance": 0, "email": "a@x", "row_version": 1}`)
	after := []byte(`{"id":1,"credit_balance":20,"email":"a@x","row_version":2}`)
	got, err := Diff(before, after)
	if err != nil {
		t.Fatal(err)
	}
	// Sorted by column; unchanged columns (id, email) are left out even
	// though the two images differ in whitespace.
	want := []FieldDiff{
		{Field: "credit_balance", Before: []byte("0"), After: []byte("20")},
		{Field: "row_version", Before: []byte("1"), After: []byte("2")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Diff = %q, want %q", got, want)
	}
}

func TestDiffInsertAndDelete(t *testing.T) {
	img := []byte(`{"b": {"x": 1}, "a": null}`)
	ins, err := Diff(nil, img)
	if err != nil {
		t.Fatal(err)
	}
	// A JSON null column is a value ("null"), not a missing column (nil).
	want := []FieldDiff{
		{Field: "a", Before: nil, After: []byte("null")},
		{Field: "b", Before: nil, After: []byte(`{"x":1}`)},
	}
	if !reflect.DeepEqual(ins, want) {
		t.Fatalf("insert Diff = %q, want %q", ins, want)
	}
	del, err := Diff(img, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(del) != 2 || del[0].After != nil || string(del[0].Before) != "null" {
		t.Fatalf("delete Diff = %q, want both columns with no after value", del)
	}
}

func TestDiffRejectsBadImages(t *testing.T) {
	for _, img := range []string{`null`, `[1]`, `{"a":`} {
		if _, err := Diff([]byte(img), nil); err == nil {
			t.Errorf("Diff(%s, nil): no error, want one", img)
		}
	}
}

func TestFormat(t *testing.T) {
	cases := []struct {
		name string
		c    Change
		want string
	}{
		{"update", Change{Table: "customers", RowID: 7, Op: OpUpdate,
			Before: []byte(`{"credit_balance":0,"row_version":1}`),
			After:  []byte(`{"credit_balance":20,"row_version":2}`)},
			"update customers 7: credit_balance 0 -> 20, row_version 1 -> 2"},
		// A 0-credit invoice: the balance does not move but the trigger still
		// bumps row_version, so the update is visible in the diff.
		{"zero-credit update", Change{Table: "customers", RowID: 7, Op: OpUpdate,
			Before: []byte(`{"credit_balance":5,"row_version":3}`),
			After:  []byte(`{"credit_balance":5,"row_version":4}`)},
			"update customers 7: row_version 3 -> 4"},
		{"insert", Change{Table: "credit_grants", RowID: 2, Op: OpInsert,
			After: []byte(`{"amount":20,"stripe_invoice_id":"in_1"}`)},
			`insert credit_grants 2: amount (none) -> 20, stripe_invoice_id (none) -> "in_1"`},
		{"no change", Change{Table: "subscriptions", RowID: 1, Op: OpUpdate,
			Before: []byte(`{"status":"active"}`), After: []byte(`{"status":"active"}`)},
			"update subscriptions 1: (no column changed)"},
	}
	for _, c := range cases {
		got, err := Format(c.c)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.name, got, c.want)
		}
	}
}

func TestChangeValidate(t *testing.T) {
	img := []byte(`{}`)
	valid := []Change{
		{Table: "customers", Op: OpInsert, After: img},
		{Table: "customers", Op: OpUpdate, Before: img, After: img},
		{Table: "customers", Op: OpDelete, Before: img},
	}
	for _, c := range valid {
		if err := c.validate(); err != nil {
			t.Errorf("%+v: %v, want valid", c, err)
		}
	}
	invalid := []Change{
		{Table: "customers", Op: OpInsert, Before: img, After: img},
		{Table: "customers", Op: OpInsert},
		{Table: "customers", Op: OpUpdate, After: img},
		{Table: "customers", Op: OpDelete, Before: img, After: img},
		{Table: "customers", Op: "upsert", After: img},
		{Op: OpInsert, After: img},
	}
	for _, c := range invalid {
		if err := c.validate(); err == nil {
			t.Errorf("%+v: valid, want an error", c)
		}
	}
}
