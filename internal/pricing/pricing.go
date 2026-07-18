package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	AnthropicPricingURL = "https://platform.claude.com/docs/en/about-claude/pricing"
	OpenAICompareURL    = "https://developers.openai.com/api/docs/models/compare"
	refreshInterval     = 24 * time.Hour
	maxPricingPageBytes = 12 << 20
)

type Rate struct {
	Model       string  `json:"model"`
	DisplayName string  `json:"display_name,omitempty"`
	Input       float64 `json:"input_per_mtok"`
	Output      float64 `json:"output_per_mtok"`
	CacheCreate float64 `json:"cache_create_per_mtok,omitempty"`
	CacheRead   float64 `json:"cache_read_per_mtok,omitempty"`
	Source      string  `json:"source"`
	SourceURL   string  `json:"source_url"`
	FetchedAt   string  `json:"fetched_at,omitempty"`
}

type cacheFile struct {
	UpdatedAt string          `json:"updated_at"`
	Models    map[string]Rate `json:"models"`
}

type Manager struct {
	mu        sync.RWMutex
	refreshMu sync.Mutex
	cachePath string
	client    *http.Client
	now       func() time.Time
	models    map[string]Rate
	updatedAt time.Time
	lastError string
}

func New(dataDir string) *Manager {
	m := &Manager{
		cachePath: filepath.Join(dataDir, "api-pricing.json"),
		client:    &http.Client{Timeout: 20 * time.Second},
		now:       time.Now,
		models:    fallbackRates(),
	}
	m.load()
	return m
}

func (m *Manager) Start(stop <-chan struct{}) {
	go func() {
		m.RefreshIfStale(context.Background())
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.RefreshIfStale(context.Background())
			case <-stop:
				return
			}
		}
	}()
}

func (m *Manager) RefreshIfStale(ctx context.Context) {
	m.mu.RLock()
	updated := m.updatedAt
	lastError := m.lastError
	m.mu.RUnlock()
	if lastError == "" && !updated.IsZero() && m.now().Sub(updated) < refreshInterval {
		return
	}
	_ = m.Refresh(ctx)
}

func (m *Manager) Refresh(ctx context.Context) error {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()

	type result struct {
		rates map[string]Rate
		err   error
	}
	results := make(chan result, 2)
	go func() {
		body, err := m.fetch(ctx, AnthropicPricingURL)
		if err != nil {
			results <- result{err: fmt.Errorf("anthropic: %w", err)}
			return
		}
		rates, err := parseAnthropicPricing(body, m.now())
		results <- result{rates: rates, err: err}
	}()
	go func() {
		rates, err := m.fetchOpenAI(ctx)
		results <- result{rates: rates, err: err}
	}()

	now := m.now().UTC()
	merged := map[string]Rate{}
	m.mu.RLock()
	for key, rate := range m.models {
		merged[key] = rate
	}
	m.mu.RUnlock()
	var refreshErrs []error
	success := false
	for i := 0; i < 2; i++ {
		res := <-results
		if res.err != nil {
			refreshErrs = append(refreshErrs, res.err)
			continue
		}
		if len(res.rates) == 0 {
			refreshErrs = append(refreshErrs, errors.New("official pricing source returned no models"))
			continue
		}
		success = true
		for key, rate := range res.rates {
			rate.FetchedAt = now.Format(time.RFC3339)
			merged[key] = rate
		}
	}
	if !success {
		err := errors.Join(refreshErrs...)
		m.setLastError(err)
		return err
	}
	m.mu.Lock()
	m.models = merged
	m.updatedAt = now
	m.lastError = errorString(errors.Join(refreshErrs...))
	m.mu.Unlock()
	if err := m.save(); err != nil {
		return err
	}
	return errors.Join(refreshErrs...)
}

func (m *Manager) fetchOpenAI(ctx context.Context) (map[string]Rate, error) {
	body, err := m.fetch(ctx, OpenAICompareURL)
	if err != nil {
		return nil, fmt.Errorf("openai compare: %w", err)
	}
	slugs := discoverOpenAIModels(body)
	seen := map[string]bool{}
	for _, slug := range slugs {
		seen[slug] = true
	}
	for _, slug := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5", "gpt-5.4", "gpt-5.2", "gpt-5"} {
		if !seen[slug] {
			slugs = append(slugs, slug)
			seen[slug] = true
		}
	}
	if len(slugs) == 0 {
		return nil, errors.New("openai compare: no model links found")
	}
	type result struct {
		rate Rate
		err  error
	}
	results := make(chan result, len(slugs))
	for _, slug := range slugs {
		slug := slug
		go func() {
			url := "https://developers.openai.com/api/docs/models/" + slug
			page, err := m.fetch(ctx, url)
			if err != nil {
				results <- result{err: err}
				return
			}
			rate, err := parseOpenAIModelPricing(slug, page, url)
			results <- result{rate: rate, err: err}
		}()
	}
	rates := map[string]Rate{}
	var errs []error
	for range slugs {
		res := <-results
		if res.err != nil {
			errs = append(errs, res.err)
			continue
		}
		rates[res.rate.Model] = res.rate
	}
	if len(rates) == 0 {
		return nil, fmt.Errorf("openai model prices: %w", errors.Join(errs...))
	}
	return rates, nil
}

