// Package metrics exposes numbers about a running process in the Prometheus text
// format, without any dependency.
//
// There are three kinds of metric, as in Prometheus: a counter only goes up, a
// gauge goes up and down, and a histogram counts observations into buckets. Each
// can have labels. Gauges that are only worth reading at the moment of a scrape (the
// role of a Raft node, the size of a log) are produced by a collector function that
// the registry calls when it is asked for the text.
//
// Names and labels are checked when a metric is created, and a mistake there (a bad
// name, a name used twice) panics: it is a mistake in the code, found the first time
// the program starts, not something to handle at run time.
package metrics

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

var (
	nameRE  = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// Kind is the type of a metric family.
type Kind string

// The kinds.
const (
	KindCounter   Kind = "counter"
	KindGauge     Kind = "gauge"
	KindHistogram Kind = "histogram"
)

// Label is one name and value.
type Label struct{ Name, Value string }

// Sample is one line of the output.
type Sample struct {
	Suffix string // "", "_bucket", "_sum" or "_count"
	Labels []Label
	Value  float64
}

// Family is a metric with all its samples.
type Family struct {
	Name    string
	Help    string
	Kind    Kind
	Samples []Sample
}

type collector interface {
	collect() []Family
}

// Registry holds metrics and writes them out.
type Registry struct {
	mu         sync.Mutex
	names      map[string]bool
	collectors []collector
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{names: map[string]bool{}} }

func (r *Registry) reserve(name string) {
	if !nameRE.MatchString(name) {
		panic(fmt.Sprintf("metrics: %q is not a valid metric name", name))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.names[name] {
		panic(fmt.Sprintf("metrics: %q is registered twice", name))
	}
	r.names[name] = true
}

func (r *Registry) add(c collector) {
	r.mu.Lock()
	r.collectors = append(r.collectors, c)
	r.mu.Unlock()
}

func checkLabels(names []string) {
	seen := map[string]bool{}
	for _, l := range names {
		if !labelRE.MatchString(l) || strings.HasPrefix(l, "__") || l == "le" {
			panic(fmt.Sprintf("metrics: %q is not a valid label name", l))
		}
		if seen[l] {
			panic(fmt.Sprintf("metrics: label %q is used twice", l))
		}
		seen[l] = true
	}
}

// ---- children of a labelled metric -------------------------------------------

// vec stores one child per combination of label values.
type vec[T any] struct {
	name     string
	help     string
	kind     Kind
	labels   []string
	newChild func() *T

	mu       sync.Mutex
	children map[string]*child[T]
}

type child[T any] struct {
	values []string
	v      *T
}

func newVec[T any](r *Registry, name, help string, kind Kind, labels []string, mk func() *T) *vec[T] {
	r.reserve(name)
	checkLabels(labels)
	v := &vec[T]{name: name, help: help, kind: kind, labels: labels, newChild: mk, children: map[string]*child[T]{}}
	return v
}

// with returns the child for the label values, creating it on first use.
func (v *vec[T]) with(values []string) *T {
	if len(values) != len(v.labels) {
		panic(fmt.Sprintf("metrics: %s takes %d label values, got %d", v.name, len(v.labels), len(values)))
	}
	key := strings.Join(values, "\x00")
	v.mu.Lock()
	defer v.mu.Unlock()
	c, ok := v.children[key]
	if !ok {
		c = &child[T]{values: append([]string(nil), values...), v: v.newChild()}
		v.children[key] = c
	}
	return c.v
}

// sorted returns the children in a stable order, so that the output is the same
// every time.
func (v *vec[T]) sorted() []*child[T] {
	v.mu.Lock()
	out := make([]*child[T], 0, len(v.children))
	for _, c := range v.children {
		out = append(out, c)
	}
	v.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return strings.Join(out[i].values, "\x00") < strings.Join(out[j].values, "\x00") })
	return out
}

func (v *vec[T]) labelsFor(values []string) []Label {
	out := make([]Label, len(values))
	for i, val := range values {
		out[i] = Label{v.labels[i], val}
	}
	return out
}

// ---- counter -----------------------------------------------------------------

// Counter only goes up.
type Counter struct{ n atomic.Uint64 }

// Inc adds one.
func (c *Counter) Inc() { c.n.Add(1) }

