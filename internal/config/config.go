package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
	// Default RouteTarget.Enable to true when not specified.
	for _, route := range cfg.Routes {
		for i := range route.Targets {
			if route.Targets[i].Enable == nil {
				t := true
				route.Targets[i].Enable = &t
			}
		}
	}
	return &cfg, nil
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
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return os.WriteFile(m.path, data, 0644)
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
					// On rename (vim save pattern), re-add the file.
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
