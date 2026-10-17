package booking

import "testing"

func TestNormalizeCustomer(t *testing.T) {
	cases := []struct {
		name string
		in   CustomerInput
		ok   bool
	}{
		{"phone only", CustomerInput{Name: "Ann", Phone: "(02) 9876-5432"}, true},
		{"email only", CustomerInput{Name: "Ann", Email: "ann@example.com"}, true},
		{"blank name", CustomerInput{Name: " \t", Phone: "0400111222"}, false},
		{"no contact", CustomerInput{Name: "Ann"}, false},
		{"two @", CustomerInput{Name: "Ann", Email: "a@b@c.com"}, false},
		{"no dot in host", CustomerInput{Name: "Ann", Email: "ann@localhost"}, false},
		{"space in email", CustomerInput{Name: "Ann", Email: "an n@example.com"}, false},
		{"too few digits", CustomerInput{Name: "Ann", Phone: "+1 (23) 4"}, false},
		{"letters in phone", CustomerInput{Name: "Ann", Phone: "0400 CALL ME"}, false},
		{"long name", CustomerInput{Name: string(make([]rune, MaxCustomerName+1)), Phone: "0400111222"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := normalizeCustomer(tc.in)
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
		})
	}
	c, err := normalizeCustomer(CustomerInput{Name: "  Ann Lee ", Email: " Ann@Example.COM "})
	if err != nil || c.Name != "Ann Lee" || c.Email != "ann@example.com" {
		t.Fatalf("normalized = %+v %v", c, err)
	}
}