// Add adds n.
func (c *Counter) Add(n uint64) { c.n.Add(n) }

// Value returns the current count.
func (c *Counter) Value() uint64 { return c.n.Load() }

// CounterVec is a counter with labels.
type CounterVec struct{ v *vec[Counter] }

// NewCounterVec registers a labelled counter.
func (r *Registry) NewCounterVec(name, help string, labels ...string) *CounterVec {
	cv := &CounterVec{v: newVec(r, name, help, KindCounter, labels, func() *Counter { return &Counter{} })}
	r.add(cv)
	return cv
}

// NewCounter registers a counter without labels.
func (r *Registry) NewCounter(name, help string) *Counter {
	return r.NewCounterVec(name, help).With()
}

// With returns the counter for the label values.
func (cv *CounterVec) With(values ...string) *Counter { return cv.v.with(values) }

func (cv *CounterVec) collect() []Family {
	f := Family{Name: cv.v.name, Help: cv.v.help, Kind: KindCounter}
	for _, c := range cv.v.sorted() {
		f.Samples = append(f.Samples, Sample{Labels: cv.v.labelsFor(c.values), Value: float64(c.v.Value())})
	}
	return []Family{f}
}

// ---- gauge -------------------------------------------------------------------

// Gauge goes up and down.
type Gauge struct{ bits atomic.Uint64 }

// Set sets the value.
func (g *Gauge) Set(x float64) { g.bits.Store(math.Float64bits(x)) }

// Add adds x, which may be negative.
func (g *Gauge) Add(x float64) {
	for {
		old := g.bits.Load()
		if g.bits.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+x)) {
			return
		}
	}
}

// Inc adds one.
func (g *Gauge) Inc() { g.Add(1) }

// Dec subtracts one.
func (g *Gauge) Dec() { g.Add(-1) }

// Value returns the current value.
func (g *Gauge) Value() float64 { return math.Float64frombits(g.bits.Load()) }

// GaugeVec is a gauge with labels.
type GaugeVec struct{ v *vec[Gauge] }

// NewGaugeVec registers a labelled gauge.
func (r *Registry) NewGaugeVec(name, help string, labels ...string) *GaugeVec {
	gv := &GaugeVec{v: newVec(r, name, help, KindGauge, labels, func() *Gauge { return &Gauge{} })}
	r.add(gv)
	return gv
}

// NewGauge registers a gauge without labels.
func (r *Registry) NewGauge(name, help string) *Gauge {
	return r.NewGaugeVec(name, help).With()
}

// With returns the gauge for the label values.
func (gv *GaugeVec) With(values ...string) *Gauge { return gv.v.with(values) }

func (gv *GaugeVec) collect() []Family {
	f := Family{Name: gv.v.name, Help: gv.v.help, Kind: KindGauge}
	for _, c := range gv.v.sorted() {
		f.Samples = append(f.Samples, Sample{Labels: gv.v.labelsFor(c.values), Value: c.v.Value()})
	}
	return []Family{f}
}

// ---- histogram ---------------------------------------------------------------

// Histogram counts observations into buckets.
type Histogram struct {
	bounds []float64 // upper bounds, ascending; +Inf is implicit

	mu     sync.Mutex
	counts []uint64 // per bucket, not cumulative; the last is the +Inf bucket
	sum    float64
	n      uint64
}

func newHistogram(bounds []float64) *Histogram {
	return &Histogram{bounds: bounds, counts: make([]uint64, len(bounds)+1)}
}

// Observe records one value.
func (h *Histogram) Observe(x float64) {
	i := sort.SearchFloat64s(h.bounds, x) // first bound >= x: the bucket "le" that holds x
	h.mu.Lock()
	h.counts[i]++
	h.sum += x
	h.n++
	h.mu.Unlock()
}

// Snapshot returns the cumulative counts per bound (the last is the total), the
// sum and the count of observations.
func (h *Histogram) Snapshot() (cumulative []uint64, sum float64, count uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cumulative = make([]uint64, len(h.counts))
	var run uint64
	for i, c := range h.counts {
		run += c
		cumulative[i] = run
	}
	return cumulative, h.sum, h.n
}

// HistogramVec is a histogram with labels.
type HistogramVec struct {
	v      *vec[Histogram]
	bounds []float64
}