func (m *Manager) fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "remote-agent-pricing/1.0")
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPricingPageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxPricingPageBytes {
		return nil, errors.New("pricing page is too large")
	}
	return body, nil
}

func (m *Manager) Cost(model string, usage map[string]any) (float64, bool, Rate) {
	variant := strings.ToLower(stringAny(usage["pricing_variant"]))
	if variant != "" && variant != "standard" && variant != "normal" {
		return 0, false, Rate{}
	}
	tier := strings.ToLower(stringAny(usage["service_tier"]))
	if tier != "" && tier != "standard" && tier != "default" {
		return 0, false, Rate{}
	}
	rate, ok := m.rate(model)
	if !ok {
		return 0, false, Rate{}
	}
	cost := floatAny(usage["input_tokens"])*rate.Input +
		floatAny(usage["output_tokens"])*rate.Output +
		floatAny(usage["cache_creation_input_tokens"])*rate.CacheCreate +
		floatAny(usage["cache_read_input_tokens"])*rate.CacheRead
	multiplier := 1.0
	if strings.EqualFold(stringAny(usage["inference_geo"]), "us") && strings.HasPrefix(normalizeModel(model), "claude-") {
		multiplier = 1.1
	}
	return cost / 1_000_000 * multiplier, true, rate
}

func (m *Manager) EnrichUsage(value any) any {
	switch rows := value.(type) {
	case []map[string]any:
		for _, row := range rows {
			m.enrichUsageRow(row)
		}
	case []any:
		for _, raw := range rows {
			if row, ok := raw.(map[string]any); ok {
				m.enrichUsageRow(row)
			}
		}
	}
	return value
}

func (m *Manager) EnrichMessages(messages []map[string]any) {
	for _, message := range messages {
		if stringAny(message["kind"]) != "turn_usage" {
			continue
		}
		usage, _ := message["usage"].(map[string]any)
		if usage == nil {
			continue
		}
		if rows := usage["models"]; rows != nil {
			m.EnrichUsage(rows)
			var total float64
			known := true
			switch list := rows.(type) {
			case []map[string]any:
				for _, row := range list {
					if _, ok := row["cost_usd"]; !ok {
						known = false
					}
					total += floatAny(row["cost_usd"])
				}
			default:
				known = false
			}
			if known {
				usage["cost_usd"] = total
			}
			usage["cost_known"] = known
			continue
		}
		m.enrichUsageRow(usage)
	}
}

func (m *Manager) enrichUsageRow(row map[string]any) {
	cost, ok, rate := m.Cost(stringAny(row["model"]), row)
	if !ok {
		delete(row, "cost_usd")
		row["cost_known"] = false
		return
	}
	row["cost_usd"] = cost
	row["cost_known"] = true
	row["price_source"] = rate.SourceURL
}

func (m *Manager) Status() map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	official, fallback := 0, 0
	for _, rate := range m.models {
		if rate.Source == "official" {
			official++
		} else {
			fallback++
		}
	}
	return map[string]any{
		"updated_at": nullableTime(m.updatedAt), "official_models": official,
		"fallback_models": fallback, "last_error": m.lastError,
		"sources": []string{AnthropicPricingURL, OpenAICompareURL},
	}
}

func (m *Manager) rate(model string) (Rate, bool) {
	key := normalizeModel(model)
	m.mu.RLock()
	defer m.mu.RUnlock()
	if rate, ok := m.models[key]; ok {
		return rate, true
	}
	keys := make([]string, 0, len(m.models))
	for candidate := range m.models {
		if strings.HasPrefix(key, candidate+"-") && versionSuffix(key[len(candidate)+1:]) {
			keys = append(keys, candidate)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	if len(keys) == 0 {
		return Rate{}, false
	}
	return m.models[keys[0]], true
}

func (m *Manager) load() {
	body, err := os.ReadFile(m.cachePath)
	if err != nil {
		return
	}
	var cached cacheFile
	if json.Unmarshal(body, &cached) != nil {
		return
	}
	for key, rate := range cached.Models {
		m.models[normalizeModel(key)] = rate
	}
	m.updatedAt, _ = time.Parse(time.RFC3339, cached.UpdatedAt)
}

func (m *Manager) save() error {
	m.mu.RLock()
	cached := cacheFile{UpdatedAt: m.updatedAt.Format(time.RFC3339), Models: map[string]Rate{}}
	for key, rate := range m.models {
		if rate.Source == "official" {
			cached.Models[key] = rate
		}
	}
	m.mu.RUnlock()
	body, err := json.MarshalIndent(cached, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.cachePath), 0o700); err != nil {
		return err
	}
	tmp := m.cachePath + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.cachePath)
}

