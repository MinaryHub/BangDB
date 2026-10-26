package engine

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/knoxxr/bangdb/internal/store"
	"github.com/knoxxr/bangdb/internal/value"
)

// Date and time functions in the style of SQLite. Times are text in
// "YYYY-MM-DD HH:MM:SS" (UTC unless the 'localtime' modifier is used).
//
//	date(time, modifiers...)      -> 'YYYY-MM-DD'
//	time(time, modifiers...)      -> 'HH:MM:SS'
//	datetime(time, modifiers...)  -> 'YYYY-MM-DD HH:MM:SS'
//	strftime(format, time, ...)   -> formatted text (%Y %m %d %H %M %S %f %j %w %W %s %%)
//	julianday(time, ...)          -> REAL
//	unixepoch(time, ...)          -> INTEGER seconds
//
// time is 'now', an ISO-8601 date/time ('2026-10-25', '2026-10-25 13:45',
// '2026-10-25T13:45:00.123Z', ...), or a number (seconds with the
// 'unixepoch' modifier, otherwise a Julian day). Modifiers: '+N days',
// '-N hours', '+N minutes', '+N seconds', '+N months', '+N years',
// 'start of day', 'start of month', 'start of year', 'unixepoch',
// 'localtime', 'utc', 'weekday N'.

func init() {
	for name, f := range map[string]func([]any) (any, error){
		"DATE":      func(a []any) (any, error) { return dateFn(a, "2006-01-02") },
		"TIME":      func(a []any) (any, error) { return dateFn(a, "15:04:05") },
		"DATETIME":  func(a []any) (any, error) { return dateFn(a, "2006-01-02 15:04:05") },
		"JULIANDAY": fnJulianDay,
		"UNIXEPOCH": fnUnixEpoch,
		"STRFTIME":  fnStrftime,
	} {
		min := 0
		if name == "STRFTIME" {
			min = 1
		}
		scalarFuncs[name] = scalarFunc{minArgs: min, maxArgs: -1, fn: f}
	}
	scalarFuncs["TIME_BUCKET"] = scalarFunc{minArgs: 2, maxArgs: 3, fn: fnTimeBucket}
}

// fnTimeBucket implements time_bucket(width, t [, unit]): the start of the
// bucket of the given width containing t. For an epoch number t, width is a
// number of the same unit or an interval such as '5 minutes' (unit: 's',
// 'ms' or 'us'; default 's'). For a date/time text t, width is an interval
// and the result is a datetime text.
func fnTimeBucket(a []any) (any, error) {
	if a[0] == nil || a[1] == nil {
		return nil, nil
	}
	unit := "s"
	if len(a) == 3 {
		u, ok := a[2].(string)
		if !ok {
			return nil, fmt.Errorf("bangdb: time_bucket unit must be 's', 'ms' or 'us'")
		}
		unit = strings.ToLower(u)
	}
	width := func(unit string) (int64, error) {
		var w int64
		var err error
		switch x := a[0].(type) {
		case int64:
			w = x
		case float64:
			if x != math.Trunc(x) {
				return 0, fmt.Errorf("bangdb: time_bucket width must be a whole number")
			}
			w = int64(x)
		case string:
			w, err = store.ParseInterval(x, unit)
		default:
			err = fmt.Errorf("bangdb: invalid time_bucket width")
		}
		if err == nil && w <= 0 {
			err = fmt.Errorf("bangdb: time_bucket width must be positive")
		}
		return w, err
	}
	floor := func(t, w int64) int64 {
		b := t - t%w
		if t%w < 0 {
			b -= w
		}
		return b
	}
	switch t := a[1].(type) {
	case int64:
		w, err := width(unit)
		if err != nil {
			return nil, err
		}
		return floor(t, w), nil
	case float64:
		w, err := width(unit)
		if err != nil {
			return nil, err
		}
		return math.Floor(t/float64(w)) * float64(w), nil
	case string:
		tm, ok := parseTime(t)
		if !ok {
			return nil, nil
		}
		w, err := width("us")
		if err != nil {
			return nil, err
		}
		us := floor(tm.UnixMicro(), w)
		out := time.UnixMicro(us).UTC()
		if us%1e6 == 0 {
			return out.Format("2006-01-02 15:04:05"), nil
		}
		return strings.TrimRight(out.Format("2006-01-02 15:04:05.000000"), "0"), nil
	}
	return nil, nil
}

func dateFn(a []any, layout string) (any, error) {
	t, ok, err := evalTime(a)
	if err != nil || !ok {
		return nil, err
	}
	return t.Format(layout), nil
}

func fnJulianDay(a []any) (any, error) {
	t, ok, err := evalTime(a)
	if err != nil || !ok {
		return nil, err
	}
	return julianDay(t), nil
}

func fnUnixEpoch(a []any) (any, error) {
	t, ok, err := evalTime(a)
	if err != nil || !ok {
		return nil, err
	}
	return t.Unix(), nil
}

