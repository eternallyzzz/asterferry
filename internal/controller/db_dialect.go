package controller

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
)

const postgresDriverName = "asterferry-postgres"

var registerPostgresDriverOnce sync.Once

// databaseBackend identifies the storage implementation without exposing a
// driver-specific type to the rest of the Controller. The logical schema and
// Store methods remain shared by both backends.
type databaseBackend string

const (
	databaseBackendSQLite   databaseBackend = DatabaseDriverSQLite
	databaseBackendPostgres databaseBackend = DatabaseDriverPostgres
)

func openConfiguredDatabase(ctx context.Context, config Config) (*sql.DB, databaseBackend, error) {
	backend, err := validateDatabaseConfig(config)
	if err != nil {
		return nil, "", err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	switch backend {
	case databaseBackendSQLite:
		path := strings.TrimSpace(config.DatabasePath)
		dsn := path
		if path != ":memory:" && !strings.HasPrefix(path, "file:") {
			abs, err := filepath.Abs(path)
			if err != nil {
				return nil, "", fmt.Errorf("resolve database path: %w", err)
			}
			if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
				return nil, "", fmt.Errorf("create database directory: %w", err)
			}
			dsn = abs
		}
		db, err := sql.Open("sqlite", sqliteDSN(dsn))
		if err != nil {
			return nil, "", err
		}
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		return db, backend, nil
	case databaseBackendPostgres:
		registerPostgresDriverOnce.Do(func() {
			sql.Register(postgresDriverName, &questionMarkPostgresDriver{inner: stdlib.GetDefaultDriver()})
		})
		db, err := sql.Open(postgresDriverName, strings.TrimSpace(config.DatabaseURL))
		if err != nil {
			return nil, "", err
		}
		maxOpen := databasePoolSize(config)
		db.SetMaxOpenConns(maxOpen)
		db.SetMaxIdleConns(minInt(4, maxOpen))
		pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := db.PingContext(pingCtx); err != nil {
			_ = db.Close()
			return nil, "", errors.New("connect to postgres: " + err.Error())
		}
		return db, backend, nil
	default:
		return nil, "", errors.New("unsupported database backend")
	}
}

