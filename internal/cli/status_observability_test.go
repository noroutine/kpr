package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/keeper"
)

// status observability lists the same backends the console
// links: name, URL, blurb. Unconfigured reads unavailable in
// words, never the raw code and never an empty list. If this
// fails, the CLI and the launchpad disagree.
func TestStatusObservabilityListsBackends(t *testing.T) {
	card := keeper.FetchObsCard(keeper.ObsURLs{Quickwit: "http://qw:8080"})
	var buf bytes.Buffer
	if err := renderObsCard(&buf, card, false); err != nil {
		t.Fatalf("renderObsCard: %v", err)
	}
	if want := "Quickwit: http://qw:8080"; !strings.Contains(buf.String(), want) {
		t.Errorf("card missing %q:\n%s", want, buf.String())
	}
	empty := keeper.FetchObsCard(keeper.ObsURLs{})
	buf.Reset()
	if err := renderObsCard(&buf, empty, false); err != nil {
		t.Fatalf("renderObsCard: %v", err)
	}
	if want := "observability: unavailable — no backends configured"; !strings.Contains(buf.String(), want) {
		t.Errorf("empty card missing %q:\n%s", want, buf.String())
	}
	if strings.Contains(buf.String(), "standing:") {
		t.Errorf("empty card leaks the raw code:\n%s", buf.String())
	}
}

// The command resolves its URLs from the environment at run
// time, like every other command wires through OpenDeps —
// reading the installed global raw would see the empty
// default and cry unavailable beside a linked console. If
// this fails, the CLI and serve reason about different
// configs.
func TestStatusObservabilityRunEReadsEnv(t *testing.T) {
	t.Setenv("KPR_QUICKWIT_URL", "http://qw:8080")
	prev := config.Current()
	t.Cleanup(func() { config.SetCurrent(prev) })
	var buf bytes.Buffer
	statusObservabilityCmd.SetOut(&buf)
	defer statusObservabilityCmd.SetOut(nil)
	if err := statusObservabilityCmd.RunE(statusObservabilityCmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if want := "Quickwit: http://qw:8080"; !strings.Contains(buf.String(), want) {
		t.Errorf("RunE missing %q:\n%s", want, buf.String())
	}
}

// status observability --json is the card struct verbatim:
// machine readers get the standing code, not the words. If
// this fails, --json drifts from the model both renderers
// sign.
func TestStatusObservabilityJSONIsCardVerbatim(t *testing.T) {
	card := keeper.FetchObsCard(keeper.ObsURLs{Grafana: "http://gf:3000"})
	var buf bytes.Buffer
	if err := renderObsCard(&buf, card, true); err != nil {
		t.Fatalf("renderObsCard: %v", err)
	}
	var got struct {
		Standing string `json:"standing"`
		Links    []struct {
			Name string `json:"name"`
		} `json:"links"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Standing != "ok" || len(got.Links) != 1 || got.Links[0].Name != "Grafana" {
		t.Errorf("json = %+v, want ok Grafana card", got)
	}
}