func fnStrftime(a []any) (any, error) {
	if a[0] == nil {
		return nil, nil
	}
	t, ok, err := evalTime(a[1:])
	if err != nil || !ok {
		return nil, err
	}
	format := value.Format(a[0])
	var sb strings.Builder
	for i := 0; i < len(format); i++ {
		c := format[i]
		if c != '%' || i+1 == len(format) {
			sb.WriteByte(c)
			continue
		}
		i++
		switch format[i] {
		case 'Y':
			fmt.Fprintf(&sb, "%04d", t.Year())
		case 'm':
			fmt.Fprintf(&sb, "%02d", int(t.Month()))
		case 'd':
			fmt.Fprintf(&sb, "%02d", t.Day())
		case 'H':
			fmt.Fprintf(&sb, "%02d", t.Hour())
		case 'M':
			fmt.Fprintf(&sb, "%02d", t.Minute())
		case 'S':
			fmt.Fprintf(&sb, "%02d", t.Second())
		case 'f':
			fmt.Fprintf(&sb, "%06.3f", float64(t.Second())+float64(t.Nanosecond())/1e9)
		case 'j':
			fmt.Fprintf(&sb, "%03d", t.YearDay())
		case 'w':
			fmt.Fprintf(&sb, "%d", int(t.Weekday()))
		case 'W':
			// Week of the year, weeks starting on Monday (00-53).
			fmt.Fprintf(&sb, "%02d", (t.YearDay()+6-(int(t.Weekday())+6)%7)/7)
		case 's':
			fmt.Fprintf(&sb, "%d", t.Unix())
		case 'J':
			sb.WriteString(strconv.FormatFloat(julianDay(t), 'f', -1, 64))
		case '%':
			sb.WriteByte('%')
		default:
			return nil, fmt.Errorf("bangdb: strftime: unknown format %%%c", format[i])
		}
	}
	return sb.String(), nil
}

func julianDay(t time.Time) float64 {
	return float64(t.UnixNano())/86400e9 + 2440587.5
}

// evalTime parses the time argument and applies the modifiers. ok=false
// means the result is NULL (missing or unparseable input).
func evalTime(a []any) (time.Time, bool, error) {
	var mods []string
	for _, m := range a[min(1, len(a)):] {
		if m == nil {
			return time.Time{}, false, nil
		}
		mods = append(mods, strings.ToLower(strings.TrimSpace(value.Format(m))))
	}
	unixMod := false
	for _, m := range mods {
		unixMod = unixMod || m == "unixepoch"
	}
	var t time.Time
	if len(a) == 0 {
		t = time.Now().UTC()
	} else {
		switch v := a[0].(type) {
		case nil:
			return time.Time{}, false, nil
		case int64, float64:
			f := toFloat(v)
			if unixMod {
				sec, frac := math.Modf(f)
				t = time.Unix(int64(sec), int64(frac*1e9)).UTC()
			} else { // Julian day number
				t = time.Unix(0, int64((f-2440587.5)*86400e9)).UTC()
			}
		default:
			var ok bool
			if t, ok = parseTime(value.Format(v)); !ok {
				return time.Time{}, false, nil
			}
		}
	}
	for _, m := range mods {
		var ok bool
		if t, ok = applyModifier(t, m); !ok {
			return time.Time{}, false, nil
		}
	}
	return t, true, nil
}

var timeLayouts = []string{
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04",
	"2006-01-02T15:04",
	"2006-01-02",
	"15:04:05.999999999",
	"15:04:05",
	"15:04",
	time.RFC3339Nano,
}

func parseTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if strings.EqualFold(s, "now") {
		return time.Now().UTC(), true
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), true
	}
	for _, l := range timeLayouts {
		if t, err := time.ParseInLocation(l, s, time.UTC); err == nil {
			if !strings.Contains(l, "2006") { // time only: date 2000-01-01, like SQLite
				t = t.AddDate(2000, 0, 0)
			}
			return t, true
		}
	}
	return time.Time{}, false
}

func applyModifier(t time.Time, m string) (time.Time, bool) {
	switch m {
	case "unixepoch":
		return t, true
	case "utc":
		return t.UTC(), true
	case "localtime":
		return t.Local(), true
	case "start of day":
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()), true
	case "start of month":
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location()), true
	case "start of year":
		return time.Date(t.Year(), 1, 1, 0, 0, 0, 0, t.Location()), true
	}
	if rest, ok := strings.CutPrefix(m, "weekday "); ok {
		n, err := strconv.Atoi(strings.TrimSpace(rest))
		if err != nil || n < 0 || n > 6 {
			return t, false
		}
		return t.AddDate(0, 0, (n-int(t.Weekday())+7)%7), true
	}
	fields := strings.Fields(m)
	if len(fields) != 2 {
		return t, false
	}
	n, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return t, false
	}
	unit := strings.TrimSuffix(fields[1], "s")
	switch unit {
	case "second":
		return t.Add(time.Duration(n * float64(time.Second))), true
	case "minute":
		return t.Add(time.Duration(n * float64(time.Minute))), true
	case "hour":
		return t.Add(time.Duration(n * float64(time.Hour))), true
	case "day":
		return t.Add(time.Duration(n * 24 * float64(time.Hour))), true
	case "month":
		return t.AddDate(0, int(n), 0), true
	case "year":
		return t.AddDate(int(n), 0, 0), true
	}
	return t, false
}
