package database

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/benjaco/devflow/pkg/api"
)

type postgresCommandConnection struct {
	URL             string
	URLWithUsername string
	Host            string
	Port            string
	Database        string
	User            string
	Password        string
}

func withPostgresCommandCredentials(rawURL string, fallback api.DBInstance, base map[string]string, fn func(map[string]string, string) error) error {
	connection, err := parsePostgresCommandConnection(rawURL, fallback)
	if err != nil {
		return err
	}
	passfile, err := os.CreateTemp("", "devflow-pgpass-*")
	if err != nil {
		return fmt.Errorf("create temporary Postgres password file: %w", err)
	}
	passPath := passfile.Name()
	defer os.Remove(passPath)
	if err := passfile.Chmod(0o600); err != nil {
		_ = passfile.Close()
		return fmt.Errorf("secure temporary Postgres password file: %w", err)
	}
	entry, err := postgresPassEntry(connection)
	if err != nil {
		_ = passfile.Close()
		return err
	}
	if _, err := passfile.WriteString(entry); err != nil {
		_ = passfile.Close()
		return fmt.Errorf("write temporary Postgres password file: %w", err)
	}
	if err := passfile.Close(); err != nil {
		return fmt.Errorf("close temporary Postgres password file: %w", err)
	}

	env, err := sanitizePostgresURLs(base)
	if err != nil {
		return err
	}
	env["PGPASSFILE"] = passPath
	env["PGPASSWORD"] = ""
	env["DATABASE_URL"] = connection.URL
	setEnvIfNotEmpty(env, "PGHOST", connection.Host)
	setEnvIfNotEmpty(env, "PGPORT", connection.Port)
	setEnvIfNotEmpty(env, "PGDATABASE", connection.Database)
	setEnvIfNotEmpty(env, "PGUSER", connection.User)
	return fn(env, connection.URL)
}

func parsePostgresCommandConnection(rawURL string, fallback api.DBInstance) (postgresCommandConnection, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		// net/url parse errors can embed the full input URL. Do not copy a
		// possibly credential-bearing URL into task errors or logs.
		return postgresCommandConnection{}, fmt.Errorf("parse Postgres connection URL: invalid URL")
	}
	if parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		return postgresCommandConnection{}, fmt.Errorf("Postgres connection URL must use postgres or postgresql scheme")
	}
	queryPassword, hasQueryPassword, err := sanitizePostgresQuery(parsed)
	if err != nil {
		return postgresCommandConnection{}, err
	}
	connection := postgresCommandConnection{
		Host:     firstNonEmptyDatabase(parsed.Hostname(), fallback.Host),
		Port:     firstNonEmptyDatabase(parsed.Port(), positivePort(fallback.Port), "5432"),
		Database: firstNonEmptyDatabase(strings.TrimPrefix(parsed.EscapedPath(), "/"), fallback.Name),
		User:     fallback.User,
		Password: fallback.Password,
	}
	if decoded, decodeErr := url.PathUnescape(connection.Database); decodeErr == nil {
		connection.Database = decoded
	}
	if parsed.User != nil {
		if username := parsed.User.Username(); username != "" {
			connection.User = username
		}
		if password, ok := parsed.User.Password(); ok {
			connection.Password = password
		}
	}
	// libpq applies URI query parameters after userinfo, so an explicitly
	// supplied query password wins even when it is empty.
	if hasQueryPassword {
		connection.Password = queryPassword
	}
	parsed.User = nil
	connection.URL = parsed.String()
	if connection.User != "" {
		parsed.User = url.User(connection.User)
	}
	connection.URLWithUsername = parsed.String()
	return connection, nil
}

func postgresPassEntry(connection postgresCommandConnection) (string, error) {
	fields := []string{
		firstNonEmptyDatabase(connection.Host, "*"),
		firstNonEmptyDatabase(connection.Port, "*"),
		firstNonEmptyDatabase(connection.Database, "*"),
		firstNonEmptyDatabase(connection.User, "*"),
		connection.Password,
	}
	for index, field := range fields {
		if strings.ContainsAny(field, "\r\n") {
			return "", fmt.Errorf("Postgres password-file field %d contains a newline", index+1)
		}
		fields[index] = pgpassEscape(field)
	}
	return strings.Join(fields, ":") + "\n", nil
}

func sanitizePostgresURLs(values map[string]string) (map[string]string, error) {
	out := mergeStringMaps(values, nil)
	if out == nil {
		out = map[string]string{}
	}
	// process.Run inherits the caller environment before applying overrides.
	// Add sanitized overrides for any ambient Postgres URL even when the
	// adapter did not copy that key into PrepareOptions.Env.
	for _, item := range os.Environ() {
		key, value, ok := strings.Cut(item, "=")
		if !ok || !isPostgresURL(value) {
			continue
		}
		if _, configured := out[key]; !configured {
			out[key] = value
		}
	}
	for key, value := range out {
		if !isPostgresURL(value) {
			continue
		}
		connection, err := parsePostgresCommandConnection(value, api.DBInstance{})
		if err != nil {
			return nil, fmt.Errorf("sanitize Postgres URL environment variable %q: %w", key, err)
		}
		out[key] = connection.URL
	}
	return out, nil
}

// sanitizePostgresQuery extracts the one URI query credential that Devflow
// can transport safely and removes every occurrence before the URL is exposed
// to a callback, process environment, or Docker exec configuration. Values
// are decoded exactly once with URL query semantics.
func sanitizePostgresQuery(parsed *url.URL) (password string, present bool, err error) {
	if parsed.RawQuery == "" {
		return "", false, nil
	}
	for _, field := range strings.Split(parsed.RawQuery, "&") {
		if field == "" {
			continue
		}
		rawKey, rawValue, _ := strings.Cut(field, "=")
		key, decodeErr := url.QueryUnescape(rawKey)
		if decodeErr != nil {
			return "", false, fmt.Errorf("parse Postgres connection URL query: invalid encoding")
		}
		switch strings.ToLower(key) {
		case "password":
			value, decodeErr := url.QueryUnescape(rawValue)
			if decodeErr != nil {
				return "", false, fmt.Errorf("parse Postgres connection URL query: invalid credential encoding")
			}
			password, present = value, true
		case "sslpassword", "oauth_client_secret", "passfile":
			return "", false, fmt.Errorf("Postgres connection URL contains unsupported credential parameter %q", strings.ToLower(key))
		}
	}
	query, parseErr := url.ParseQuery(parsed.RawQuery)
	if parseErr != nil {
		return "", false, fmt.Errorf("parse Postgres connection URL query: invalid query")
	}
	for key := range query {
		if strings.EqualFold(key, "password") {
			delete(query, key)
		}
	}
	parsed.RawQuery = query.Encode()
	return password, present, nil
}

func isPostgresURL(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(value, "postgres://") || strings.HasPrefix(value, "postgresql://")
}

func pgpassEscape(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	return strings.ReplaceAll(value, ":", `\:`)
}

func positivePort(port int) string {
	if port <= 0 {
		return ""
	}
	return strconv.Itoa(port)
}

func setEnvIfNotEmpty(env map[string]string, key, value string) {
	if value != "" {
		env[key] = value
	}
}
