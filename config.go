package main

import (
	"encoding/json"
	"os"
)

// LoadConfigFromFile 从文件加载配置
func LoadConfigFromFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	// 设置默认值
	if config.ListenAddr == "" {
		config.ListenAddr = ":8080"
	}

	return &config, nil
}

// SaveConfigToFile 保存配置到文件
func SaveConfigToFile(config *Config, path string) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
