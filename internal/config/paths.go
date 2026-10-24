package config

import (
	"fmt"
	"os"
	"strings"
)

// ToCanonical converts an instance-local path to the canonical (shared) form.
func (in InstanceConfig) ToCanonical(p string) string {
	return mapPrefix(p, in.PathMappings, func(m PathMapping) (string, string) { return m.Instance, m.Canonical })
}

// FromCanonical converts a canonical path to this instance's local form.
func (in InstanceConfig) FromCanonical(p string) string {
	return mapPrefix(p, in.PathMappings, func(m PathMapping) (string, string) { return m.Canonical, m.Instance })
}

// mapPrefix applies the longest matching prefix mapping.
func mapPrefix(p string, ms []PathMapping, dir func(PathMapping) (string, string)) string {
	best, bestTo := "", ""
	for _, m := range ms {
		from, to := dir(m)
		from = strings.TrimRight(from, "/")
		if (p == from || strings.HasPrefix(p, from+"/")) && len(from) > len(best) {
			best, bestTo = from, strings.TrimRight(to, "/")
		}
	}
	if best == "" {
		return p
	}
	return bestTo + strings.TrimPrefix(p, best)
}

// SamePath compares two paths ignoring trailing slashes.
func SamePath(a, b string) bool {
	return strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
}

// Resolve returns a copy ready to connect with: ${VAR} references expanded and
// password_file read. Used to test instances that are not saved yet.
func (in InstanceConfig) Resolve() (InstanceConfig, error) {
	in.URL = os.ExpandEnv(in.URL)
	in.Username = os.ExpandEnv(in.Username)
	in.Password = os.ExpandEnv(in.Password)
	in.APIKey = os.ExpandEnv(in.APIKey)
	for _, f := range []struct {
		key  string
		path string
		dst  *string
	}{{"password_file", in.PasswordFile, &in.Password}, {"api_key_file", in.APIKeyFile, &in.APIKey}} {
		if f.path == "" {
			continue
		}
		b, err := os.ReadFile(f.path)
		if err != nil {
			return in, fmt.Errorf("reading %s: %w", f.key, err)
		}
		*f.dst = strings.TrimSpace(string(b))
	}
	return in, nil
}

// Resolve returns a copy ready to use: ${VAR} references expanded and token
// files read. Used to test notifiers that are not saved yet.
func (n NotificationConfig) Resolve() (NotificationConfig, error) {
	n.BotToken = os.ExpandEnv(n.BotToken)
	n.ChatID = os.ExpandEnv(n.ChatID)
	n.URL = os.ExpandEnv(n.URL)
	n.Token = os.ExpandEnv(n.Token)
	for _, f := range []struct {
		key  string
		path string
		dst  *string
	}{{"bot_token_file", n.BotTokenFile, &n.BotToken}, {"token_file", n.TokenFile, &n.Token}} {
		if f.path == "" {
			continue
		}
		b, err := os.ReadFile(f.path)
		if err != nil {
			return n, fmt.Errorf("reading %s: %w", f.key, err)
		}
		*f.dst = strings.TrimSpace(string(b))
	}
	return n, nil
}
