package metrics

import (
	"math"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func text(t *testing.T, r *Registry) string {
	t.Helper()
	var b strings.Builder
	if err := r.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func mustPanic(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s: did not panic", what)
		}
	}()
	f()
}

func TestTheTextFormat(t *testing.T) {
	r := NewRegistry()
	reqs := r.NewCounterVec("app_requests_total", "Requests served.", "method", "code")
	reqs.With("Produce", "OK").Add(3)
	reqs.With("Fetch", "OK").Inc()
	reqs.With("Produce", "Unavailable").Inc()
	g := r.NewGauge("app_leader", "1 on the leader.")
	g.Set(1)
	r.NewGaugeVec("app_lag", "Lag.", "peer").With("2").Set(7.5)

	want := `# HELP app_lag Lag.
# TYPE app_lag gauge
app_lag{peer="2"} 7.5
# HELP app_leader 1 on the leader.
# TYPE app_leader gauge
app_leader 1
# HELP app_requests_total Requests served.
# TYPE app_requests_total counter
app_requests_total{method="Fetch",code="OK"} 1
app_requests_total{method="Produce",code="OK"} 3
app_requests_total{method="Produce",code="Unavailable"} 1
`
	if got := text(t, r); got != want {
		t.Fatalf("output:\n%s\nwant:\n%s", got, want)
	}
	// The same registry gives the same text every time.
	if text(t, r) != want {
		t.Fatal("the output is not stable")
	}
}

func TestACounterOnlyGoesUpAndIsSafeForManyGoroutines(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("n_total", "n")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				c.Inc()
			}
		}()
	}
	wg.Wait()
	c.Add(5)
	if c.Value() != 20_005 {
		t.Fatalf("counter = %d", c.Value())
	}
	if !strings.Contains(text(t, r), "n_total 20005\n") {
		t.Fatalf("text: %s", text(t, r))
	}
}

func TestAGaugeGoesUpAndDown(t *testing.T) {
	r := NewRegistry()
	g := r.NewGauge("g", "g")
	g.Set(10)
	g.Inc()
	g.Dec()
	g.Add(-2.5)
	if g.Value() != 7.5 {
		t.Fatalf("gauge = %v", g.Value())
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); g.Add(1) }()
	}
	wg.Wait()
	if g.Value() != 57.5 {
		t.Fatalf("after concurrent adds: %v", g.Value())
	}
}

func TestAHistogramCountsIntoBucketsWithInclusiveBounds(t *testing.T) {
	r := NewRegistry()
	h := r.NewHistogramVec("lat_seconds", "Latency.", []float64{0.1, 1}, "method").With("Produce")
	for _, x := range []float64{0.05, 0.1, 0.5, 1, 2, 3} {
		h.Observe(x)
	}
	// le=0.1 holds 0.05 and 0.1 (a bound is inclusive); le=1 adds 0.5 and 1; +Inf adds 2 and 3.
	want := `# HELP lat_seconds Latency.
# TYPE lat_seconds histogram
lat_seconds_bucket{method="Produce",le="0.1"} 2
lat_seconds_bucket{method="Produce",le="1"} 4
lat_seconds_bucket{method="Produce",le="+Inf"} 6
lat_seconds_sum{method="Produce"} 6.65
lat_seconds_count{method="Produce"} 6
`
	if got := text(t, r); got != want {
		t.Fatalf("output:\n%s\nwant:\n%s", got, want)
	}
	cum, sum, n := h.Snapshot()
	if len(cum) != 3 || cum[2] != 6 || n != 6 || math.Abs(sum-6.65) > 1e-9 {
		t.Fatalf("snapshot = %v %v %v", cum, sum, n)
	}
}

