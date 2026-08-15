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

	env := sanitizePostgresURLs(base)
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

func sanitizePostgresURLs(values map[string]string) map[string]string {
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
		parsed, err := url.Parse(strings.TrimSpace(value))
		if err != nil {
			out[key] = ""
			continue
		}
		parsed.User = nil
		out[key] = parsed.String()
	}
	return out
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
