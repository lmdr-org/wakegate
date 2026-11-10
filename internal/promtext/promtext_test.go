package promtext

import (
	"strings"
	"testing"
)

func TestWrite(t *testing.T) {
	var w Writer
	w.Gauge("wakegate_target_up", "Whether the target answers.").Set(nil, 1)
	w.Counter("wakegate_requests_total", "Requests received.").
		Set(Labels{"service": "s3", "result": "relayed"}, 3).
		Set(Labels{"service": "ollama", "result": `a"b`}, 1.5)
	var b strings.Builder
	if _, err := w.WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	want := `# HELP wakegate_target_up Whether the target answers.
# TYPE wakegate_target_up gauge
wakegate_target_up 1
# HELP wakegate_requests_total Requests received.
# TYPE wakegate_requests_total counter
wakegate_requests_total{result="a\"b",service="ollama"} 1.5
wakegate_requests_total{result="relayed",service="s3"} 3
`
	if b.String() != want {
		t.Fatalf("got\n%s\nwant\n%s", b.String(), want)
	}
}
