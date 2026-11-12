package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Deepak1365/relayguard/internal/app"
)

func TestIsBusinessError(t *testing.T) {
	var syntaxErr error
	{
		var v map[string]any
		syntaxErr = json.Unmarshal([]byte(`{"id":`), &v)
	}
	var typeErr error
	{
		var v struct{ AmountPaid int64 }
		typeErr = json.Unmarshal([]byte(`{"AmountPaid":"lots"}`), &v)
	}
	pg := func(code, constraint string) error {
		return &pgconn.PgError{Code: code, ConstraintName: constraint}
	}

	business := []error{
		fmt.Errorf("%w cus_x", app.ErrUnknownCustomer),
		fmt.Errorf("subscription sub_x: %w", app.ErrConcurrentInsert),
		fmt.Errorf("decode invoice: %w", syntaxErr),
		fmt.Errorf("decode invoice: %w", typeErr),
	}
	for _, err := range business {
		if !isBusinessError(err) {
			t.Errorf("isBusinessError(%v) = false, want true", err)
		}
	}

	notBusiness := []error{
		nil,
		errors.New("something unexpected"),
		fmt.Errorf("lock customer 1: %w", pg("55P03", "")),                                 // lock_timeout
		fmt.Errorf("update balance: %w", pg("57014", "")),                                  // statement_timeout
		fmt.Errorf("insert customer: %w", pg("40P01", "")),                                 // deadlock
		fmt.Errorf("insert customer: %w", pg("23505", "customers_stripe_customer_id_key")), // duplicate customer
		fmt.Errorf("record change 1: %w", pg("23505", "change_log_apply_once")),            // concurrent apply
		fmt.Errorf("lookup customer: %w", context.DeadlineExceeded),
		fmt.Errorf("lookup customer: %w", context.Canceled),
		// A chain that mentions a business sentinel but also carries a
		// database error is still not the event's fault.
		fmt.Errorf("%w: %w", app.ErrUnknownCustomer, pg("08006", "")),
	}
	for _, err := range notBusiness {
		if isBusinessError(err) {
			t.Errorf("isBusinessError(%v) = true, want false", err)
		}
	}
}
