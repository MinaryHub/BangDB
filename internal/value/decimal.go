package value

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// Decimal is an exact decimal number: Unscaled / 10^Scale, with up to 18
// significant digits (the range of int64). It is the value of columns
// declared DECIMAL(p,s) or NUMERIC(p,s), for amounts that must add up
// exactly (money), unlike REAL.
type Decimal struct {
	Unscaled int64
	Scale    uint8 // digits after the point, 0..18
}

// MaxDecimalScale is the largest scale (and precision) of a Decimal.
const MaxDecimalScale = 18

// ErrDecimalOverflow is returned when a result does not fit a Decimal.
var ErrDecimalOverflow = errors.New("bangdb: decimal value out of range (more than 18 digits)")

var pow10 = func() [MaxDecimalScale + 1]int64 {
	var p [MaxDecimalScale + 1]int64
	p[0] = 1
	for i := 1; i < len(p); i++ {
		p[i] = p[i-1] * 10
	}
	return p
}()

// ParseDecimal parses text such as "12.50", "-0.1", "3", "1e-2".
func ParseDecimal(s string) (Decimal, error) {
	s = strings.TrimSpace(s)
	r, ok := new(big.Rat).SetString(s)
	if !ok || strings.ContainsAny(s, "/") {
		return Decimal{}, fmt.Errorf("bangdb: %q is not a decimal number", s)
	}
	// The scale is the number of digits the text needs after the point.
	scale := 0
	if i := strings.IndexAny(s, "eE"); i < 0 {
		if j := strings.IndexByte(s, '.'); j >= 0 {
			scale = len(s) - j - 1
		}
	} else {
		for scale <= MaxDecimalScale && !isScaledInt(r, scale) {
			scale++
		}
	}
	if scale > MaxDecimalScale {
		scale = MaxDecimalScale
	}
	return fromRat(r, scale)
}

func isScaledInt(r *big.Rat, scale int) bool {
	x := new(big.Rat).Mul(r, new(big.Rat).SetInt64(pow10[scale]))
	return x.IsInt()
}

// fromRat rounds r to scale digits (half away from zero).
func fromRat(r *big.Rat, scale int) (Decimal, error) {
	x := new(big.Rat).Mul(r, new(big.Rat).SetInt64(pow10[scale]))
	n := roundHalfAway(x)
	if !n.IsInt64() {
		return Decimal{}, ErrDecimalOverflow
	}
	return Decimal{n.Int64(), uint8(scale)}, nil
}

func roundHalfAway(x *big.Rat) *big.Int {
	num, den := new(big.Int).Set(x.Num()), x.Denom()
	neg := num.Sign() < 0
	num.Abs(num)
	q, m := new(big.Int).QuoRem(num, den, new(big.Int))
	if m.Mul(m, big.NewInt(2)).Cmp(den) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if neg {
		q.Neg(q)
	}
	return q
}

// DecimalFromInt converts an integer.
func DecimalFromInt(i int64) Decimal { return Decimal{i, 0} }

// DecimalFromFloat converts f through its shortest decimal form, so that
// a literal such as 0.1 becomes exactly 0.1.
func DecimalFromFloat(f float64) (Decimal, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return Decimal{}, ErrDecimalOverflow
	}
	return ParseDecimal(strconv.FormatFloat(f, 'g', -1, 64))
}

func (d Decimal) rat() *big.Rat { return big.NewRat(d.Unscaled, pow10[d.Scale]) }

// String formats d with exactly Scale digits after the point.
func (d Decimal) String() string {
	neg := d.Unscaled < 0
	u := new(big.Int).Abs(big.NewInt(d.Unscaled)).String()
	if d.Scale > 0 {
		if len(u) <= int(d.Scale) {
			u = strings.Repeat("0", int(d.Scale)-len(u)+1) + u
		}
		u = u[:len(u)-int(d.Scale)] + "." + u[len(u)-int(d.Scale):]
	}
	if neg {
		return "-" + u
	}
	return u
}

// Float64 converts d (inexactly).
func (d Decimal) Float64() float64 { f, _ := d.rat().Float64(); return f }

// Rescale rounds d to scale digits (half away from zero).
func (d Decimal) Rescale(scale int) (Decimal, error) {
	if scale < 0 || scale > MaxDecimalScale {
		return Decimal{}, fmt.Errorf("bangdb: decimal scale %d out of range 0..%d", scale, MaxDecimalScale)
	}
	if scale == int(d.Scale) {
		return d, nil
	}
	return fromRat(d.rat(), scale)
}

// Digits returns the number of digits of the unscaled value.
func (d Decimal) Digits() int {
	return len(new(big.Int).Abs(big.NewInt(d.Unscaled)).String())
}

