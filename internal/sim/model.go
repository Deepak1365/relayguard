package sim

// Expected is the correct final state, computed from the true history with
// plain Go and no database. It is the ground truth the checker compares against.
type Expected struct {
	Customers     map[string]*CustomerState
	Subscriptions map[string]string // subscription id -> final status
	Invoices      int
	TotalCredits  int64
}

type CustomerState struct {
	Credits int64
	Grants  int
}

// Model applies every event exactly once, in true order.
func Model(events []Event) Expected {
	exp := Expected{
		Customers:     map[string]*CustomerState{},
		Subscriptions: map[string]string{},
	}
	for _, e := range events {
		switch e.Type {
		case "customer.created":
			exp.Customers[e.CustomerID] = &CustomerState{}
		case "invoice.paid":
			credits := e.AmountPaid / 100
			c := exp.Customers[e.CustomerID]
			c.Credits += credits
			c.Grants++
			exp.Invoices++
			exp.TotalCredits += credits
		default: // subscription events: the latest one in true order wins
			exp.Subscriptions[e.ObjectID] = e.Status
		}
	}
	return exp
}