// DefaultLatencyBuckets are bucket bounds in seconds for request latencies, from
// half a millisecond to ten seconds.
var DefaultLatencyBuckets = []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// NewHistogramVec registers a labelled histogram. The bounds must be ascending.
func (r *Registry) NewHistogramVec(name, help string, bounds []float64, labels ...string) *HistogramVec {
	if len(bounds) == 0 || !sort.Float64sAreSorted(bounds) {
		panic(fmt.Sprintf("metrics: the buckets of %s must be ascending and not empty", name))
	}
	for i, b := range bounds {
		if math.IsInf(b, 0) || math.IsNaN(b) || (i > 0 && b == bounds[i-1]) {
			panic(fmt.Sprintf("metrics: bad bucket bound %v in %s", b, name))
		}
	}
	bs := append([]float64(nil), bounds...)
	hv := &HistogramVec{bounds: bs, v: newVec(r, name, help, KindHistogram, labels, func() *Histogram { return newHistogram(bs) })}
	r.add(hv)
	return hv
}

// NewHistogram registers a histogram without labels.
func (r *Registry) NewHistogram(name, help string, bounds []float64) *Histogram {
	return r.NewHistogramVec(name, help, bounds).With()
}

// With returns the histogram for the label values.
func (hv *HistogramVec) With(values ...string) *Histogram { return hv.v.with(values) }

func (hv *HistogramVec) collect() []Family {
	f := Family{Name: hv.v.name, Help: hv.v.help, Kind: KindHistogram}
	for _, c := range hv.v.sorted() {
		base := hv.v.labelsFor(c.values)
		cum, sum, n := c.v.Snapshot()
		for i, b := range hv.bounds {
			f.Samples = append(f.Samples, Sample{Suffix: "_bucket", Labels: append(append([]Label(nil), base...), Label{"le", formatValue(b)}), Value: float64(cum[i])})
		}
		f.Samples = append(f.Samples,
			Sample{Suffix: "_bucket", Labels: append(append([]Label(nil), base...), Label{"le", "+Inf"}), Value: float64(cum[len(cum)-1])},
			Sample{Suffix: "_sum", Labels: base, Value: sum},
			Sample{Suffix: "_count", Labels: base, Value: float64(n)},
		)
	}
	return []Family{f}
}

// ---- collector functions -------------------------------------------------------

type funcCollector struct{ fn func() []Family }

func (f funcCollector) collect() []Family { return f.fn() }

// AddCollector registers a function that produces metrics when the text is
// requested. Use it for values that are cheaper to read when asked than to keep up to
// date, such as the state of a Raft node. The names it produces are reserved so that
// nothing else can take them; the function must return the same names every time.
func (r *Registry) AddCollector(names []string, fn func() []Family) {
	for _, n := range names {
		r.reserve(n)
	}
	r.add(funcCollector{fn})
}

// ---- output ------------------------------------------------------------------

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

func escapeHelp(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s)
}

func escapeLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// WriteText writes every metric in the Prometheus text format, sorted by name.
func (r *Registry) WriteText(w io.Writer) error {
	r.mu.Lock()
	cs := append([]collector(nil), r.collectors...)
	r.mu.Unlock()

	var fams []Family
	for _, c := range cs {
		fams = append(fams, c.collect()...)
	}
	sort.SliceStable(fams, func(i, j int) bool { return fams[i].Name < fams[j].Name })

	var b strings.Builder
	for _, f := range fams {
		if len(f.Samples) == 0 {
			continue // a metric that has not been touched yet has nothing to say
		}
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", f.Name, escapeHelp(f.Help), f.Name, f.Kind)
		for _, s := range f.Samples {
			b.WriteString(f.Name + s.Suffix)
			if len(s.Labels) > 0 {
				b.WriteByte('{')
				for i, l := range s.Labels {
					if i > 0 {
						b.WriteByte(',')
					}
					fmt.Fprintf(&b, `%s="%s"`, l.Name, escapeLabel(l.Value))
				}
				b.WriteByte('}')
			}
			b.WriteString(" " + formatValue(s.Value) + "\n")
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// Handler serves the metrics for a Prometheus scrape.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_ = r.WriteText(w)
	})
}