func (m *Manager) setLastError(err error) {
	m.mu.Lock()
	m.lastError = errorString(err)
	m.mu.Unlock()
}

var (
	tagRE         = regexp.MustCompile(`(?s)<[^>]+>`)
	spaceRE       = regexp.MustCompile(`\s+`)
	rowRE         = regexp.MustCompile(`(?s)<tr[^>]*>(.*?)</tr>`)
	cellRE        = regexp.MustCompile(`(?s)<td[^>]*>(.*?)</td>`)
	dollarRE      = regexp.MustCompile(`\$([0-9]+(?:\.[0-9]+)?)`)
	openAIHrefRE  = regexp.MustCompile(`href=["']/api/docs/models/([a-z0-9][a-z0-9.\-]+)["']`)
	openAIPriceRE = regexp.MustCompile(`(?i)Text tokens\s+Per 1M tokens.{0,120}?Input\s+\$([0-9]+(?:\.[0-9]+)?)\s+Cached input\s+(?:\$([0-9]+(?:\.[0-9]+)?)|-)\s+Output\s+\$([0-9]+(?:\.[0-9]+)?)`)
	versionRE     = regexp.MustCompile(`^(?:20[0-9]{2}(?:-?[0-9]{2}){2}|v[0-9]+)(?:-|$)`)
	startingRE    = regexp.MustCompile(`(?i)starting ([A-Z][a-z]+ [0-9]{1,2}, [0-9]{4})`)
	throughRE     = regexp.MustCompile(`(?i)through ([A-Z][a-z]+ [0-9]{1,2}, [0-9]{4})`)
)

func parseAnthropicPricing(body []byte, now time.Time) (map[string]Rate, error) {
	s := string(body)
	header := strings.Index(s, "Base Input Tokens")
	if header < 0 {
		return nil, errors.New("anthropic pricing table header not found")
	}
	start := strings.LastIndex(s[:header], "<table")
	endRel := strings.Index(s[header:], "</table>")
	if start < 0 || endRel < 0 {
		return nil, errors.New("anthropic pricing table bounds not found")
	}
	table := s[start : header+endRel+len("</table>")]
	rates := map[string]Rate{}
	for _, row := range rowRE.FindAllStringSubmatch(table, -1) {
		cells := cellRE.FindAllStringSubmatch(row[1], -1)
		if len(cells) < 6 {
			continue
		}
		name := plainText(cells[0][1])
		if !activeDatedRow(name, now) {
			continue
		}
		values := make([]float64, 0, 5)
		valid := true
		for _, cell := range cells[1:6] {
			value, ok := dollarValue(plainText(cell[1]))
			if !ok {
				valid = false
				break
			}
			values = append(values, value)
		}
		if !valid {
			continue
		}
		display := cleanAnthropicName(name)
		key := anthropicModelKey(display)
		if key == "" {
			continue
		}
		rates[key] = Rate{
			Model: key, DisplayName: display, Input: values[0], CacheCreate: values[1],
			CacheRead: values[3], Output: values[4], Source: "official", SourceURL: AnthropicPricingURL,
		}
	}
	if len(rates) == 0 {
		return nil, errors.New("anthropic pricing table had no usable rows")
	}
	return rates, nil
}

func discoverOpenAIModels(body []byte) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, match := range openAIHrefRE.FindAllSubmatch(body, -1) {
		slug := string(match[1])
		if seen[slug] || !strings.HasPrefix(slug, "gpt-") {
			continue
		}
		seen[slug] = true
		out = append(out, slug)
		if len(out) == 12 {
			break
		}
	}
	return out
}

func parseOpenAIModelPricing(slug string, body []byte, sourceURL string) (Rate, error) {
	text := plainText(string(body))
	match := openAIPriceRE.FindStringSubmatch(text)
	if len(match) != 4 {
		return Rate{}, fmt.Errorf("%s: text token pricing not found", slug)
	}
	input, _ := strconv.ParseFloat(match[1], 64)
	cached, _ := strconv.ParseFloat(match[2], 64)
	output, _ := strconv.ParseFloat(match[3], 64)
	cacheCreate := 0.0
	if strings.Contains(strings.ToLower(text), "cache writes are billed at 1.25x") {
		cacheCreate = input * 1.25
	}
	return Rate{
		Model: normalizeModel(slug), DisplayName: slug, Input: input, Output: output,
		CacheCreate: cacheCreate, CacheRead: cached, Source: "official", SourceURL: sourceURL,
	}, nil
}

