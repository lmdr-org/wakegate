// Package promtext writes metrics in the Prometheus text format (version 0.0.4).
package promtext

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// ContentType is the Content-Type of a /metrics response.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Labels are the labels of one series.
type Labels map[string]string

// Writer collects metric families in the order they are declared, then writes them.
// It is built for one scrape and is not safe for concurrent use.
type Writer struct {
	families []*family
}

type family struct {
	name, typ, help string
	series          []series
}

type series struct {
	labels string
	value  float64
}

// Counter declares a counter and its series.
func (w *Writer) Counter(name, help string) *Family { return w.add(name, "counter", help) }

// Gauge declares a gauge and its series.
func (w *Writer) Gauge(name, help string) *Family { return w.add(name, "gauge", help) }

func (w *Writer) add(name, typ, help string) *Family {
	f := &family{name: name, typ: typ, help: help}
	w.families = append(w.families, f)
	return &Family{f: f}
}

// Family adds series to a declared metric.
type Family struct{ f *family }

// Set adds one series.
func (f *Family) Set(l Labels, v float64) *Family {
	f.f.series = append(f.f.series, series{labels: render(l), value: v})
	return f
}

// WriteTo writes every family; series are sorted by labels.
func (w *Writer) WriteTo(out io.Writer) (int64, error) {
	var b strings.Builder
	for _, f := range w.families {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", f.name, helpEscaper.Replace(f.help), f.name, f.typ)
		sort.SliceStable(f.series, func(i, j int) bool { return f.series[i].labels < f.series[j].labels })
		for _, s := range f.series {
			fmt.Fprintf(&b, "%s%s %s\n", f.name, s.labels, formatValue(s.value))
		}
	}
	n, err := io.WriteString(out, b.String())
	return int64(n), err
}

// Bool is 1 for true, 0 for false.
func Bool(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func render(l Labels) string {
	if len(l) == 0 {
		return ""
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + `="` + valueEscaper.Replace(l[k]) + `"`
	}
	return "{" + strings.Join(parts, ",") + "}"
}

var (
	valueEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
)

func formatValue(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
