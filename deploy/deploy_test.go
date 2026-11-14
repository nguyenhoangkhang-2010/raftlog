package deploy

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

type panel struct {
	ID      int                      `json:"id"`
	Title   string                   `json:"title"`
	GridPos struct{ X, Y, W, H int } `json:"gridPos"`
	Datasrc struct{ UID string }     `json:"datasource"`
	Targets []struct {
		Expr string `json:"expr"`
	} `json:"targets"`
}

type dashboard struct {
	UID    string  `json:"uid"`
	Title  string  `json:"title"`
	Panels []panel `json:"panels"`
}

// The dashboard may ask only for metrics that the nodes serve: a typo would show an empty panel and look like
// a healthy cluster.
func TestTheDashboardAsksOnlyForMetricsThatTheNodesServe(t *testing.T) {
	var d dashboard
	if err := json.Unmarshal([]byte(read(t, "grafana/dashboards/raftlog.json")), &d); err != nil {
		t.Fatalf("the dashboard is not JSON: %v", err)
	}
	if d.UID == "" || d.Title == "" || len(d.Panels) == 0 {
		t.Fatalf("a dashboard needs a uid, a title and panels: %+v", d)
	}
	served := map[string]bool{}
	src := read(t, "../internal/observe/observe.go")
	for _, m := range regexp.MustCompile(`"((?:raftlog|go|process)_[a-z_]+)"`).FindAllStringSubmatch(src, -1) {
		served[m[1]] = true
	}
	if len(served) < 20 {
		t.Fatalf("found only %d metrics in observe.go: the pattern is wrong", len(served))
	}
	ids := map[int]bool{}
	used := map[string]bool{}
	for _, p := range d.Panels {
		if ids[p.ID] {
			t.Errorf("panel id %d is used twice", p.ID)
		}
		ids[p.ID] = true
		if p.Title == "" || p.GridPos.W <= 0 || p.GridPos.X < 0 || p.GridPos.X+p.GridPos.W > 24 {
			t.Errorf("panel %q does not fit the grid: %+v", p.Title, p.GridPos)
		}
		if p.Datasrc.UID != "prometheus" {
			t.Errorf("panel %q: data source %q", p.Title, p.Datasrc.UID)
		}
		if len(p.Targets) == 0 {
			t.Errorf("panel %q asks for nothing", p.Title)
		}
		for _, tg := range p.Targets {
			names := regexp.MustCompile(`\b(?:raftlog|go|process)_[a-z_]+`).FindAllString(tg.Expr, -1)
			// "up" is made by Prometheus itself for every target it scrapes, not served by a node
			if len(names) == 0 && !strings.Contains(tg.Expr, `up{job="raftlog"}`) {
				t.Errorf("panel %q: no metric in %q", p.Title, tg.Expr)
			}
			for _, n := range names {
				base := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(n, "_bucket"), "_sum"), "_count")
				if !served[n] && !served[base] {
					t.Errorf("panel %q: %q is not a metric of the nodes", p.Title, n)
				}
				used[base] = true
			}
		}
	}
	// the questions that matter during a failover are all on the page
	for _, want := range []string{"raftlog_raft_is_leader", "raftlog_raft_term", "raftlog_raft_elections_started_total",
		"raftlog_raft_follower_lag_entries", "raftlog_request_duration_seconds", "raftlog_produced_messages_total"} {
		if !used[want] {
			names := make([]string, 0, len(used))
			for n := range used {
				names = append(names, n)
			}
			sort.Strings(names)
			t.Errorf("the dashboard does not show %s (it shows %v)", want, names)
		}
	}
}

// The data source that Grafana is given must be the one the dashboard names, and must reach the Prometheus of
// the Compose file, which must scrape the three nodes.
func TestGrafanaPrometheusAndTheNodesAgree(t *testing.T) {
	ds := read(t, "grafana/provisioning/datasources/prometheus.yml")
	if !strings.Contains(ds, "uid: prometheus") || !strings.Contains(ds, "url: http://prometheus:9090") {
		t.Errorf("the data source:\n%s", ds)
	}
	prom := read(t, "prometheus/prometheus.yml")
	for _, n := range []string{"node1:2112", "node2:2112", "node3:2112"} {
		if !strings.Contains(prom, `"`+n+`"`) {
			t.Errorf("Prometheus does not scrape %s", n)
		}
	}
	if !strings.Contains(prom, "job_name: raftlog") {
		t.Error("the dashboard counts the nodes that are up with job=raftlog")
	}
	compose := read(t, "docker-compose.yml")
	if !strings.Contains(compose, "RAFTLOG_METRICS_LISTEN: \":2112\"") {
		t.Error("the nodes do not serve their metrics on 2112")
	}
}

// Nothing of the demonstration may be reachable from another machine: the ports are published on the loopback
// address, because the passwords of a demonstration are well known and the console can write to the cluster.
func TestEveryPublishedPortIsOnTheLoopbackAddress(t *testing.T) {
	compose := read(t, "docker-compose.yml")
	found := 0
	for _, line := range strings.Split(compose, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || !strings.HasPrefix(trimmed, "ports:") {
			continue
		}
		found++
		if !strings.Contains(trimmed, `"127.0.0.1:`) {
			t.Errorf("a port is published to every address: %s", trimmed)
		}
	}
	if found < 6 {
		t.Errorf("found %d published ports, want one for each of the 3 nodes, the service, the console, Prometheus and Grafana", found)
	}
}

// The build context goes to the Docker daemon and is copied into the build stage: it must not carry the
// files that hold secrets or the data of a run.
func TestTheBuildContextLeavesOutSecretsAndData(t *testing.T) {
	ignore := read(t, "../.dockerignore")
	for _, want := range []string{".env", ".env.*", "data", "console-data", ".git"} {
		found := false
		for _, line := range strings.Split(ignore, "\n") {
			if strings.TrimSpace(line) == want {
				found = true
			}
		}
		if !found {
			t.Errorf(".dockerignore does not leave out %q", want)
		}
	}
}