func plainText(raw string) string {
	return strings.TrimSpace(spaceRE.ReplaceAllString(html.UnescapeString(tagRE.ReplaceAllString(raw, " ")), " "))
}

func activeDatedRow(name string, now time.Time) bool {
	if match := startingRE.FindStringSubmatch(name); len(match) == 2 {
		start, err := time.Parse("January 2, 2006", match[1])
		if err == nil && now.Before(start) {
			return false
		}
	}
	if match := throughRE.FindStringSubmatch(name); len(match) == 2 {
		end, err := time.Parse("January 2, 2006", match[1])
		if err == nil && !now.Before(end.Add(24*time.Hour)) {
			return false
		}
	}
	return true
}

func cleanAnthropicName(name string) string {
	for _, marker := range []string{" (", " through ", " starting "} {
		if i := strings.Index(strings.ToLower(name), marker); i >= 0 {
			name = name[:i]
		}
	}
	return strings.TrimSpace(name)
}

func anthropicModelKey(display string) string {
	key := strings.ToLower(display)
	key = strings.ReplaceAll(key, ".", "-")
	return normalizeModel(key)
}

func normalizeModel(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	model = strings.ReplaceAll(model, "_", "-")
	model = strings.ReplaceAll(model, " ", "-")
	for strings.Contains(model, "--") {
		model = strings.ReplaceAll(model, "--", "-")
	}
	return strings.Trim(model, "-")
}

func versionSuffix(suffix string) bool {
	return versionRE.MatchString(suffix)
}

func dollarValue(text string) (float64, bool) {
	match := dollarRE.FindStringSubmatch(text)
	if len(match) != 2 {
		return 0, false
	}
	value, err := strconv.ParseFloat(match[1], 64)
	return value, err == nil
}

func fallbackRates() map[string]Rate {
	rates := map[string]Rate{}
	add := func(model string, in, out, create, read float64, url string) {
		rates[model] = Rate{Model: model, DisplayName: model, Input: in, Output: out, CacheCreate: create, CacheRead: read, Source: "fallback", SourceURL: url}
	}
	add("claude-fable-5", 10, 50, 12.5, 1, AnthropicPricingURL)
	add("claude-mythos-5", 10, 50, 12.5, 1, AnthropicPricingURL)
	for _, model := range []string{"claude-opus-4-8", "claude-opus-4-7", "claude-opus-4-6", "claude-opus-4-5"} {
		add(model, 5, 25, 6.25, .5, AnthropicPricingURL)
	}
	add("claude-opus-4-1", 15, 75, 18.75, 1.5, AnthropicPricingURL)
	add("claude-sonnet-5", 2, 10, 2.5, .2, AnthropicPricingURL)
	for _, model := range []string{"claude-sonnet-4-6", "claude-sonnet-4-5", "claude-sonnet-4"} {
		add(model, 3, 15, 3.75, .3, AnthropicPricingURL)
	}
	add("claude-haiku-4-5", 1, 5, 1.25, .1, AnthropicPricingURL)
	add("claude-haiku-3-5", .8, 4, 1, .08, AnthropicPricingURL)
	add("gpt-5.6-sol", 5, 30, 6.25, .5, "https://developers.openai.com/api/docs/models/gpt-5.6-sol")
	add("gpt-5.6", 5, 30, 6.25, .5, "https://developers.openai.com/api/docs/models/gpt-5.6-sol")
	add("gpt-5.6-terra", 2.5, 15, 3.125, .25, "https://developers.openai.com/api/docs/models/gpt-5.6-terra")
	add("gpt-5.6-luna", 1, 6, 1.25, .1, "https://developers.openai.com/api/docs/models/gpt-5.6-luna")
	add("gpt-5.5", 5, 30, 0, .5, "https://developers.openai.com/api/docs/models/gpt-5.5")
	add("gpt-5.4", 2.5, 15, 0, .25, "https://developers.openai.com/api/docs/models/gpt-5.4")
	add("gpt-5.2", 1.75, 14, 0, .175, "https://developers.openai.com/api/docs/models/gpt-5.2")
	add("gpt-5", 1.25, 10, 0, .125, "https://developers.openai.com/api/docs/models/gpt-5")
	return rates
}

func floatAny(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		n, _ := v.Float64()
		return n
	}
	return 0
}

func stringAny(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.Format(time.RFC3339)
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
