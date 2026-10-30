package provider

import (
	"bytes"
	"encoding/json"
	"strings"
)

// ParseGeneric normalizes a provider call response into canonical Usage.
//
// It is total: a truncated body, an SSE stream, a non-usage payload or a 4xx
// error body all return a best-effort Usage and a nil error. Values are merged
// with max() so a stream that reports input tokens in one event and output
// tokens in another (Anthropic) ends up complete, while a single final usage
// object (OpenAI) is kept as-is.
func ParseGeneric(stream bool, contentType string, body []byte) (Usage, error) {
	var u Usage
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return u, nil
	}
	if stream || looksSSE(body, contentType) {
		parseSSE(body, &u)
	} else {
		parseUsageObject(body, &u)
	}
	if u.Input < 0 {
		u.Input = 0
	}
	if u.Output < 0 {
		u.Output = 0
	}
	return u, nil
}

func looksSSE(body []byte, contentType string) bool {
	if strings.Contains(strings.ToLower(contentType), "event-stream") {
		return true
	}
	return bytes.HasPrefix(body, []byte("data:")) || bytes.Contains(body, []byte("\ndata:"))
}

// parseSSE walks an event stream line by line. Consecutive data lines belong to
// one event; each event is decoded independently and merged.
func parseSSE(body []byte, u *Usage) {
	body = bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	body = bytes.ReplaceAll(body, []byte("\r"), []byte("\n"))
	var data bytes.Buffer
	flush := func() {
		if data.Len() == 0 {
			return
		}
		payload := bytes.TrimSpace(data.Bytes())
		data.Reset()
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			return
		}
		parseUsageObject(payload, u)
	}
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			flush()
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue // event:, id:, retry:, comments
		}
		chunk := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(chunk) == 0 {
			continue
		}
		if data.Len() > 0 {
			data.WriteByte('\n')
		}
		data.Write(chunk)
		if json.Valid(chunk) {
			flush() // one complete JSON object per data line is the common case
		}
	}
	flush()
}

type usageNumbers struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	TotalTokens      int64 `json:"total_tokens"`

	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	PromptCacheHitTokens     int64 `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens    int64 `json:"prompt_cache_miss_tokens"`

	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
	ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`

	Cost      *float64 `json:"cost"`
	TotalCost *float64 `json:"total_cost"`

	PromptTokensDetails struct {
		Cached int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputTokensDetails struct {
		Cached int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	CompletionTokensDetails struct {
		Reasoning int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	OutputTokensDetails struct {
		Reasoning int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

type usageEnvelope struct {
	Model        string       `json:"model"`
	ModelVersion string       `json:"modelVersion"`
	Usage        usageNumbers `json:"usage"`
	UsageMeta    usageNumbers `json:"usageMetadata"`
	Message      struct {
		Model string       `json:"model"`
		Usage usageNumbers `json:"usage"`
	} `json:"message"`
	Response struct {
		Model string       `json:"model"`
		Usage usageNumbers `json:"usage"`
	} `json:"response"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Candidates []struct {
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	TotalCost *float64 `json:"total_cost"`
}

func parseUsageObject(b []byte, u *Usage) {
	var env usageEnvelope
	if json.Unmarshal(b, &env) != nil {
		return
	}
	if model := firstNonEmpty(env.Model, env.Message.Model, env.Response.Model, env.ModelVersion); model != "" && u.Model == "" {
		u.Model = model
	}
	mergeUsageNumbers(u, env.Usage)
	mergeUsageNumbers(u, env.UsageMeta)
	mergeUsageNumbers(u, env.Message.Usage)
	mergeUsageNumbers(u, env.Response.Usage)
	if env.TotalCost != nil && u.CostUSD == nil {
		v := *env.TotalCost
		u.CostUSD = &v
	}
	for _, ch := range env.Choices {
		if ch.FinishReason != "" {
			u.FinishReason = ch.FinishReason
		}
	}
	for _, c := range env.Candidates {
		if c.FinishReason != "" {
			u.FinishReason = c.FinishReason
		}
	}
}

func mergeUsageNumbers(u *Usage, n usageNumbers) {
	input := n.PromptTokens
	if input == 0 {
		input = n.InputTokens
	}
	if input == 0 {
		input = n.PromptTokenCount
	}
	output := n.CompletionTokens
	if output == 0 {
		output = n.OutputTokens
	}
	if output == 0 {
		output = n.CandidatesTokenCount
	}
	cacheRead := n.CacheReadInputTokens
	if cacheRead == 0 {
		cacheRead = n.PromptCacheHitTokens
	}
	if cacheRead == 0 {
		cacheRead = n.PromptTokensDetails.Cached
	}
	if cacheRead == 0 {
		cacheRead = n.InputTokensDetails.Cached
	}
	if cacheRead == 0 {
		cacheRead = n.CachedContentTokenCount
	}
	cacheWrite := n.CacheCreationInputTokens
	reasoning := n.CompletionTokensDetails.Reasoning
	if reasoning == 0 {
		reasoning = n.OutputTokensDetails.Reasoning
	}
	if reasoning == 0 {
		reasoning = n.ThoughtsTokenCount
	}
	// DeepSeek reports cache misses instead of writes; derive a write count only
	// when no explicit cache-creation count was reported.
	if cacheWrite == 0 && n.PromptCacheMissTokens > 0 {
		cacheWrite = n.PromptCacheMissTokens
	}
	u.Input = maxInt64(u.Input, input)
	u.Output = maxInt64(u.Output, output)
	u.CacheRead = maxInt64(u.CacheRead, cacheRead)
	u.CacheWrite = maxInt64(u.CacheWrite, cacheWrite)
	u.Reasoning = maxInt64(u.Reasoning, reasoning)
	if n.Cost != nil {
		v := *n.Cost
		u.CostUSD = &v
	}
	if n.TotalCost != nil {
		v := *n.TotalCost
		u.CostUSD = &v
	}
}

func maxInt64(a, b int64) int64 {
	if b > a {
		return b
	}
	return a
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
