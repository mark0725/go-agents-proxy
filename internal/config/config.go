package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

// AppConfig holds top-level application settings.
type AppConfig struct {
	Level  string `yaml:"level" json:"level"`
	Auth   bool   `yaml:"auth" json:"auth"`
	Listen string `yaml:"listen" json:"listen"`
	Port   string `yaml:"port" json:"port"`
}

// User represents a user with API access.
type User struct {
	Name     string `yaml:"name" json:"name"`
	Token    string `yaml:"token" json:"token"`
	Password string `yaml:"password" json:"password"`
}

// Token represents a client token.
type Token struct {
	ID    string `yaml:"id" json:"id"`
	Token string `yaml:"token" json:"token"`
}

// ModelMapping maps a client model pattern to a provider and upstream model.
type ModelMapping struct {
	MatchModel string `yaml:"match_model" json:"match_model"`
	Provider   string `yaml:"provider" json:"provider"`
	ModelID    string `yaml:"model_id" json:"model_id"`
	APIName    string `yaml:"api_name" json:"api_name"`
	APIType    string `yaml:"api_type" json:"api_type"`
}

// RouteTarget is a target group within a route, containing model mappings.
type RouteTarget struct {
	Name   string         `yaml:"name" json:"name"`
	Enable *bool          `yaml:"enable" json:"enable"`
	Models []ModelMapping `yaml:"models" json:"models"`
}

// Route defines an API route.
type Route struct {
	APIType string        `yaml:"api_type" json:"api_type"`
	Targets []RouteTarget `yaml:"targets" json:"targets"`
}

// APIConfig is a single provider API endpoint.
type APIConfig struct {
	Name    string `yaml:"name" json:"name"`
	APIType string `yaml:"api_type" json:"api_type"`
	BaseURL string `yaml:"base_url" json:"base_url"`
	APIKey  string `yaml:"api_key" json:"api_key"`
}

// ProviderModel is a model entry within a provider.
type ProviderModel struct {
	ModelID string `yaml:"model_id" json:"model_id"`
}

// ProviderConfig holds models and API endpoints for a provider.
type ProviderConfig struct {
	Name   string          `yaml:"name,omitempty" json:"name,omitempty"`
	Enable bool            `yaml:"enable" json:"enable"`
	Proxy  string          `yaml:"proxy,omitempty" json:"proxy,omitempty"`
	Models []ProviderModel `yaml:"models" json:"models"`
	APIs   []APIConfig     `yaml:"apis" json:"apis"`
}

// Config is the root configuration.
type Config struct {
	App       AppConfig                 `yaml:"app" json:"app"`
	Users     []User                    `yaml:"users" json:"users"`
	Tokens    []Token                   `yaml:"tokens" json:"tokens"`
	Routes    map[string]Route          `yaml:"routes" json:"routes"`
	Providers map[string]ProviderConfig `yaml:"providers" json:"providers"`
}

// ValidationError represents a config validation failure.
type ValidationError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// Manager holds the current config with thread-safe access.
type Manager struct {
	mu     sync.RWMutex
	config *Config
	path   string
}

// NewManager loads config from path and returns a manager.
func NewManager(path string) (*Manager, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}
	cfg, err := Load(absPath)
	if err != nil {
		return nil, err
	}
	return &Manager{config: cfg, path: absPath}, nil
}

// Load reads and parses the YAML config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	NormalizeConfig(&cfg)
	if errs := ValidateConfig(&cfg); len(errs) > 0 {
		return nil, fmt.Errorf("validate config: %s", formatValidationErrors(errs))
	}
	return &cfg, nil
}

// NormalizeConfig applies default values and canonicalizes config shapes.
func NormalizeConfig(cfg *Config) {
	if cfg.Routes == nil {
		cfg.Routes = map[string]Route{}
	}
	if cfg.Providers == nil {
		cfg.Providers = map[string]ProviderConfig{}
	}
	for routeID, route := range cfg.Routes {
		for i := range route.Targets {
			if route.Targets[i].Enable == nil {
				enabled := true
				route.Targets[i].Enable = &enabled
			}
			for j := range route.Targets[i].Models {
				if strings.TrimSpace(route.Targets[i].Models[j].APIType) == "" {
					route.Targets[i].Models[j].APIType = route.APIType
				}
			}
		}
		cfg.Routes[routeID] = route
	}
}

