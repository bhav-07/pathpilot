package config

import (
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	LogDir         string
	LogLevel       string
	APIPort        string
	ProxyPort      string
	MaxLogFileSize int64
}

func Load() (*Config, error) {
	cfg := &Config{
		LogDir:         getEnv("LOG_DIR", "logs"),
		LogLevel:       getEnv("LOG_LEVEL", "INFO"),
		APIPort:        getEnv("API_PORT", "8080"),
		ProxyPort:      getEnv("PROXY_PORT", "8000"),
		MaxLogFileSize: getEnvAsInt64("MAX_LOG_FILE_SIZE", 10*1024*1024),
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func (c *Config) validate() error {
	if c.LogDir == "" {
		return fmt.Errorf("LOG_DIR must not be empty")
	}
	if c.APIPort == "" {
		return fmt.Errorf("API_PORT must not be empty")
	}
	if c.ProxyPort == "" {
		return fmt.Errorf("PROXY_PORT must not be empty")
	}
	return nil
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

func getEnvAsInt64(key string, fallback int64) int64 {
	strValue := getEnv(key, "")
	if value, err := strconv.ParseInt(strValue, 10, 64); err == nil {
		return value
	}
	return fallback
}
