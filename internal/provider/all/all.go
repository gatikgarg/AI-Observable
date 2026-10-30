// Package all wires every provider adapter into provider.For.
//
// The adapters register themselves from init(), so something has to import them
// for the side effect. It cannot be package provider (that would be an import
// cycle), and it must be imported by every binary and test that resolves a
// provider kind. Import this package for its side effect:
//
//	import _ "github.com/ai-observable/ai-observable/internal/provider/all"
package all

import (
	_ "github.com/ai-observable/ai-observable/internal/provider/anthropic"
	_ "github.com/ai-observable/ai-observable/internal/provider/azure"
	_ "github.com/ai-observable/ai-observable/internal/provider/deepseek"
	_ "github.com/ai-observable/ai-observable/internal/provider/gemini"
	_ "github.com/ai-observable/ai-observable/internal/provider/groq"
	_ "github.com/ai-observable/ai-observable/internal/provider/local"
	_ "github.com/ai-observable/ai-observable/internal/provider/openai"
	_ "github.com/ai-observable/ai-observable/internal/provider/openrouter"
)
