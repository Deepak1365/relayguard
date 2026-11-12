package main

// Preview must predict exactly what the real worker then does. Same rules
// as main_db_test.go: run through `dev.ps1 db-test`.

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Deepak1365/relayguard/internal/admin"
	"github.com/Deepak1365/relayguard/internal/changelog"
	"github.com/Deepak1365/relayguard/internal/dbtest"
)

func decodeForCompare(t *testing.T, img []byte) map[string]any {
	t.Helper()
	if img == nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(img))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("decode %s: %v", img, err)
	}
	return m
}

func without(m map[string]any, keys ...string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	for _, k := range keys {
		delete(out, k)
	}
	return out
}

// For each event in turn: preview it, then let the real worker process it,
// and compare the preview's change list with the change_log rows the worker
// committed. Same seq order, tables and ops. Updates: same row, identical
// before image, identical after image (row_version included) except
// updated_at, which is now() at write time. Inserts: identical after image
// except the provisional id and the now() timestamps.
func TestDBPreviewMatchesRealApply(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	const cus = "cus_dbtest_pv"
	events := []int64{
		dbtest.InsertEvent(t, pool, "pv_cus", "customer.created", dbtest.CustomerObject(cus)),
		dbtest.InsertEvent(t, pool, "pv_inv", "invoice.paid", dbtest.InvoiceObject("in_dbtest_pv", cus, 2000)),
		dbtest.InsertEventAt(t, pool, "pv_sub_c", "customer.subscription.created", dbtest.DefaultCreated+1,
			dbtest.SubscriptionObject("sub_dbtest_pv", cus, "active")),
		dbtest.InsertEventAt(t, pool, "pv_sub_u", "customer.subscription.updated", dbtest.DefaultCreated+2,
			dbtest.SubscriptionObject("sub_dbtest_pv", cus, "past_due")),
	}
	w := newTestWorker(pool)
	for _, id := range events {
		pv, err := admin.Preview(ctx, pool, id, admin.Options{})
		if err != nil || pv.Outcome != admin.OutcomeApply {
			t.Fatalf("preview %d: %+v, %v", id, pv, err)
		}
		processN(t, w, 1)
		wantStatus(t, pool, id, "done")
		actual := dbtest.ChangeLog(t, pool, id)

		if len(pv.Changes) != len(actual) {
			t.Fatalf("inbox %d: preview has %d changes, the real apply %d", id, len(pv.Changes), len(actual))
		}
		for i, p := range pv.Changes {
			r := actual[i]
			if r.Seq != i+1 || p.Table != r.Table || p.Op != r.Op {
				t.Fatalf("inbox %d change %d: preview %s %s, real seq %d %s %s", id, i+1, p.Op, p.Table, r.Seq, r.Op, r.Table)
			}
			pBefore, pAfter := decodeForCompare(t, p.Before), decodeForCompare(t, p.After)
			switch p.Op {
			case changelog.OpUpdate:
				if p.RowID != r.RowID || !reflect.DeepEqual(pBefore, r.Before) ||
					!reflect.DeepEqual(without(pAfter, "updated_at"), without(r.After, "updated_at")) {
					t.Fatalf("inbox %d update %d differs:\npreview row %d before %v after %v\nreal    row %d before %v after %v",
						id, i+1, p.RowID, pBefore, pAfter, r.RowID, r.Before, r.After)
				}
			case changelog.OpInsert:
				ignore := []string{"id", "created_at", "updated_at"}
				if !reflect.DeepEqual(without(pAfter, ignore...), without(r.After, ignore...)) {
					t.Fatalf("inbox %d insert %d differs:\npreview %v\nreal    %v", id, i+1, pAfter, r.After)
				}
			default:
				t.Fatalf("inbox %d: unexpected op %s", id, p.Op)
			}
		}
	}
}
