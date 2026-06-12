package config

import (
	"fmt"
	"os"

	"github.com/goccy/go-yaml"
)

type ImapUserConfig struct {
	// PasswordHash is the SHA256 hex digest of the user's password
	PasswordHash string `yaml:"password-hash"`
}

type ImapConfigStruct struct {
	BindAddr string `yaml:"bind-addr"`
	TLSCert  string `yaml:"tls-cert"`
	TLSKey   string `yaml:"tls-key"`
	Users    map[string]ImapUserConfig `yaml:"users"`
}

func parseImapConfig(path string) (ImapConfigStruct, error) {
	cfg := ImapConfigStruct{}

	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("[imap] failed to read config file: %w", err)
	}

	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("[imap] failed to parse config file: %w", err)
	}

	return cfg, nil
}