// ValidateConfig validates the current config and returns field-level errors.
func ValidateConfig(cfg *Config) []ValidationError {
	var errs []ValidationError

	for providerName, provider := range cfg.Providers {
		providerPath := fmt.Sprintf("providers.%s", providerName)
		if strings.TrimSpace(providerName) == "" {
			errs = append(errs, ValidationError{Path: "providers", Message: "provider key cannot be empty"})
		}
		if strings.TrimSpace(provider.Proxy) != "" {
			if err := validateURL(provider.Proxy); err != nil {
				errs = append(errs, ValidationError{Path: providerPath + ".proxy", Message: err.Error()})
			}
		}
		for i, model := range provider.Models {
			if strings.TrimSpace(model.ModelID) == "" {
				errs = append(errs, ValidationError{Path: fmt.Sprintf("%s.models[%d].model_id", providerPath, i), Message: "model_id cannot be empty"})
			}
		}
		apiKeys := map[string]struct{}{}
		for i, api := range provider.APIs {
			apiPath := fmt.Sprintf("%s.apis[%d]", providerPath, i)
			if strings.TrimSpace(api.Name) == "" {
				errs = append(errs, ValidationError{Path: apiPath + ".name", Message: "api name cannot be empty"})
			}
			if !isValidAPIType(api.APIType) {
				errs = append(errs, ValidationError{Path: apiPath + ".api_type", Message: "api_type must be one of anthropic, openai, gemini"})
			}
			apiKey := strings.TrimSpace(api.Name) + "\x00" + strings.ToLower(strings.TrimSpace(api.APIType))
			if strings.TrimSpace(api.Name) != "" && strings.TrimSpace(api.APIType) != "" {
				if _, exists := apiKeys[apiKey]; exists {
					errs = append(errs, ValidationError{Path: apiPath, Message: "provider api name + api_type must be unique within provider"})
				}
				apiKeys[apiKey] = struct{}{}
			}
			if strings.TrimSpace(api.BaseURL) != "" {
				if err := validateURL(api.BaseURL); err != nil {
					errs = append(errs, ValidationError{Path: apiPath + ".base_url", Message: err.Error()})
				}
			}
		}
	}

	for routeID, route := range cfg.Routes {
		routePath := fmt.Sprintf("routes.%s", routeID)
		if strings.TrimSpace(routeID) == "" {
			errs = append(errs, ValidationError{Path: "routes", Message: "route key cannot be empty"})
		}
		if !isValidAPIType(route.APIType) {
			errs = append(errs, ValidationError{Path: routePath + ".api_type", Message: "api_type must be one of anthropic, openai, gemini"})
		}
		if len(route.Targets) == 0 {
			errs = append(errs, ValidationError{Path: routePath + ".targets", Message: "route must have at least one target"})
		}
		for i, target := range route.Targets {
			targetPath := fmt.Sprintf("%s.targets[%d]", routePath, i)
			if len(target.Models) == 0 {
				errs = append(errs, ValidationError{Path: targetPath + ".models", Message: "target must have at least one model mapping"})
			}
			for j, model := range target.Models {
				modelPath := fmt.Sprintf("%s.models[%d]", targetPath, j)
				if strings.TrimSpace(model.MatchModel) == "" {
					errs = append(errs, ValidationError{Path: modelPath + ".match_model", Message: "match_model cannot be empty"})
				}
				providerName := strings.TrimSpace(model.Provider)
				if providerName == "" {
					errs = append(errs, ValidationError{Path: modelPath + ".provider", Message: "provider cannot be empty"})
					continue
				}
				provider, ok := cfg.Providers[providerName]
				if !ok {
					errs = append(errs, ValidationError{Path: modelPath + ".provider", Message: "provider does not exist"})
					continue
				}
				if strings.TrimSpace(model.ModelID) == "" {
					errs = append(errs, ValidationError{Path: modelPath + ".model_id", Message: "model_id cannot be empty"})
				} else if !providerHasModel(provider, model.ModelID) {
					errs = append(errs, ValidationError{Path: modelPath + ".model_id", Message: "model_id must exist in the selected provider models"})
				}
				apiType := strings.TrimSpace(model.APIType)
				if !isValidAPIType(apiType) {
					errs = append(errs, ValidationError{Path: modelPath + ".api_type", Message: "api_type must be one of anthropic, openai, gemini"})
				}
				if apiName := strings.TrimSpace(model.APIName); apiName != "" && isValidAPIType(apiType) && !providerHasAPI(provider, apiName, apiType) {
					errs = append(errs, ValidationError{Path: modelPath + ".api_name", Message: "api_name + api_type must exist in the selected provider"})
				}
				if len(provider.APIs) == 0 {
					errs = append(errs, ValidationError{Path: "providers." + providerName + ".apis", Message: "provider must define at least one api"})
				}
			}
		}
	}

	return errs
}