func TestAHistogramWithoutLabels(t *testing.T) {
	r := NewRegistry()
	r.NewHistogram("h", "h", DefaultLatencyBuckets).Observe(0.003)
	out := text(t, r)
	if !strings.Contains(out, `h_bucket{le="0.0025"} 0`) || !strings.Contains(out, `h_bucket{le="0.005"} 1`) || !strings.Contains(out, "h_count 1\n") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestLabelValuesAndHelpAreEscaped(t *testing.T) {
	r := NewRegistry()
	r.NewGaugeVec("g", "line one\nline \\ two", "path").With("a\"b\\c\nd").Set(1)
	want := "# HELP g line one\\nline \\\\ two\n# TYPE g gauge\ng{path=\"a\\\"b\\\\c\\nd\"} 1\n"
	if got := text(t, r); got != want {
		t.Fatalf("output:\n%q\nwant:\n%q", got, want)
	}
}

func TestSpecialValues(t *testing.T) {
	r := NewRegistry()
	r.NewGaugeVec("v", "v", "k").With("inf").Set(math.Inf(1))
	r.NewGaugeVec("v2", "v", "k").With("nan").Set(math.NaN())
	r.NewGaugeVec("v3", "v", "k").With("big").Set(1e21)
	out := text(t, r)
	for _, want := range []string{`v{k="inf"} +Inf`, `v2{k="nan"} NaN`, `v3{k="big"} 1e+21`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

func TestAMetricNobodyTouchedIsLeftOut(t *testing.T) {
	r := NewRegistry()
	r.NewCounterVec("never_total", "never", "a")
	if got := text(t, r); got != "" {
		t.Fatalf("output = %q, want nothing", got)
	}
}

func TestACollectorIsAskedAtEveryScrape(t *testing.T) {
	r := NewRegistry()
	calls := 0
	r.AddCollector([]string{"node_term"}, func() []Family {
		calls++
		return []Family{{Name: "node_term", Help: "Term.", Kind: KindGauge, Samples: []Sample{{Value: float64(calls)}}}}
	})
	if !strings.Contains(text(t, r), "node_term 1\n") || !strings.Contains(text(t, r), "node_term 2\n") {
		t.Fatal("the collector was not called at each scrape")
	}
	// Its name is taken.
	mustPanic(t, "a metric with a collector's name", func() { r.NewGauge("node_term", "x") })
}

func TestMistakesInTheCodePanicAtStartup(t *testing.T) {
	r := NewRegistry()
	r.NewCounter("ok_total", "x")
	mustPanic(t, "a name twice", func() { r.NewCounter("ok_total", "x") })
	mustPanic(t, "a bad name", func() { r.NewCounter("1bad", "x") })
	mustPanic(t, "a name with a dash", func() { r.NewGauge("a-b", "x") })
	mustPanic(t, "a bad label", func() { r.NewGaugeVec("g1", "x", "bad-label") })
	mustPanic(t, "a reserved label", func() { r.NewGaugeVec("g2", "x", "__name") })
	mustPanic(t, "the label le", func() { r.NewHistogramVec("h1", "x", []float64{1}, "le") })
	mustPanic(t, "a label twice", func() { r.NewGaugeVec("g3", "x", "a", "a") })
	mustPanic(t, "unsorted buckets", func() { r.NewHistogram("h2", "x", []float64{2, 1}) })
	mustPanic(t, "no buckets", func() { r.NewHistogram("h3", "x", nil) })
	mustPanic(t, "an infinite bound", func() { r.NewHistogram("h4", "x", []float64{1, math.Inf(1)}) })
	mustPanic(t, "a repeated bound", func() { r.NewHistogram("h5", "x", []float64{1, 1}) })
	cv := r.NewCounterVec("lab_total", "x", "a", "b")
	mustPanic(t, "the wrong number of label values", func() { cv.With("only one") })
}

func TestTheHandlerServesThePrometheusFormat(t *testing.T) {
	r := NewRegistry()
	r.NewCounter("hits_total", "Hits.").Add(2)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("%d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), "hits_total 2\n") {
		t.Fatalf("body: %s", rec.Body.String())
	}
}

func TestScrapingWhileMetricsChange(t *testing.T) {
	r := NewRegistry()
	cv := r.NewCounterVec("c_total", "c", "k")
	hv := r.NewHistogramVec("h", "h", []float64{1}, "k")
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		key := string(rune('a' + i))
		cv.With(key).Inc() // the children exist before the first scrape
		hv.With(key).Observe(0.5)
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					cv.With(key).Inc()
					hv.With(key).Observe(0.5)
				}
			}
		}(key)
	}
	// A histogram's count always equals its +Inf bucket, even when it is read
	// while being written: both come from one consistent snapshot.
	value := func(out, prefix string) string {
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, prefix) {
				return strings.TrimPrefix(line, prefix)
			}
		}
		return ""
	}
	for i := 0; i < 200; i++ {
		out := text(t, r)
		inf := value(out, `h_bucket{k="a",le="+Inf"} `)
		count := value(out, `h_count{k="a"} `)
		if inf == "" || inf != count {
			t.Fatalf("scrape %d: +Inf bucket %q, count %q\n%s", i, inf, count, out)
		}
	}
	close(stop)
	wg.Wait()
}
