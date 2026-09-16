package provider

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// Adaptation notes:
// - Ported from internal/promtext/lint.go to package provider as test helper.
// - Tests Prometheus classic text exposition format for hand-crafted metrics output.

var metricNameRegex = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
var labelNameRegex = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

var classicMetricTypes = map[string]bool{
	"counter": true, "gauge": true, "histogram": true, "summary": true, "untyped": true,
}

// Lint returns every problem found in body. An empty result means Prometheus
// parses the whole scrape and promtool's naming checks pass.
func Lint(body string) []error {
	var errs []error
	fail := func(line int, format string, args ...any) {
		errs = append(errs, fmt.Errorf("line %d: %s", line, fmt.Sprintf(format, args...)))
	}

	types := map[string]string{}
	helps := map[string]bool{}
	closed := map[string]bool{}
	series := map[string]bool{}
	current := ""

	// enter moves the cursor to family, closing the previous one. A family
	// seen again after it was closed is split, which the format forbids.
	enter := func(line int, family string) {
		if family == current {
			return
		}
		if closed[family] {
			fail(line, "metric family %s is split: its lines are not contiguous", family)
		}
		if current != "" {
			closed[current] = true
		}
		current = family
	}

	for i, raw := range strings.Split(body, "\n") {
		n := i + 1
		if raw == "" {
			continue
		}
		if !utf8.ValidString(raw) {
			fail(n, "invalid UTF-8")
			continue
		}
		if strings.HasPrefix(raw, "#") {
			fields := strings.Fields(raw)
			if len(fields) < 3 || (fields[1] != "HELP" && fields[1] != "TYPE") {
				continue // free-form comment
			}
			name := fields[2]
			if !metricNameRegex.MatchString(name) {
				fail(n, "invalid metric name %q", name)
				continue
			}
			enter(n, name)
			if fields[1] == "HELP" {
				if helps[name] {
					fail(n, "second HELP for %s", name)
				}
				helps[name] = true
				continue
			}
			if len(fields) != 4 {
				fail(n, "malformed TYPE line")
				continue
			}
			if _, dup := types[name]; dup {
				fail(n, "second TYPE for %s", name)
			}
			typ := fields[3]
			if !classicMetricTypes[typ] {
				fail(n, "type %q for %s is not a classic text-format type", typ, name)
			}
			types[name] = typ
			if typ == "counter" && !strings.HasSuffix(name, "_total") {
				fail(n, "counter %s should end in _total", name)
			}
			if typ != "counter" && strings.HasSuffix(name, "_total") {
				fail(n, "non-counter %s should not end in _total", name)
			}
			continue
		}

		name, labels, rest, err := splitSample(raw)
		if err != nil {
			fail(n, "%v", err)
			continue
		}
		family := familyOf(name, types)
		if _, ok := types[family]; !ok {
			fail(n, "sample %s appears before its TYPE line", name)
		}
		enter(n, family)

		fields := strings.Fields(rest)
		if len(fields) < 1 || len(fields) > 2 {
			fail(n, "expected a value and optional timestamp, got %q", rest)
			continue
		}
		if _, err := parseValue(fields[0]); err != nil {
			fail(n, "invalid value %q", fields[0])
		}
		if len(fields) == 2 {
			if _, err := strconv.ParseInt(fields[1], 10, 64); err != nil {
				fail(n, "invalid timestamp %q", fields[1])
			}
		}
		key := name + "{" + labels + "}"
		if series[key] {
			fail(n, "duplicate series %s", key)
		}
		series[key] = true
	}
	return errs
}

// familyOf maps a histogram or summary child sample to its family name.
func familyOf(name string, types map[string]string) string {
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		base, ok := strings.CutSuffix(name, suffix)
		if !ok {
			continue
		}
		if t := types[base]; t == "histogram" || t == "summary" {
			return base
		}
	}
	return name
}