func providerHasModel(provider ProviderConfig, modelID string) bool {
	for _, model := range provider.Models {
		if model.ModelID == modelID {
			return true
		}
	}
	return false
}

func providerHasAPI(provider ProviderConfig, apiName, apiType string) bool {
	for _, api := range provider.APIs {
		if api.Name == apiName && strings.EqualFold(api.APIType, apiType) {
			return true
		}
	}
	return false
}

func isValidAPIType(apiType string) bool {
	switch strings.ToLower(strings.TrimSpace(apiType)) {
	case "anthropic", "openai", "gemini":
		return true
	default:
		return false
	}
}

func validateURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("url cannot be empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url must use http or https")
	}
	if u.Host == "" {
		return fmt.Errorf("url host cannot be empty")
	}
	return nil
}

func formatValidationErrors(errs []ValidationError) string {
	parts := make([]string, 0, len(errs))
	for _, err := range errs {
		parts = append(parts, fmt.Sprintf("%s: %s", err.Path, err.Message))
	}
	return strings.Join(parts, "; ")
}

// Get returns the current config (read-only; copy if mutation needed).
func (m *Manager) Get() *Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config
}

// Set updates the current config.
func (m *Manager) Set(cfg *Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.config = cfg
}

// Save writes the current config back to disk.
func (m *Manager) Save() error {
	m.mu.RLock()
	cfg := m.config
	m.mu.RUnlock()
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	NormalizeConfig(cfg)
	if errs := ValidateConfig(cfg); len(errs) > 0 {
		return fmt.Errorf("validate config: %s", formatValidationErrors(errs))
	}
	return m.writeConfig(cfg)
}

// SaveConfig validates, writes, and activates the given config.
func (m *Manager) SaveConfig(cfg *Config) error {
	NormalizeConfig(cfg)
	if errs := ValidateConfig(cfg); len(errs) > 0 {
		return fmt.Errorf("validate config: %s", formatValidationErrors(errs))
	}
	if err := m.writeConfig(cfg); err != nil {
		return err
	}
	m.Set(cfg)
	return nil
}

func (m *Manager) writeConfig(cfg *Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(m.path, data, 0644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// Watch starts a filesystem watcher that reloads config on change.
func (m *Manager) Watch() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := watcher.Add(m.path); err != nil {
		return err
	}
	go func() {
		var debounceTimer *time.Timer
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Rename) {
					if event.Has(fsnotify.Rename) {
						_ = watcher.Add(m.path)
					}
					if debounceTimer != nil {
						debounceTimer.Stop()
					}
					debounceTimer = time.AfterFunc(300*time.Millisecond, func() {
						cfg, err := Load(m.path)
						if err != nil {
							slog.Error("config reload failed", slog.String("error", err.Error()))
							return
						}
						m.Set(cfg)
						slog.Info("config reloaded")
					})
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				slog.Error("config watcher error", slog.String("error", err.Error()))
			}
		}
	}()
	return nil
}