func validateDatabaseConfig(config Config) (databaseBackend, error) {
	backend := databaseBackend(normalizeDatabaseDriver(config.DatabaseDriver))
	switch backend {
	case databaseBackendSQLite:
		if strings.TrimSpace(config.DatabasePath) == "" {
			return "", errors.New("controller database_path is required for sqlite")
		}
		if strings.TrimSpace(config.DatabaseURL) != "" {
			return "", errors.New("controller database_url is only valid for postgres")
		}
	case databaseBackendPostgres:
		value := strings.TrimSpace(config.DatabaseURL)
		if value == "" {
			return "", errors.New("controller database_url is required for postgres")
		}
		if len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") {
			return "", errors.New("controller database_url is invalid")
		}
	default:
		return "", errors.New("controller database_driver must be sqlite or postgres")
	}
	if config.DatabaseMaxOpenConns < 0 || config.DatabaseMaxOpenConns > maxDatabasePoolSize {
		return "", errors.New("controller database_max_open_conns is out of range")
	}
	return backend, nil
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func redactPostgresURL(value string) string {
	value = strings.TrimSpace(value)
	parsed, err := url.Parse(value)
	if err != nil || (!strings.EqualFold(parsed.Scheme, "postgres") && !strings.EqualFold(parsed.Scheme, "postgresql")) {
		return value
	}
	if parsed.User != nil {
		parsed.User = url.User(parsed.User.Username())
	}
	query := parsed.Query()
	for _, key := range []string{"password"} {
		if query.Has(key) {
			query.Set(key, "REDACTED")
		}
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// questionMarkPostgresDriver keeps the existing Store SQL readable while
// making SQLite-style positional placeholders work with PostgreSQL's $N
// syntax. Queries are rebound before they reach the pgx driver, including
// queries executed inside database/sql transactions.
type questionMarkPostgresDriver struct {
	inner driver.Driver
}

func (d *questionMarkPostgresDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &questionMarkPostgresConn{inner: conn}, nil
}

func (d *questionMarkPostgresDriver) OpenConnector(name string) (driver.Connector, error) {
	contextDriver, ok := d.inner.(driver.DriverContext)
	if !ok {
		return &questionMarkPostgresConnector{driver: d, inner: &openConnector{driver: d, name: name}}, nil
	}
	connector, err := contextDriver.OpenConnector(name)
	if err != nil {
		return nil, err
	}
	return &questionMarkPostgresConnector{driver: d, inner: connector}, nil
}

type openConnector struct {
	driver driver.Driver
	name   string
}

func (c *openConnector) Connect(_ context.Context) (driver.Conn, error) {
	return c.driver.Open(c.name)
}

func (c *openConnector) Driver() driver.Driver { return c.driver }

type questionMarkPostgresConnector struct {
	driver driver.Driver
	inner  driver.Connector
}

func (c *questionMarkPostgresConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &questionMarkPostgresConn{inner: conn}, nil
}

func (c *questionMarkPostgresConnector) Driver() driver.Driver { return c.driver }

type questionMarkPostgresConn struct {
	inner driver.Conn
}

func (c *questionMarkPostgresConn) Prepare(query string) (driver.Stmt, error) {
	return c.inner.Prepare(bindPostgresPlaceholders(query))
}

func (c *questionMarkPostgresConn) Close() error { return c.inner.Close() }

func (c *questionMarkPostgresConn) Begin() (driver.Tx, error) { return c.inner.Begin() }

func (c *questionMarkPostgresConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	query = bindPostgresPlaceholders(query)
	if preparer, ok := c.inner.(driver.ConnPrepareContext); ok {
		return preparer.PrepareContext(ctx, query)
	}
	return c.inner.Prepare(query)
}

func (c *questionMarkPostgresConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	execer, ok := c.inner.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return execer.ExecContext(ctx, bindPostgresPlaceholders(query), args)
}

func (c *questionMarkPostgresConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	queryer, ok := c.inner.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return queryer.QueryContext(ctx, bindPostgresPlaceholders(query), args)
}

func (c *questionMarkPostgresConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	beginner, ok := c.inner.(driver.ConnBeginTx)
	if !ok {
		return nil, driver.ErrSkip
	}
	return beginner.BeginTx(ctx, options)
}

func (c *questionMarkPostgresConn) Ping(ctx context.Context) error {
	pinger, ok := c.inner.(driver.Pinger)
	if !ok {
		return nil
	}
	return pinger.Ping(ctx)
}

func (c *questionMarkPostgresConn) ResetSession(ctx context.Context) error {
	resetter, ok := c.inner.(driver.SessionResetter)
	if !ok {
		return nil
	}
	return resetter.ResetSession(ctx)
}

func (c *questionMarkPostgresConn) IsValid() bool {
	validator, ok := c.inner.(driver.Validator)
	if !ok {
		return true
	}
	return validator.IsValid()
}

func (c *questionMarkPostgresConn) CheckNamedValue(value *driver.NamedValue) error {
	checker, ok := c.inner.(driver.NamedValueChecker)
	if !ok {
		return nil
	}
	return checker.CheckNamedValue(value)
}

// bindPostgresPlaceholders converts only question marks in SQL code. Literal
// strings, quoted identifiers, comments and backtick-quoted SQLite identifiers
// are left untouched so the adapter cannot corrupt JSON or diagnostic text.
func bindPostgresPlaceholders(query string) string {
	var builder strings.Builder
	builder.Grow(len(query) + 16)
	argument := 0
	const (
		normalState byte = iota
		singleQuoteState
		doubleQuoteState
		backtickState
		lineCommentState
		blockCommentState
	)
	state := normalState
	for index := 0; index < len(query); index++ {
		value := query[index]
		switch state {
		case singleQuoteState, doubleQuoteState, backtickState:
			builder.WriteByte(value)
			quote := byte('\'')
			if state == doubleQuoteState {
				quote = '"'
			} else if state == backtickState {
				quote = '`'
			}
			if value == '\\' && index+1 < len(query) {
				builder.WriteByte(query[index+1])
				index++
			} else if value == quote {
				if index+1 < len(query) && query[index+1] == quote {
					builder.WriteByte(query[index+1])
					index++
				} else {
					state = normalState
				}
			}
		case lineCommentState:
			builder.WriteByte(value)
			if value == '\n' || value == '\r' {
				state = normalState
			}
		case blockCommentState:
			builder.WriteByte(value)
			if value == '*' && index+1 < len(query) && query[index+1] == '/' {
				builder.WriteByte(query[index+1])
				index++
				state = normalState
			}
		default:
			switch value {
			case '\'', '"', '`':
				builder.WriteByte(value)
				if value == '\'' {
					state = singleQuoteState
				} else if value == '"' {
					state = doubleQuoteState
				} else {
					state = backtickState
				}
			case '-', '/':
				if index+1 < len(query) && ((value == '-' && query[index+1] == '-') || (value == '/' && query[index+1] == '*')) {
					builder.WriteByte(value)
					builder.WriteByte(query[index+1])
					index++
					if value == '-' {
						state = lineCommentState
					} else {
						state = blockCommentState
					}
				} else {
					builder.WriteByte(value)
				}
			case '?':
				argument++
				builder.WriteByte('$')
				builder.WriteString(itoa(argument))
			default:
				builder.WriteByte(value)
			}
		}
	}
	return builder.String()
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	index := len(digits)
	for value > 0 {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[index:])
}
