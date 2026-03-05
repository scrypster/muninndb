package plugin

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// ProviderScheme identifies the provider type.
type ProviderScheme string

const (
	SchemeLocal     ProviderScheme = "local"
	SchemeOllama    ProviderScheme = "ollama"
	SchemeOpenAI    ProviderScheme = "openai"
	SchemeAnthropic ProviderScheme = "anthropic"
	SchemeVoyage    ProviderScheme = "voyage"
	SchemeCohere    ProviderScheme = "cohere"
	SchemeGoogle    ProviderScheme = "google"
	SchemeJina      ProviderScheme = "jina"
	SchemeMistral   ProviderScheme = "mistral"
)

// ProviderConfig holds the parsed provider configuration.
type ProviderConfig struct {
	Scheme  ProviderScheme // ollama, openai, anthropic, voyage
	Host    string         // resolved host (e.g., "localhost" or "api.openai.com")
	Port    int            // resolved port (e.g., 11434 or 443)
	Model   string         // model name (e.g., "nomic-embed-text")
	BaseURL string         // fully constructed base URL (e.g., "http://localhost:11434")
}

// ParseProviderURL parses a provider URL and returns a ProviderConfig.
// Supports:
//   - ollama://host:port/model
//   - openai://model
//   - anthropic://model
//   - voyage://model
func ParseProviderURL(raw string) (*ProviderConfig, error) {
	if raw == "" {
		return nil, fmt.Errorf("empty provider URL")
	}

	// Parse as a URL
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("malformed provider URL: %w", err)
	}

	scheme := ProviderScheme(parsed.Scheme)
	config := &ProviderConfig{
		Scheme: scheme,
	}

	switch scheme {
	case SchemeLocal:
		// local://model-name — no host/port needed; assets are embedded in the binary.
		model := parsed.Hostname()
		if model == "" {
			model = strings.TrimPrefix(parsed.Path, "/")
		}
		if model == "" {
			model = "all-MiniLM-L6-v2"
		}
		config.Model = model
		return config, nil
	case SchemeOllama:
		return parseOllamaURL(parsed, config)
	case SchemeOpenAI:
		return parseOpenAIURL(parsed, config)
	case SchemeAnthropic:
		return parseAnthropicURL(parsed, config)
	case SchemeVoyage:
		return parseVoyageURL(parsed, config)
	case SchemeCohere:
		return parseCohereURL(parsed, config)
	case SchemeGoogle:
		return parseGoogleURL(parsed, config)
	case SchemeJina:
		return parseJinaURL(parsed, config)
	case SchemeMistral:
		return parseMistralURL(parsed, config)
	default:
		return nil, fmt.Errorf("unknown provider scheme: %q", scheme)
	}
}

func parseOllamaURL(parsed *url.URL, config *ProviderConfig) (*ProviderConfig, error) {
	// ollama://host:port/model
	host := parsed.Hostname()
	if host == "" {
		return nil, fmt.Errorf("ollama URL requires a host")
	}
	config.Host = host

	portStr := parsed.Port()
	if portStr == "" {
		return nil, fmt.Errorf("ollama URL requires a port (e.g., ollama://localhost:11434/model)")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("invalid port in ollama URL: %w", err)
	}
	config.Port = port

	// Model is the path (without leading /)
	model := strings.TrimPrefix(parsed.Path, "/")
	if model == "" {
		return nil, fmt.Errorf("ollama URL requires a model (e.g., ollama://localhost:11434/nomic-embed-text)")
	}
	config.Model = model

	config.BaseURL = fmt.Sprintf("http://%s:%d", config.Host, config.Port)
	return config, nil
}

func parseOpenAIURL(parsed *url.URL, config *ProviderConfig) (*ProviderConfig, error) {
	// openai://model
	model := parsed.Hostname()
	if model == "" {
		// Try to get model from the path (openai:///model-name would parse differently)
		model = strings.TrimPrefix(parsed.Path, "/")
		if model == "" && parsed.Opaque != "" {
			model = parsed.Opaque
		}
	}
	if model == "" {
		return nil, fmt.Errorf("openai URL requires a model (e.g., openai://text-embedding-3-small)")
	}
	config.Model = model

	config.Host = "api.openai.com"
	config.Port = 443
	config.BaseURL = "https://api.openai.com"
	if raw := strings.TrimSpace(parsed.Query().Get("base_url")); raw != "" {
		baseURL, host, port, err := parseHTTPBaseURL(raw, "openai")
		if err != nil {
			return nil, err
		}
		config.Host = host
		config.Port = port
		config.BaseURL = baseURL
	}
	return config, nil
}

