package provider

import "net/http"

// RegisterBuiltInProviders registers the four built-in providers (Gemini native + 3 OpenAI-compatible presets).
func RegisterBuiltInProviders(reg *Registry, client *http.Client) error {
	geminiMeta := ProviderMetadata{
		ID:             "gemini",
		Name:           "Google Gemini",
		ShortName:      "GEMINI",
		Icon:           "✳",
		Description:    "Save your Google AI Studio key to list all Gemini models and test which ones respond.",
		KeyLabel:       "Google AI Studio API Key",
		KeyPlaceholder: "Paste your Gemini API key (AIzaSy...)",
		HelpText:       "Get a Gemini API key from Google AI Studio:",
		HelpURL:        "https://aistudio.google.com/app/apikey",
		HelpButton:     "Open Google AI Studio ↗",
		BuiltIn:        true,
		AdapterType:    "gemini",
		RequiresAPIKey: true,
		DefaultModel:   "gemini-2.0-flash",
		Capabilities: Capabilities{
			Chat:            true,
			Streaming:       false,
			ModelDiscovery:  true,
			SystemPrompt:    true,
			ReasoningOutput: false,
			JSONMode:        true,
		},
	}
	geminiCfg := ProviderConfig{
		ID:         "gemini",
		Enabled:    true,
		BaseURL:    "https://generativelanguage.googleapis.com/v1beta/models",
		AuthType:   "header",
		AuthHeader: "x-goog-api-key",
		SortOrder:  10,
	}
	if err := reg.Register(NewGeminiAdapter(geminiMeta, client), geminiCfg); err != nil {
		return err
	}

	openRouterMeta := ProviderMetadata{
		ID:                "openrouter",
		Name:              "OpenRouter",
		ShortName:         "OPENROUTER",
		Icon:              "⇄",
		Description:       "Connect OpenRouter to access free (:free) and open-weight models. Free models are filtered at the top automatically.",
		KeyLabel:          "OpenRouter API Key",
		KeyPlaceholder:    "Paste your OpenRouter API key (sk-or-v1-...)",
		HelpText:          "Get a free OpenRouter API key:",
		HelpURL:           "https://openrouter.ai/keys",
		HelpButton:        "Open OpenRouter Keys ↗",
		BuiltIn:           true,
		AdapterType:       "openai_compat",
		RequiresAPIKey:    true,
		SupportsFreeBadge: true,
		Capabilities: Capabilities{
			Chat:            true,
			Streaming:       true,
			ModelDiscovery:  true,
			SystemPrompt:    true,
			ReasoningOutput: true,
			JSONMode:        true,
		},
	}
	openRouterCfg := ProviderConfig{
		ID:            "openrouter",
		Enabled:       true,
		BaseURL:       "https://openrouter.ai/api/v1",
		ChatPath:      "/chat/completions",
		ModelsPath:    "/models",
		AuthType:      "bearer",
		AuthHeader:    "Authorization",
		ExtraHeaders:  map[string]string{"HTTP-Referer": "https://github.com/sahand363/sahand-cortext", "X-Title": "Sahand-CorText"},
		PricingFormat: "openrouter",
		SortOrder:     20,
	}
	if err := reg.Register(NewOpenAICompatAdapter(openRouterMeta, client), openRouterCfg); err != nil {
		return err
	}

	aiPrimeMeta := ProviderMetadata{
		ID:             "aiprime",
		Name:           "AIPrime",
		ShortName:      "AIPRIME",
		Icon:           "◈",
		Description:    "Connect AIPrime to use GLM 5.3 Flash (glm-5.3-flash) and any other models enabled on your AIPrime wallet.",
		KeyLabel:       "AIPrime API Key (https://api.aiprime.shop/v1)",
		KeyPlaceholder: "Paste your AIPrime API key (sk-...)",
		HelpText:       "Manage your AIPrime keys and models:",
		HelpURL:        "https://aiprime.shop",
		HelpButton:     "Open AIPrime Panel ↗",
		BuiltIn:        true,
		AdapterType:    "openai_compat",
		RequiresAPIKey: true,
		DefaultModel:   "glm-5.3-flash",
		Capabilities: Capabilities{
			Chat:            true,
			Streaming:       true,
			ModelDiscovery:  true,
			SystemPrompt:    true,
			ReasoningOutput: true,
			JSONMode:        true,
		},
	}
	aiPrimeCfg := ProviderConfig{
		ID:                     "aiprime",
		Enabled:                true,
		BaseURL:                "https://api.aiprime.shop/v1",
		ChatPath:               "/chat/completions",
		ModelsPath:             "/models",
		AuthType:               "bearer",
		AuthHeader:             "Authorization",
		OmitMaxTokens:          true,
		StreamOnRetry:          true,
		AllowDiscoveryFallback: true,
		PricingFormat:          "wallet",
		Featured: []DiscoveredModel{
			{
				Provider:    "aiprime",
				ID:          "glm-5.3-flash",
				DisplayName: "GLM 5.3 Flash",
				Description: "AIPrime Featured · Reasoning & Vision capable",
				ContextLen:  128000,
				IsFree:      false,
				PriceInfo:   "$0.006 in / $0.018 out",
			},
		},
		SortOrder: 30,
	}
	if err := reg.Register(NewOpenAICompatAdapter(aiPrimeMeta, client), aiPrimeCfg); err != nil {
		return err
	}

	codeCraftMeta := ProviderMetadata{
		ID:             "codecraft",
		Name:           "CodeCraft API",
		ShortName:      "CODECRAFT",
		Icon:           "⚡",
		Description:    "Connect CodeCraft API to test which of their hosted models respond on your plan.",
		KeyLabel:       "CodeCraft API Key (https://codecraftapi.com/v1)",
		KeyPlaceholder: "Paste your CodeCraft API key (cc_...)",
		HelpText:       "Get a CodeCraft API key:",
		HelpURL:        "https://codecraftapi.com/docs/getting-started",
		HelpButton:     "Open CodeCraft Docs ↗",
		BuiltIn:        true,
		AdapterType:    "openai_compat",
		RequiresAPIKey: true,
		Capabilities: Capabilities{
			Chat:            true,
			Streaming:       true,
			ModelDiscovery:  true,
			SystemPrompt:    true,
			ReasoningOutput: true,
			JSONMode:        true,
		},
	}
	codeCraftCfg := ProviderConfig{
		ID:            "codecraft",
		Enabled:       true,
		BaseURL:       "https://codecraftapi.com/v1",
		ChatPath:      "/chat/completions",
		ModelsPath:    "/models",
		AuthType:      "bearer",
		AuthHeader:    "Authorization",
		PricingFormat: "codecraft",
		SortOrder:     40,
	}
	return reg.Register(NewOpenAICompatAdapter(codeCraftMeta, client), codeCraftCfg)
}
