package supervisor

import (
	"context"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/pricing"
)

type captureRunner struct {
	req   agent.Request
	text  string
	usage agent.Usage
}

func (c *captureRunner) ID() string { return agent.IDCodex }
func (c *captureRunner) Detect(context.Context) agent.Availability {
	return agent.Availability{Available: true}
}
func (c *captureRunner) SupportsWeb() bool { return false }
func (c *captureRunner) Run(_ context.Context, r agent.Request) (agent.Result, error) {
	c.req = r
	return agent.Result{Text: c.text, Usage: c.usage}, nil
}

func TestAgentModelUsesNoToolsAndValidatesStructuredOutput(t *testing.T) {
	r := &captureRunner{text: `{"diagnosis":"stalled","confidence":0.9,"evidence":["heartbeat"],"recommended_action":"terminate_requeue","human_approval_required":false,"suggested_retry_limit":2,"suggested_termination_limit":1}`}
	m := NewAgentModel(r, "gpt-test", t.TempDir(), time.Second, 4, pricing.Table{Version: "v1", Rates: map[string]pricing.Rate{"codex/gpt-test": {InputUSDPerMillion: 1}}})
	d, _, err := m.Diagnose(context.Background(), ModelContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !r.req.NoTools || r.req.MaxTurns != 4 || d.RecommendedAction != ActionTerminateRequeue {
		t.Fatalf("request=%+v decision=%+v", r.req, d)
	}
}

func TestAgentModelRejectsForbiddenOrMalformedAction(t *testing.T) {
	r := &captureRunner{text: `{"diagnosis":"edit it","confidence":1,"evidence":[],"recommended_action":"edit_source","human_approval_required":false,"suggested_retry_limit":0,"suggested_termination_limit":0}`}
	m := NewAgentModel(r, "", t.TempDir(), time.Second, 2, pricing.Table{})
	if _, _, err := m.Diagnose(context.Background(), ModelContext{}); err == nil {
		t.Fatal("forbidden action accepted")
	}
}