// Cmp compares two decimals numerically.
func (d Decimal) Cmp(e Decimal) int {
	if d.Scale == e.Scale {
		return cmpInt64(d.Unscaled, e.Unscaled)
	}
	return d.rat().Cmp(e.rat())
}

// DecimalOp applies + - * / or % to two decimals. Sums and differences
// keep the larger scale, products add the scales (up to 18), and
// quotients get the larger scale plus 6 digits (up to 18), rounded. A zero
// divisor gives ok == false (NULL).
func DecimalOp(op string, a, b Decimal) (res Decimal, ok bool, err error) {
	x, y := a.rat(), b.rat()
	scale := int(max(a.Scale, b.Scale))
	r := new(big.Rat)
	switch op {
	case "+":
		r.Add(x, y)
	case "-":
		r.Sub(x, y)
	case "*":
		r.Mul(x, y)
		scale = min(int(a.Scale)+int(b.Scale), MaxDecimalScale)
	case "/", "%":
		if b.Unscaled == 0 {
			return Decimal{}, false, nil
		}
		if op == "/" {
			r.Quo(x, y)
			scale = min(scale+6, MaxDecimalScale)
			break
		}
		// Remainder with the sign of the dividend, as for integers.
		q := new(big.Rat).Quo(x, y)
		t := new(big.Int).Quo(q.Num(), q.Denom()) // truncated toward zero
		r.Sub(x, new(big.Rat).Mul(new(big.Rat).SetInt(t), y))
	default:
		return Decimal{}, false, fmt.Errorf("bangdb: unknown decimal operator %s", op)
	}
	d, err := fromRat(r, scale)
	if err != nil && op == "/" {
		// A long quotient: keep as many digits as fit.
		for s := scale - 1; s >= 0 && err != nil; s-- {
			d, err = fromRat(r, s)
		}
	}
	return d, err == nil, err
}

// DecimalSpec returns the precision and scale of a DECIMAL(p,s) column
// type.
func DecimalSpec(t Type) (p, s int, ok bool) {
	str := string(t)
	if !strings.HasPrefix(str, "DECIMAL(") || !strings.HasSuffix(str, ")") {
		return 0, 0, false
	}
	parts := strings.Split(str[len("DECIMAL("):len(str)-1], ",")
	if len(parts) != 2 {
		return 0, 0, false
	}
	p, err1 := strconv.Atoi(parts[0])
	s, err2 := strconv.Atoi(parts[1])
	return p, s, err1 == nil && err2 == nil
}

// DecimalType returns the column type DECIMAL(p,s).
func DecimalType(p, s int) Type { return Type(fmt.Sprintf("DECIMAL(%d,%d)", p, s)) }

// parseDecimalDecl recognizes DECIMAL(p,s) / NUMERIC(p,s) (and DEC, with
// one argument for scale 0) in a declared type.
func parseDecimalDecl(n string) (Type, bool) {
	open := strings.IndexByte(n, '(')
	if open < 0 || !strings.HasSuffix(n, ")") {
		return "", false
	}
	switch strings.TrimSpace(n[:open]) {
	case "DECIMAL", "NUMERIC", "DEC":
	default:
		return "", false
	}
	args := strings.Split(n[open+1:len(n)-1], ",")
	p, err := strconv.Atoi(strings.TrimSpace(args[0]))
	s := 0
	if err == nil && len(args) == 2 {
		s, err = strconv.Atoi(strings.TrimSpace(args[1]))
	}
	if err != nil || len(args) > 2 || p < 1 || p > MaxDecimalScale || s < 0 || s > p {
		return "", false
	}
	return DecimalType(p, s), true
}

// ToDecimal converts a numeric value (or numeric text) to a Decimal.
func ToDecimal(v any) (Decimal, bool) {
	switch x := v.(type) {
	case Decimal:
		return x, true
	case int64:
		return DecimalFromInt(x), true
	case float64:
		d, err := DecimalFromFloat(x)
		return d, err == nil
	case string:
		d, err := ParseDecimal(x)
		return d, err == nil
	}
	return Decimal{}, false
}

// coerceDecimal stores v in a DECIMAL(p,s) column: rounded to s digits,
// with at most p digits in all.
func coerceDecimal(v any, p, s int) (any, error) {
	d, ok := ToDecimal(v)
	if !ok {
		return nil, fmt.Errorf("%w: cannot store %s value %s in DECIMAL(%d,%d) column", ErrTypeMismatch, TypeOf(v), Format(v), p, s)
	}
	d, err := d.Rescale(s)
	if err != nil {
		return nil, err
	}
	if d.Digits() > p {
		return nil, fmt.Errorf("bangdb: value %s does not fit DECIMAL(%d,%d)", d, p, s)
	}
	return d, nil
}
