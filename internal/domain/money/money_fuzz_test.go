package money

import "testing"

// Whatever New accepts, its decimal rendering parses back to the same value.
func FuzzMoneyRoundTrip(f *testing.F) {
	for _, seed := range []struct{ amount, currency string }{
		{"10.00", "BRL"}, {"0.00", "USD"}, {"007.50", "EUR"},
		{"92233720368547758.07", "BRL"}, {"92233720368547758.08", "BRL"},
		{"1.5", "BRL"}, {"-1.00", "BRL"}, {"1.00", "brl"}, {"١.٠٠", "BRL"},
	} {
		f.Add(seed.amount, seed.currency)
	}

	f.Fuzz(func(t *testing.T, amount, currency string) {
		m, err := New(amount, currency)
		if err != nil {
			return
		}
		if m.AmountInMinorUnits() < 0 {
			t.Fatalf("New(%q, %q) = %d, negative", amount, currency, m.AmountInMinorUnits())
		}
		back, err := New(m.DecimalString(), m.Currency())
		if err != nil {
			t.Fatalf("New(%q) accepted but its rendering %q does not parse: %v", amount, m.DecimalString(), err)
		}
		if back != m {
			t.Fatalf("round trip %q -> %q -> %v, want %v", amount, m.DecimalString(), back, m)
		}
	})
}
