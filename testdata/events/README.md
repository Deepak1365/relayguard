# Synthetic event fixtures

Hand-written events in Stripe's snapshot format. They are NOT real Stripe events
(IDs start with `evt_fx_`), and the receiver does not check signatures yet, so they
can be posted directly. Correct outcome of applying each event exactly once, in order:

| After | Customer `cus_fx_1` | Subscription `sub_fx_1` |
|---|---|---|
| customer_created | balance 0, 0 grants | none |
| invoice_paid ($20.00) | balance 20, 1 grant totalling 20 | none |
| subscription_active | unchanged | active |
| subscription_canceled (newer than active) | unchanged | canceled |
