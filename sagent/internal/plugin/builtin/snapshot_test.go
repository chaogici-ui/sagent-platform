package builtin

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sagent/core/internal/config"
)

var ParsePrometheusForTest = parsePrometheusText

func CollectMySQLForTest(target config.MySQLTarget, client *http.Client) []MetricBatch {
	m := NewMySQLProbe(config.MySQLProbeConfig{Targets: []config.MySQLTarget{target}})
	m.client = client
	out := make(chan MetricBatch, 4)
	m.scrape(out)
	close(out)
	var batches []MetricBatch
	for batch := range out {
		batches = append(batches, batch)
	}
	return batches
}

type probeTransport struct{ endpoint *url.URL }

func (p probeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	copy := req.Clone(req.Context())
	copy.URL.Scheme, copy.URL.Host = p.endpoint.Scheme, p.endpoint.Host
	return http.DefaultTransport.RoundTrip(copy)
}

func TestMySQLTargetSnapshots(t *testing.T) {
	var mu sync.Mutex
	status, body, truncated := http.StatusOK, "mysql_up 1\nmysql_threads 7\n", false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/probe" || r.URL.Query().Get("target") == "" {
			t.Errorf("wrong probe request: %s", r.URL)
		}
		if truncated {
			w.Header().Set("Content-Length", "999")
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	endpoint, _ := url.Parse(srv.URL)
	m := NewMySQLProbe(config.MySQLProbeConfig{Targets: []config.MySQLTarget{
		{ResourceID: "db-1", DSN: "u@tcp(db-1:3306)/"},
		{ResourceID: "db-2", DSN: "u@tcp(db-2:3306)/"},
	}})
	m.client = &http.Client{Transport: probeTransport{endpoint}, Timeout: time.Second}
	out := make(chan MetricBatch, 4)
	m.scrape(out)
	first, firstHealth, second, secondHealth := <-out, <-out, <-out, <-out
	if first.Err != nil || second.Err != nil || first.Source == second.Source || first.Metrics[0].Labels["resource_id"] != "db-1" || second.Metrics[0].Labels["resource_id"] != "db-2" {
		t.Fatalf("targets not isolated: %+v %+v", first, second)
	}
	if firstHealth.Source == secondHealth.Source || firstHealth.Metrics[0].Value != 1 || secondHealth.Metrics[0].Value != 1 {
		t.Fatalf("health targets not isolated: %+v %+v", firstHealth, secondHealth)
	}
	for _, tc := range []struct {
		name               string
		status             int
		body               string
		truncated, failure bool
	}{
		{"non2xx", 503, "mysql_up 999\n", false, true},
		{"parse", 200, "mysql_up invalid\n", false, true},
		{"read", 200, "mysql_up 1\n", true, true},
		{"empty", 200, "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			status, body, truncated = tc.status, tc.body, tc.truncated
			mu.Unlock()
			m.scrape(out)
			for i, source := range []string{first.Source, second.Source} {
				batch, health := <-out, <-out
				if batch.Source != source || (batch.Err != nil) != tc.failure || len(batch.Metrics) != 0 {
					t.Fatalf("bad target snapshot: %+v", batch)
				}
				_, healthSource := MySQLSnapshotSources(m.targets[i])
				if health.Source != healthSource || health.Err != nil || tc.failure && (len(health.Metrics) != 1 || health.Metrics[0].Value != 0) || !tc.failure && len(health.Metrics) != 0 {
					t.Fatalf("bad health snapshot: %+v", health)
				}
			}
		})
	}
	srv.Close()
	m.scrape(out)
	for range m.targets {
		batch, health := <-out, <-out
		if batch.Err == nil || health.Err != nil || len(health.Metrics) != 1 || health.Metrics[0].Value != 0 {
			t.Fatal("transport error accepted or health not updated")
		}
	}
}

