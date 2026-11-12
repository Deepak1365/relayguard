package sim

import (
	"strings"
	"testing"
)

func TestSeedScopePrefixesDoNotCollide(t *testing.T) {
	s1, s12 := seedScope(1), seedScope(12)
	for _, c := range []struct{ prefix, id string }{
		{s1.events, "evt_s12_00001"},
		{s1.customers, "cus_s12_000"},
		{s1.subscriptions, "sub_s12_000"},
	} {
		if strings.HasPrefix(c.id, c.prefix) {
			t.Errorf("seed 1 prefix %q matches seed 12 id %q", c.prefix, c.id)
		}
	}
	// And they do match their own seed's ids (as Generate builds them).
	// SubscriptionChance 1 guarantees a subscription event to check.
	sawSub := false
	for _, e := range Generate(12, Config{Customers: 1, MaxInvoices: 1, SubscriptionChance: 1}) {
		if !strings.HasPrefix(e.ID, s12.events) || !strings.HasPrefix(e.CustomerID, s12.customers) {
			t.Errorf("seed 12 scope %+v does not match its own event %s / %s", s12, e.ID, e.CustomerID)
		}
		if strings.HasPrefix(e.Type, "customer.subscription.") {
			sawSub = true
			if !strings.HasPrefix(e.ObjectID, s12.subscriptions) {
				t.Errorf("seed 12 subscription prefix %q does not match %s", s12.subscriptions, e.ObjectID)
			}
		}
	}
	if !sawSub {
		t.Fatal("generated history has no subscription event")
	}
}

func TestCompareApplyRows(t *testing.T) {
	cases := []struct {
		typ, status string
		got         []string
		wantKind    string // "" = no violation
	}{
		{"customer.created", "done", []string{"customers"}, ""},
		{"invoice.paid", "done", []string{"credit_grants", "customers"}, ""},
		{"customer.subscription.updated", "done", []string{"subscriptions"}, ""},
		{"invoice.paid", "done", []string{"credit_grants"}, KindChangeLogMissing},
		{"invoice.paid", "done", nil, KindChangeLogMissing},
		{"customer.created", "done", []string{"customers", "customers"}, KindChangeLogUnexpected},
		{"invoice.paid", "done", []string{"customers", "credit_grants"}, KindChangeLogUnexpected}, // wrong order
		{"customer.subscription.created", "stale", nil, ""},
		{"customer.subscription.created", "stale", []string{"subscriptions"}, KindChangeLogUnexpected},
		{"charge.refunded", "skipped", nil, ""},
		{"test.poison", "dead", nil, ""},
		{"invoice.paid", "dead", []string{"credit_grants"}, KindChangeLogUnexpected},
		{"invoice.paid", "pending", nil, ""},
		{"charge.refunded", "done", []string{"customers"}, KindChangeLogUnexpected}, // unknown type has no effect
	}
	for _, c := range cases {
		// Well-numbered seqs here; numbering is tested below.
		seqs := make([]int32, len(c.got))
		for i := range seqs {
			seqs[i] = int32(i + 1)
		}
		got := compareApplyRows("evt", c.typ, c.status, c.got, seqs)
		kind := ""
		if got != nil {
			kind = got.Kind
		}
		if kind != c.wantKind {
			t.Errorf("%s %s %v: kind %q, want %q", c.status, c.typ, c.got, kind, c.wantKind)
		}
	}
}

// Right tables, wrong numbering: the Recorder always numbers 1..n, so
// anything else means rows were renumbered or moved between events.
func TestCompareApplyRowsSeq(t *testing.T) {
	tables := []string{"credit_grants", "customers"}
	cases := []struct {
		seqs     []int32
		wantKind string
	}{
		{[]int32{1, 2}, ""},
		{[]int32{2, 3}, KindChangeLogUnexpected}, // offset
		{[]int32{1, 3}, KindChangeLogUnexpected}, // gap
	}
	for _, c := range cases {
		got := compareApplyRows("evt", "invoice.paid", "done", tables, c.seqs)
		kind := ""
		if got != nil {
			kind = got.Kind
			if !strings.Contains(got.Detail, "no gaps") {
				t.Errorf("seq %v: detail %q, want the numbering reason", c.seqs, got.Detail)
			}
		}
		if kind != c.wantKind {
			t.Errorf("seq %v: kind %q, want %q", c.seqs, kind, c.wantKind)
		}
	}
}

func TestOtherGenerationRows(t *testing.T) {
	if got := otherGenerationRows("evt", 1, nil); got != nil {
		t.Fatalf("no other generations: %+v, want no violation", *got)
	}
	got := otherGenerationRows("evt", 1, []int32{0, 2})
	if got == nil || got.Kind != KindChangeLogUnexpected || got.Subject != "evt" ||
		!strings.Contains(got.Detail, "[0 2]") || !strings.Contains(got.Detail, "generation 1") {
		t.Fatalf("other generations [0 2] at generation 1: %+v, want changelog_unexpected naming both", got)
	}
}

func rv(n int64) *int64 { return &n }

func TestCheckChain(t *testing.T) {
	ins := chainEntry{ID: 1, Op: "insert", AfterRV: rv(1)}
	upd := func(id, after int64, chained bool) chainEntry {
		return chainEntry{ID: id, Op: "update", AfterRV: rv(after), Chained: chained}
	}
	cases := []struct {
		name     string
		entries  []chainEntry
		wantKind string
		wantText string
	}{
		{"insert only", []chainEntry{ins}, "", ""},
		{"insert, two updates", []chainEntry{ins, upd(2, 2, true), upd(3, 3, true)}, "", ""},
		{"insert, update, delete", []chainEntry{ins, upd(2, 2, true), {ID: 3, Op: "delete", Chained: true}}, "", ""},
		{"no entries", []chainEntry{{}}, KindChangeLogMissing, "no change_log entry"},
		{"starts with update", []chainEntry{upd(1, 2, false)}, KindChangeLogChainBreak, "want insert"},
		{"first row_version 5", []chainEntry{{ID: 1, Op: "insert", AfterRV: rv(5)}}, KindChangeLogChainBreak, "want 1"},
		{"first row_version missing", []chainEntry{{ID: 1, Op: "insert"}}, KindChangeLogChainBreak, "missing"},
		{"images not chained", []chainEntry{ins, upd(2, 2, false)}, KindChangeLogChainBreak, "unlogged write"},
		{"row_version skips", []chainEntry{ins, upd(2, 3, true)}, KindChangeLogChainBreak, "want 2"},
		{"row_version repeats", []chainEntry{ins, upd(2, 1, true)}, KindChangeLogChainBreak, "want 2"},
		{"second insert", []chainEntry{ins, {ID: 2, Op: "insert", AfterRV: rv(1), Chained: true}}, KindChangeLogChainBreak, "already existed"},
		{"entry after delete", []chainEntry{ins, {ID: 2, Op: "delete", Chained: true}, upd(3, 2, true)}, KindChangeLogChainBreak, "follow a delete"},
	}
	for _, c := range cases {
		got := checkChain("customers/1", c.entries)
		if c.wantKind == "" {
			if got != nil {
				t.Errorf("%s: %+v, want no violation", c.name, *got)
			}
			continue
		}
		if got == nil || got.Kind != c.wantKind || !strings.Contains(got.Detail, c.wantText) {
			t.Errorf("%s: %+v, want %s containing %q", c.name, got, c.wantKind, c.wantText)
		}
	}
}
