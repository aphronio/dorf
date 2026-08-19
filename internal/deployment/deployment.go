package deployment

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Database Database `json:"database"`
}

type Database struct {
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
	Name     string `json:"name,omitempty"`
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
	Image    string `json:"image,omitempty"`
	ImageID  string `json:"image_id,omitempty"`
}

func Path(home string) string {
	root := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if root == "" {
		root = filepath.Join(home, ".config")
	}
	return filepath.Join(root, "dorf", "deployment.json")
}

func Load(path string) (Config, bool, error) {
	contents, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Config{}, false, nil
	}
	if err != nil {
		return Config{}, false, fmt.Errorf("read Dorf deployment configuration: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(contents, &cfg); err != nil {
		return Config{}, false, fmt.Errorf("decode Dorf deployment configuration: %w", err)
	}
	if err := cfg.Database.Validate(); err != nil {
		return Config{}, false, err
	}
	return cfg, true, nil
}

func Save(path string, cfg Config) error {
	if err := cfg.Database.Validate(); err != nil {
		return err
	}
	contents, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create Dorf configuration directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".deployment-*.json")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("commit Dorf deployment configuration: %w", err)
	}
	return nil
}

func (d Database) Validate() error {
	if d.Host != "127.0.0.1" || d.Port < 1024 || d.Port > 65535 || strings.TrimSpace(d.Name) == "" || strings.TrimSpace(d.User) == "" || strings.TrimSpace(d.Password) == "" || strings.TrimSpace(d.Image) == "" || strings.TrimSpace(d.ImageID) == "" {
		return fmt.Errorf("Docker PostgreSQL deployment configuration is incomplete")
	}
	return nil
}

func (d Database) URL() (string, error) {
	if err := d.Validate(); err != nil {
		return "", err
	}
	value := &url.URL{
		Scheme: "postgresql",
		User:   url.UserPassword(d.User, d.Password),
		Host:   d.Host + ":" + strconv.Itoa(d.Port),
		Path:   "/" + d.Name,
	}
	query := value.Query()
	query.Set("sslmode", "disable")
	value.RawQuery = query.Encode()
	return value.String(), nil
}
