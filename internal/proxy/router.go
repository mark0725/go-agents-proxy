package proxy

import (
	"fmt"
	"strings"

	"github.com/mark0725/go-agents-proxy/internal/config"
)

// ResolvedTarget holds the resolved provider API and target model.
type ResolvedTarget struct {
	Provider        string
	ModelID         string
	API             config.APIConfig
	ClientAPIType   string // normalized client/request api type
	ProviderAPIType string // normalized provider/upstream api type
	Proxy           string // provider-level proxy URL
}

// ResolveTargets looks up a route and model, returning ordered fallback targets.
func ResolveTargets(cfg *config.Config, routeID, modelID string) ([]ResolvedTarget, error) {
	route, ok := cfg.Routes[routeID]
	if !ok {
		return nil, fmt.Errorf("route not found: %s", routeID)
	}

	var result []ResolvedTarget
	for _, target := range route.Targets {
		if target.Enable != nil && !*target.Enable {
			continue
		}
		// Find the best matching model mapping in this target group.
		var bestMatch *config.ModelMapping
		bestPriority := -1
		for i := range target.Models {
			m := target.Models[i].MatchModel
			if m == modelID {
				bestMatch = &target.Models[i]
				break // exact match wins
			}
			priority := matchPriority(m, modelID)
			if priority > bestPriority {
				bestMatch = &target.Models[i]
				bestPriority = priority
			}
		}
		if bestMatch == nil {
			continue
		}

		providerCfg, ok := cfg.Providers[bestMatch.Provider]
		if !ok || !providerCfg.Enable {
			continue
		}
		apiType := bestMatch.APIType
		if strings.TrimSpace(apiType) == "" {
			apiType = route.APIType
		}
		api := pickAPI(providerCfg, apiType, bestMatch.APIName)
		clientAPIType := normalizeAPIType(route.APIType)
		providerAPIType := normalizeAPIType(api.APIType)
		if providerAPIType == "" {
			providerAPIType = clientAPIType
		}
		result = append(result, ResolvedTarget{
			Provider:        bestMatch.Provider,
			ModelID:         bestMatch.ModelID,
			API:             api,
			ClientAPIType:   clientAPIType,
			ProviderAPIType: providerAPIType,
			Proxy:           providerCfg.Proxy,
		})
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("no targets available for model: %s/%s", routeID, modelID)
	}
	return result, nil
}

// pickAPI selects the best matching API from a provider config.
func pickAPI(providerCfg config.ProviderConfig, apiType, apiName string) config.APIConfig {
	if apiName != "" {
		for _, api := range providerCfg.APIs {
			if api.Name == apiName && strings.EqualFold(api.APIType, apiType) {
				return api
			}
		}
	}
	for _, api := range providerCfg.APIs {
		if strings.EqualFold(api.APIType, apiType) {
			return api
		}
	}
	if len(providerCfg.APIs) > 0 {
		return providerCfg.APIs[0]
	}
	return config.APIConfig{}
}

// matchPriority returns a priority score for how well pattern matches modelID.
// Higher score = better match. -1 means no match.
//
//	exact match      -> stops search immediately (highest priority)
//	"prefix*" match  -> len(prefix) (longer prefix = higher priority)
//	"*" match        -> 0 (lowest priority)
func matchPriority(pattern, modelID string) int {
	if pattern == "*" {
		return 0
	}
	if strings.HasSuffix(pattern, "*") {
		prefix := strings.TrimSuffix(pattern, "*")
		if strings.HasPrefix(modelID, prefix) {
			return len(prefix)
		}
	}
	return -1
}

// normalizeAPIType normalizes api_type values.
func normalizeAPIType(t string) string {
	switch strings.ToLower(t) {
	case "openai":
		return "openai"
	case "anthropic":
		return "anthropic"
	case "google", "gemini":
		return "google"
	default:
		return t
	}
}