func TestExecFullStdoutSnapshots(t *testing.T) {
	e := NewExecScripts(config.CustomScriptsConfig{})
	out := make(chan MetricBatch, 1)
	script := config.ExecScript{Name: "load", ResourceID: "remote", Interval: time.Second, Command: "printf 'node_load1 1\\nnode_load5 5\\n'"}
	e.executeOnce(script, out, time.Second)
	first := <-out
	if first.Err != nil || len(first.Metrics) != 2 || first.Metrics[0].Labels["resource_id"] != "remote" {
		t.Fatalf("invalid script batch: %+v", first)
	}
	for _, tc := range []struct {
		command string
		count   int
		failure bool
	}{
		{"printf 'node_load1 2\\n'", 1, false},
		{"true", 0, false},
		{"printf 'node_load1 9\\n'; exit 1", 0, true},
		{"printf 'node_load1 invalid\\n'", 0, true},
	} {
		script.Command = tc.command
		e.executeOnce(script, out, time.Second)
		batch := <-out
		if batch.Source != first.Source || len(batch.Metrics) != tc.count || (batch.Err != nil) != tc.failure || batch.Freshness != 3*time.Second {
			t.Fatalf("wrong script replacement: %+v", batch)
		}
	}
	script.ResourceID = "other"
	script.Command = "true"
	e.executeOnce(script, out, time.Second)
	if batch := <-out; batch.Source == first.Source {
		t.Fatal("script resources share a source")
	}
}

func TestPortUnreachableIsSuccessButInvalidTypeFails(t *testing.T) {
	p := NewPortChecker([]PortTarget{{Name: "down", Type: "tcp", Address: "invalid-address"}, {Name: "invalid", Type: "invalid"}}, time.Second)
	out := make(chan MetricBatch, 2)
	p.check(out)
	down, invalid := <-out, <-out
	if down.Err != nil || len(down.Metrics) != 2 || down.Metrics[0].Value != 0 {
		t.Fatalf("down is a valid probe snapshot: %+v", down)
	}
	if invalid.Err == nil || len(invalid.Metrics) != 0 || invalid.Source == down.Source {
		t.Fatalf("invalid type was successful: %+v", invalid)
	}
}

func TestSnapshotSendsRespondToStop(t *testing.T) {
	groups := map[string]bool{}
	for _, key := range HostMetricGroupKeys() {
		groups[key] = false
	}
	h := NewHostMetricsCollector(time.Second, groups, nil)
	s := NewScrapePlugin([]ScrapeTarget{{Name: "invalid", URL: ":invalid"}}, time.Second)
	p := NewPortChecker([]PortTarget{{Type: "tcp", Address: "invalid"}}, time.Second)
	e := NewExecScripts(config.CustomScriptsConfig{})
	m := NewMySQLProbe(config.MySQLProbeConfig{Targets: []config.MySQLTarget{{ResourceID: "invalid"}}})
	for _, tc := range []struct {
		name string
		run  func(chan<- MetricBatch)
		stop chan struct{}
	}{
		{"host", h.runOnce, h.stopCh}, {"scrape", s.scrape, s.stopCh}, {"port", p.check, p.stopCh},
		{"exec", func(ch chan<- MetricBatch) { e.executeOnce(config.ExecScript{Command: "true"}, ch, time.Second) }, e.stopCh},
		{"mysql", m.scrape, m.stopCh},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := make(chan MetricBatch, 1)
			out <- MetricBatch{}
			done := make(chan struct{})
			go func() { defer close(done); tc.run(out) }()
			close(tc.stop)
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("full channel blocked collector stop")
			}
		})
	}
}

func TestStrictPrometheusParsing(t *testing.T) {
	text := "# HELP node_load1 load\\nhelp\n# TYPE node_load1 gauge\n" + `node_load1{device="a, b\"c\\d\ne"} 1` + "\nnode_load5 2\n"
	metrics, err := parsePrometheusText(text)
	if err != nil || len(metrics) != 2 {
		t.Fatalf("valid body rejected: %+v %v", metrics, err)
	}
	if metrics[0].Labels["device"] != "a, b\"c\\d\ne" || metrics[0].Help != "load\nhelp" || metrics[1].Help != "" {
		t.Fatalf("labels/help corrupted: %+v", metrics)
	}
	for _, body := range []string{"node_load1 invalid", "node_load1 1\nbroken", `node_load1{a="unterminated} 1`, `node_load1{a="x",a="y"} 1`, `node_load1{a="bad\q"} 1`, "invalid-name 1"} {
		if metrics, err := parsePrometheusText(body); err == nil || len(metrics) != 0 {
			t.Errorf("malformed body accepted: %q %+v", body, metrics)
		}
	}
	if a, b := SnapshotSource("exec", "a:b", "c"), SnapshotSource("exec", "a", "b:c"); a == b || strings.Contains(a, "\n") {
		t.Fatal("source identity is ambiguous")
	}
}
