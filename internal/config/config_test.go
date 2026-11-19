package config

import (
	"strings"
	"testing"
	"time"
)

func TestExpandEnvKeepsTemplateVars(t *testing.T) {
	t.Setenv("SHELF_TEST", "secret")
	in := `key: ${SHELF_TEST} other: ${SHELF_MISSING:-fallback} tpl: '{{$x := .Title}}{{$x}}'`
	got := ExpandEnv(in)
	want := `key: secret other: fallback tpl: '{{$x := .Title}}{{$x}}'`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestParseDefaultsAndRuleInheritance(t *testing.T) {
	c, err := Parse([]byte(`
action: copy
stable_for: 30s
min_size: 50MB
rules:
  - watch: /in
    output: /out
  - name: tv
    watch: /in2
    output: /out2
    action: move
    type: tv
    templates:
      episode: '{{.Show}}/{{epcode .}}{{.Ext}}'
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.StableFor.D() != 30*time.Second || c.MinSize != 50_000_000 {
		t.Errorf("durations/sizes: %v %v", c.StableFor.D(), c.MinSize)
	}
	r0, r1 := c.Rules[0], c.Rules[1]
	if r0.Name != "rule1" || r0.Action != "copy" || r0.ReviewDir != "/out/.shelf-review" {
		t.Errorf("rule0 defaults wrong: %+v", r0)
	}
	if r1.Action != "move" || r1.Renderer() == nil || !r1.IsRecursive() {
		t.Errorf("rule1 wrong: %+v", r1)
	}
}

func TestValidateErrors(t *testing.T) {
	_, err := Parse([]byte(`
action: teleport
rules:
  - watch: relative/path
    output: /out
    templates: { movie: '{{.Title' }
`))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"action", "absolute", "movie template"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	if _, err := Parse([]byte("acton: move\n")); err == nil {
		t.Error("typo'd field should be rejected")
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"0": 0, "1024": 1024, "50MB": 50e6, "1.5GiB": 1610612736, "700 mb": 700e6} {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
}
