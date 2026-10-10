package pomoai

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// DefaultBaseURL is used when no --base-url, POMOAI_BASE_URL or saved value exists.
const DefaultBaseURL = "https://www.pomoai.ai"

type config struct {
	BaseURL string `json:"base_url,omitempty"`
	APIKey  string `json:"api_key,omitempty"`
}

func pomoaiHome() (string, error) {
	if h := os.Getenv("POMOAI_HOME"); h != "" {
		return filepath.Abs(h)
	}
	d, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate home directory (set POMOAI_HOME): %w", err)
	}
	return filepath.Join(d, ".pomoai"), nil
}

func configPath(home string) string { return filepath.Join(home, "config.json") }

func loadConfig(home string) (config, error) {
	var c config
	data, err := os.ReadFile(configPath(home))
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("invalid %s: %w", configPath(home), err)
	}
	return c, nil
}

func saveConfig(home string, c config) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(configPath(home), data)
}

// writeFileAtomic writes via temp file + fsync + rename, mode 0600, dir 0700.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

func keyFP(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:16]
}

// resolveKey: POMOAI_API_KEY first, then config. Returns key and its source.
func resolveKey(c config) (string, string, error) {
	if k := strings.TrimSpace(os.Getenv("POMOAI_API_KEY")); k != "" {
		return k, "env", nil
	}
	if k := strings.TrimSpace(c.APIKey); k != "" {
		return k, "config", nil
	}
	return "", "", &ExitError{Exit: exitAuth, Code: "api_key_missing", Msg: "no API key configured",
		Hint: "set POMOAI_API_KEY, or ask the user to run `pomoai login`"}
}

// resolveBaseURL: --base-url, POMOAI_BASE_URL, config, DefaultBaseURL.
func resolveBaseURL(flag string, c config) (string, error) {
	for _, v := range []string{flag, os.Getenv("POMOAI_BASE_URL"), c.BaseURL} {
		if strings.TrimSpace(v) != "" {
			return normalizeBaseURL(v)
		}
	}
	return normalizeBaseURL(DefaultBaseURL)
}

func normalizeBaseURL(raw string) (string, error) {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	s = strings.TrimRight(strings.TrimSuffix(s, "/v1"), "/")
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", &ExitError{Exit: exitUsage, Code: "usage", Msg: fmt.Sprintf("invalid base URL %q (want https://host[/path])", raw)}
	}
	if u.Scheme == "http" {
		h := u.Hostname()
		if ip := net.ParseIP(h); h != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return "", &ExitError{Exit: exitUsage, Code: "base_url_insecure", Msg: "refusing to send the API key over plain http to " + u.Host + "; use https"}
		}
	}
	return s, nil
}
