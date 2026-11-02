package value

import (
	"bytes"
	"math/rand"
	"sort"
	"testing"
)

func dec(s string) Decimal {
	d, err := ParseDecimal(s)
	if err != nil {
		panic(err)
	}
	return d
}

func TestDecimalBasics(t *testing.T) {
	for in, want := range map[string]string{
		"12.50": "12.50", "-0.1": "-0.1", "3": "3", "0.000": "0.000", "1e-2": "0.01", "-1.5E3": "-1500", ".5": "0.5",
	} {
		if got := dec(in).String(); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
	if d, _ := DecimalFromFloat(0.1); d.String() != "0.1" {
		t.Errorf("0.1 from float: %s", d)
	}
	for _, c := range []struct{ op, a, b, want string }{
		{"+", "0.1", "0.2", "0.3"},
		{"-", "1.00", "0.005", "0.995"},
		{"*", "19.99", "3", "59.97"},
		{"*", "1.5", "1.5", "2.25"},
		{"/", "10.00", "3", "3.33333333"},
		{"/", "1", "8", "0.125000"},
		{"%", "10.5", "3", "1.5"},
		{"%", "-10.5", "3", "-1.5"},
	} {
		got, ok, err := DecimalOp(c.op, dec(c.a), dec(c.b))
		if err != nil || !ok || got.String() != c.want {
			t.Errorf("%s %s %s = %s %v %v, want %s", c.a, c.op, c.b, got, ok, err, c.want)
		}
	}
	if _, ok, _ := DecimalOp("/", dec("1"), dec("0.00")); ok {
		t.Error("division by zero gave a value")
	}
	if _, _, err := DecimalOp("*", dec("999999999999"), dec("99999999999")); err == nil {
		t.Error("overflow not reported")
	}
	if r, _ := dec("2.345").Rescale(2); r.String() != "2.35" {
		t.Errorf("round half away: %s", r)
	}
	if r, _ := dec("-2.345").Rescale(2); r.String() != "-2.35" {
		t.Errorf("round half away (negative): %s", r)
	}
	if ParseType("DECIMAL(10, 2)") != "DECIMAL(10,2)" || ParseType("NUMERIC(5)") != "DECIMAL(5,0)" || ParseType("DECIMAL") != Real {
		t.Error("ParseType")
	}
	if _, err := Coerce(dec("123456.789"), "DECIMAL(8,2)"); err != nil {
		t.Errorf("fits after rounding: %v", err)
	}
	if _, err := Coerce(int64(1234567), "DECIMAL(8,2)"); err == nil {
		t.Error("too many digits accepted")
	}
	if v, _ := Coerce("0.1", "DECIMAL(10,2)"); Format(v) != "0.10" {
		t.Errorf("coerce text: %v", v)
	}
	if v, _ := Coerce(dec("4.00"), Integer); v != int64(4) {
		t.Errorf("decimal to integer: %v", v)
	}
	if _, err := Coerce(dec("4.50"), Integer); err == nil {
		t.Error("lossy decimal to integer accepted")
	}
}

func TestDecimalOrderAndEncoding(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	var vals []any
	for i := 0; i < 2000; i++ {
		d := Decimal{rng.Int63n(2e12) - 1e12, uint8(rng.Intn(19))}
		vals = append(vals, d)
		if i%10 == 0 {
			vals = append(vals, int64(rng.Intn(2000)-1000), float64(rng.Intn(2000)-1000)/7)
		}
	}
	vals = append(vals, Decimal{0, 0}, Decimal{0, 5}, Decimal{-1, 18}, Decimal{1, 18})
	// Row records round-trip.
	row, err := DecodeRow(EncodeRow(vals))
	if err != nil {
		t.Fatal(err)
	}
	for i := range vals {
		if row[i] != vals[i] {
			t.Fatalf("row value %d: %v != %v", i, row[i], vals[i])
		}
	}
	// Keys of decimals order as numbers, across scales, and decode.
	var decs []Decimal
	for _, v := range vals {
		if d, ok := v.(Decimal); ok {
			decs = append(decs, d)
		}
	}
	sort.Slice(decs, func(i, j int) bool { return bytes.Compare(AppendKey(nil, decs[i]), AppendKey(nil, decs[j])) < 0 })
	for i := 1; i < len(decs); i++ {
		if decs[i-1].Cmp(decs[i]) > 0 {
			t.Fatalf("key order: %s before %s", decs[i-1], decs[i])
		}
	}
	for _, d := range decs {
		got, ok := DecodeKey(AppendKey(nil, d), 1)
		if !ok || got[0] != d {
			t.Fatalf("key round trip %s: %v", d, got)
		}
	}
	// Compare mixes decimals, integers and floats numerically.
	if Compare(dec("0.10"), 0.1) != 0 || Compare(dec("2.00"), int64(2)) != 0 || Compare(int64(3), dec("2.99")) != 1 {
		t.Error("mixed comparison")
	}
}