func parseHTTPBaseURL(raw, provider string) (string, string, int, error) {
	baseURL, err := url.Parse(raw)
	if err != nil {
		return "", "", 0, fmt.Errorf("%s base_url is malformed: %w", provider, err)
	}
	scheme := strings.ToLower(strings.TrimSpace(baseURL.Scheme))
	if scheme != "http" && scheme != "https" {
		return "", "", 0, fmt.Errorf("%s base_url must use http or https", provider)
	}
	host := strings.TrimSpace(baseURL.Hostname())
	if host == "" {
		return "", "", 0, fmt.Errorf("%s base_url must include a host", provider)
	}

	port := 0
	if portStr := strings.TrimSpace(baseURL.Port()); portStr != "" {
		port, err = strconv.Atoi(portStr)
		if err != nil {
			return "", "", 0, fmt.Errorf("%s base_url has invalid port: %w", provider, err)
		}
		if port < 1 || port > 65535 {
			return "", "", 0, fmt.Errorf("%s base_url port must be between 1 and 65535", provider)
		}
	} else if scheme == "https" {
		port = 443
	} else {
		port = 80
	}

	cleanPath := strings.TrimRight(baseURL.Path, "/")
	cleanPath = strings.TrimSuffix(cleanPath, "/v1")
	if cleanPath == "/" {
		cleanPath = ""
	}
	normalized := &url.URL{
		Scheme: scheme,
		Host:   baseURL.Host,
		Path:   cleanPath,
	}
	return normalized.String(), host, port, nil
}

func parseAnthropicURL(parsed *url.URL, config *ProviderConfig) (*ProviderConfig, error) {
	// anthropic://model
	model := parsed.Hostname()
	if model == "" {
		model = strings.TrimPrefix(parsed.Path, "/")
		if model == "" && parsed.Opaque != "" {
			model = parsed.Opaque
		}
	}
	if model == "" {
		return nil, fmt.Errorf("anthropic URL requires a model (e.g., anthropic://claude-haiku)")
	}
	config.Model = model

	config.Host = "api.anthropic.com"
	config.Port = 443
	config.BaseURL = "https://api.anthropic.com"
	return config, nil
}

func parseVoyageURL(parsed *url.URL, config *ProviderConfig) (*ProviderConfig, error) {
	// voyage://model
	model := parsed.Hostname()
	if model == "" {
		model = strings.TrimPrefix(parsed.Path, "/")
		if model == "" && parsed.Opaque != "" {
			model = parsed.Opaque
		}
	}
	if model == "" {
		return nil, fmt.Errorf("voyage URL requires a model (e.g., voyage://voyage-3)")
	}
	config.Model = model

	config.Host = "api.voyageai.com"
	config.Port = 443
	config.BaseURL = "https://api.voyageai.com"
	return config, nil
}

func parseCohereURL(parsed *url.URL, config *ProviderConfig) (*ProviderConfig, error) {
	model := parsed.Hostname()
	if model == "" {
		model = strings.TrimPrefix(parsed.Path, "/")
		if model == "" && parsed.Opaque != "" {
			model = parsed.Opaque
		}
	}
	if model == "" {
		return nil, fmt.Errorf("cohere URL requires a model (e.g., cohere://embed-v4)")
	}
	config.Model = model

	config.Host = "api.cohere.com"
	config.Port = 443
	config.BaseURL = "https://api.cohere.com"
	return config, nil
}

func parseGoogleURL(parsed *url.URL, config *ProviderConfig) (*ProviderConfig, error) {
	model := parsed.Hostname()
	if model == "" {
		model = strings.TrimPrefix(parsed.Path, "/")
		if model == "" && parsed.Opaque != "" {
			model = parsed.Opaque
		}
	}
	if model == "" {
		return nil, fmt.Errorf("google URL requires a model (e.g., google://text-embedding-004)")
	}
	config.Model = model

	config.Host = "generativelanguage.googleapis.com"
	config.Port = 443
	config.BaseURL = "https://generativelanguage.googleapis.com"
	return config, nil
}

func parseJinaURL(parsed *url.URL, config *ProviderConfig) (*ProviderConfig, error) {
	model := parsed.Hostname()
	if model == "" {
		model = strings.TrimPrefix(parsed.Path, "/")
		if model == "" && parsed.Opaque != "" {
			model = parsed.Opaque
		}
	}
	if model == "" {
		return nil, fmt.Errorf("jina URL requires a model (e.g., jina://jina-embeddings-v3)")
	}
	config.Model = model

	config.Host = "api.jina.ai"
	config.Port = 443
	config.BaseURL = "https://api.jina.ai"
	return config, nil
}

func parseMistralURL(parsed *url.URL, config *ProviderConfig) (*ProviderConfig, error) {
	model := parsed.Hostname()
	if model == "" {
		model = strings.TrimPrefix(parsed.Path, "/")
		if model == "" && parsed.Opaque != "" {
			model = parsed.Opaque
		}
	}
	if model == "" {
		return nil, fmt.Errorf("mistral URL requires a model (e.g., mistral://mistral-embed)")
	}
	config.Model = model

	config.Host = "api.mistral.ai"
	config.Port = 443
	config.BaseURL = "https://api.mistral.ai"
	return config, nil
}