func parseValue(s string) (float64, error) {
	switch s {
	case "NaN":
		return math.NaN(), nil
	case "+Inf":
		return math.Inf(1), nil
	case "-Inf":
		return math.Inf(-1), nil
	}
	return strconv.ParseFloat(s, 64)
}

// splitSample parses `name{a="x",b="y"} rest` and returns a canonical label
// string for duplicate detection. Label values may use only the three
// escapes the format defines.
func splitSample(line string) (name, labels, rest string, err error) {
	end := strings.IndexAny(line, "{ ")
	if end < 0 {
		return "", "", "", fmt.Errorf("sample has no value: %q", line)
	}
	name = line[:end]
	if !metricNameRegex.MatchString(name) {
		return "", "", "", fmt.Errorf("invalid metric name %q", name)
	}
	if line[end] == ' ' {
		return name, "", line[end+1:], nil
	}

	var canon []string
	seen := map[string]bool{}
	i := end + 1
	for {
		if i < len(line) && line[i] == '}' {
			i++
			break
		}
		eq := strings.IndexByte(line[i:], '=')
		if eq < 0 {
			return "", "", "", fmt.Errorf("unterminated label set in %s", name)
		}
		lname := line[i : i+eq]
		if !labelNameRegex.MatchString(lname) {
			return "", "", "", fmt.Errorf("invalid label name %q in %s", lname, name)
		}
		if seen[lname] {
			return "", "", "", fmt.Errorf("label %s repeated in %s", lname, name)
		}
		seen[lname] = true
		i += eq + 1
		if i >= len(line) || line[i] != '"' {
			return "", "", "", fmt.Errorf("label %s value is not quoted in %s", lname, name)
		}
		i++
		var val strings.Builder
		closedQuote := false
		for i < len(line) {
			c := line[i]
			if c == '"' {
				closedQuote = true
				i++
				break
			}
			if c == '\\' {
				if i+1 >= len(line) {
					break
				}
				switch line[i+1] {
				case '\\', '"', 'n':
					val.WriteByte(line[i+1])
				default:
					return "", "", "", fmt.Errorf("label %s in %s uses escape \\%c, which the text format does not define", lname, name, line[i+1])
				}
				i += 2
				continue
			}
			val.WriteByte(c)
			i++
		}
		if !closedQuote {
			return "", "", "", fmt.Errorf("unterminated label value for %s in %s", lname, name)
		}
		canon = append(canon, lname+"="+strconv.Quote(val.String()))
		if i < len(line) && line[i] == ',' {
			i++
		}
	}
	if i >= len(line) || line[i] != ' ' {
		return "", "", "", fmt.Errorf("expected a space after the label set of %s", name)
	}
	return name, strings.Join(canon, ","), line[i+1:], nil
}

func TestPromtextLintCatchesKnownBreakage(t *testing.T) {
	cases := map[string]string{
		"info type":      "# TYPE x_info info\nx_info{version=\"1\"} 1\n",
		"split family":   "# TYPE a gauge\na 1\n# TYPE b gauge\nb 1\na{x=\"y\"} 2\n",
		"go escape":      "# TYPE a gauge\na{x=\"\\t\"} 1\n",
		"gauge _total":   "# TYPE a_total gauge\na_total 1\n",
		"counter suffix": "# TYPE a counter\na 1\n",
		"no type":        "a 1\n",
		"duplicate":      "# TYPE a gauge\na{x=\"1\"} 1\na{x=\"1\"} 2\n",
	}
	for name, body := range cases {
		if len(Lint(body)) == 0 {
			t.Errorf("%s: Lint found nothing in %q", name, body)
		}
	}
	good := "# HELP a_total ok\n# TYPE a_total counter\na_total{x=\"q\\\"\\\\\\n\"} 1\n# TYPE h histogram\nh_bucket{le=\"+Inf\"} 1\nh_sum 1\nh_count 1\n"
	for _, err := range Lint(good) {
		t.Errorf("valid exposition flagged: %v", err)
	}
}
