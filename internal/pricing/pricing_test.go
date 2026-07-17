package pricing

import (
	"context"
	"math"
	"os"
	"testing"
	"time"
)

func TestLiveOfficialPricingPages(t *testing.T) {
	if os.Getenv("RC_PRICING_LIVE") != "1" {
		t.Skip("set RC_PRICING_LIVE=1 to validate official pricing pages")
	}
	m := New(t.TempDir())
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	status := m.Status()
	if status["official_models"].(int) < 6 {
		t.Fatalf("too few official models parsed: %#v", status)
	}
	usage := map[string]any{"input_tokens": int64(1000), "output_tokens": int64(100)}
	for _, model := range []string{"claude-opus-4-8", "gpt-5.6-sol"} {
		if _, ok, rate := m.Cost(model, usage); !ok || rate.Source != "official" {
			t.Fatalf("%s did not resolve to a live official rate: %#v", model, rate)
		}
	}
}

func TestParseAnthropicPricingUsesCurrentDatedRow(t *testing.T) {
	html := `<table><thead><tr><th>Model</th><th>Base Input Tokens</th><th>5m Cache Writes</th><th>1h Cache Writes</th><th>Cache Hits &amp; Refreshes</th><th>Output Tokens</th></tr></thead><tbody>
<tr><td>Claude Sonnet 5<br/>through August 31, 2026</td><td>$2 / MTok</td><td>$2.50 / MTok</td><td>$4 / MTok</td><td>$0.20 / MTok</td><td>$10 / MTok</td></tr>
<tr><td>Claude Sonnet 5<br/>starting September 1, 2026</td><td>$3 / MTok</td><td>$3.75 / MTok</td><td>$6 / MTok</td><td>$0.30 / MTok</td><td>$15 / MTok</td></tr>
<tr><td>Claude Opus 4.8</td><td>$5 / MTok</td><td>$6.25 / MTok</td><td>$10 / MTok</td><td>$0.50 / MTok</td><td>$25 / MTok</td></tr>
</tbody></table>`
	rates, err := parseAnthropicPricing([]byte(html), time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	sonnet := rates["claude-sonnet-5"]
	if sonnet.Input != 2 || sonnet.Output != 10 || sonnet.CacheCreate != 2.5 || sonnet.CacheRead != .2 {
		t.Fatalf("wrong current Sonnet rate: %#v", sonnet)
	}
	opus := rates["claude-opus-4-8"]
	if opus.Input != 5 || opus.Output != 25 {
		t.Fatalf("wrong Opus rate: %#v", opus)
	}

	rates, err = parseAnthropicPricing([]byte(html), time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if rates["claude-sonnet-5"].Input != 3 || rates["claude-sonnet-5"].Output != 15 {
		t.Fatalf("wrong post-introductory Sonnet rate: %#v", rates["claude-sonnet-5"])
	}
}

func TestParseOpenAIModelPricing(t *testing.T) {
	html := `<main><h1>GPT-5.6 Sol</h1><section>Text tokens <b>Per 1M tokens</b> · Batch API price [Input]
Input <span>$5.00</span> Cached input <span>$0.50</span> Output <span>$30.00</span></section>
<p>Cache writes are billed at 1.25x the uncached input token rate.</p></main>`
	rate, err := parseOpenAIModelPricing("gpt-5.6-sol", []byte(html), "https://example.test/model")
	if err != nil {
		t.Fatal(err)
	}
	if rate.Input != 5 || rate.Output != 30 || rate.CacheRead != .5 || rate.CacheCreate != 6.25 {
		t.Fatalf("wrong OpenAI rate: %#v", rate)
	}
}

func TestDiscoverOpenAIModelsDeduplicatesAndLimitsToGPT(t *testing.T) {
	html := []byte(`<a href="/api/docs/models/gpt-5.6-sol">Sol</a>
<a href="/api/docs/models/gpt-5.6-sol">Sol again</a>
<a href="/api/docs/models/gpt-5.6-terra">Terra</a>
<a href="/api/docs/models/o3">O3</a>`)
	got := discoverOpenAIModels(html)
	if len(got) != 2 || got[0] != "gpt-5.6-sol" || got[1] != "gpt-5.6-terra" {
		t.Fatalf("unexpected discovered models: %#v", got)
	}
}

func TestCostMatchesVersionedModelAndAllTokenClasses(t *testing.T) {
	m := New(t.TempDir())
	usage := map[string]any{
		"input_tokens": int64(1000), "output_tokens": int64(200),
		"cache_creation_input_tokens": int64(300), "cache_read_input_tokens": int64(4000),
	}
	cost, ok, _ := m.Cost("claude-opus-4-8-20260701", usage)
	if !ok {
		t.Fatal("expected versioned model match")
	}
	want := (1000*5 + 200*25 + 300*6.25 + 4000*.5) / 1_000_000
	if math.Abs(cost-want) > 1e-12 {
		t.Fatalf("cost=%v want=%v", cost, want)
	}
	if _, ok, _ := m.Cost("private-unpriced-model", usage); ok {
		t.Fatal("unknown model must not use a guessed price")
	}
	if _, ok, _ := m.Cost("gpt-5.5-private-alias", usage); ok {
		t.Fatal("arbitrary model aliases must not inherit a guessed prefix price")
	}
	fast := map[string]any{"input_tokens": int64(1000), "pricing_variant": "fast"}
	if _, ok, _ := m.Cost("claude-opus-4-8", fast); ok {
		t.Fatal("unsupported premium variants must not use standard pricing")
	}
	us := map[string]any{"input_tokens": int64(1_000_000), "inference_geo": "us"}
	cost, ok, _ = m.Cost("claude-opus-4-8", us)
	if !ok || cost != 5.5 {
		t.Fatalf("US inference multiplier missing: cost=%v ok=%v", cost, ok)
	}
}
