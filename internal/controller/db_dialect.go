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

const (
	postgresDriverName      = "asterferry-postgres"
	postgresConnMaxLifetime = 30 * time.Minute
	postgresConnMaxIdleTime = 5 * time.Minute
)

var registerPostgresDriverOnce sync.Once

// databaseBackend identifies the storage implementation without exposing a
// driver-specific type to the rest of the Controller. The logical schema and
// Store methods remain shared by both backends.
type databaseBackend string

const (
	databaseBackendSQLite   databaseBackend = DatabaseDriverSQLite
	databaseBackendPostgres databaseBackend = DatabaseDriverPostgres
)

// schemaTypes contains the small set of DDL differences between the supported
// databases. Table definitions stay shared so a new column cannot silently be
// added to only one backend.
type schemaTypes struct {
	integer    string
	bigInteger string
	blob       string
	real       string
	autoID     string
}

// databaseDialect is the Controller's internal database contract. Store code
// continues to use SQLite-style '?' arguments; the PostgreSQL driver adapter
// binds them at the driver boundary.
type databaseDialect interface {
	backend() databaseBackend
	bind(query string) string
	forUpdateSuffix() string
	schemaTypes() schemaTypes
	relationCountQuery() string
	tableExistsQuery() string
	columnExistsQuery() string
	indexExistsQuery() string
}

type sqliteDialect struct{}

func (sqliteDialect) backend() databaseBackend { return databaseBackendSQLite }
func (sqliteDialect) bind(query string) string { return query }
func (sqliteDialect) forUpdateSuffix() string  { return "" }
func (sqliteDialect) schemaTypes() schemaTypes {
	return schemaTypes{integer: "INTEGER", bigInteger: "INTEGER", blob: "BLOB", real: "REAL", autoID: "INTEGER PRIMARY KEY AUTOINCREMENT"}
}
func (sqliteDialect) relationCountQuery() string {
	return `SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`
}
func (sqliteDialect) tableExistsQuery() string {
	return `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`
}
func (sqliteDialect) columnExistsQuery() string {
	return `SELECT count(*) FROM pragma_table_info(?) WHERE name=?`
}
func (sqliteDialect) indexExistsQuery() string {
	return `SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`
}

type postgresDialect struct{}

func (postgresDialect) backend() databaseBackend { return databaseBackendPostgres }
func (postgresDialect) bind(query string) string { return bindPostgresPlaceholders(query) }
func (postgresDialect) forUpdateSuffix() string  { return " FOR UPDATE" }
func (postgresDialect) schemaTypes() schemaTypes {
	return schemaTypes{integer: "INTEGER", bigInteger: "BIGINT", blob: "BYTEA", real: "DOUBLE PRECISION", autoID: "BIGSERIAL PRIMARY KEY"}
}
func (postgresDialect) relationCountQuery() string {
	return `SELECT count(*) FROM pg_class AS c JOIN pg_namespace AS n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relkind IN ('r','p','v','m','S','f')`
}
func (postgresDialect) tableExistsQuery() string {
	return `SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_type='BASE TABLE' AND table_name=?`
}
func (postgresDialect) columnExistsQuery() string {
	return `SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=? AND column_name=?`
}
func (postgresDialect) indexExistsQuery() string {
	return `SELECT count(*) FROM pg_indexes WHERE schemaname=current_schema() AND indexname=?`
}

func newDatabaseDialect(backend databaseBackend) databaseDialect {
	if backend == databaseBackendPostgres {
		return postgresDialect{}
	}
	return sqliteDialect{}
}

// selectForUpdateClause is deliberately backend-specific. SQLite uses the
// Controller's single-writer connection and does not support PostgreSQL's row
// lock syntax; PostgreSQL needs it for read/modify/write barriers such as the
// two-sided assignment acknowledgement.
func (s *Store) selectForUpdateClause() string {
	if s == nil || s.dialect == nil {
		return ""
	}
	return s.dialect.forUpdateSuffix()
}

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
		// Recycle connections before PostgreSQL or an intermediary load
		// balancer can silently retire a long-lived socket. This bounds both
		// the age of active connections and the time idle connections remain
		// available for reuse.
		db.SetConnMaxLifetime(postgresConnMaxLifetime)
		db.SetConnMaxIdleTime(postgresConnMaxIdleTime)
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
	return c.inner.Prepare((postgresDialect{}).bind(query))
}

func (c *questionMarkPostgresConn) Close() error { return c.inner.Close() }

func (c *questionMarkPostgresConn) Begin() (driver.Tx, error) { return c.inner.Begin() }

func (c *questionMarkPostgresConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	query = (postgresDialect{}).bind(query)
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
	return execer.ExecContext(ctx, (postgresDialect{}).bind(query), args)
}

func (c *questionMarkPostgresConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	queryer, ok := c.inner.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return queryer.QueryContext(ctx, (postgresDialect{}).bind(query), args)
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

// bindPostgresPlaceholders converts only bind markers in SQL code. Literal
// strings, PostgreSQL dollar-quoted bodies, quoted identifiers, comments and
// backtick-quoted SQLite identifiers are left untouched. PostgreSQL JSON
// operators are also preserved, so the adapter has a defined boundary instead
// of relying on every future query author to remember its implementation.
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
		dollarQuoteState
	)
	state := normalState
	dollarDelimiter := ""
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
		case dollarQuoteState:
			if strings.HasPrefix(query[index:], dollarDelimiter) {
				builder.WriteString(dollarDelimiter)
				index += len(dollarDelimiter) - 1
				state = normalState
				dollarDelimiter = ""
			} else {
				builder.WriteByte(value)
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
			case '$':
				if delimiter, ok := postgresDollarQuoteDelimiter(query, index); ok {
					builder.WriteString(delimiter)
					index += len(delimiter) - 1
					dollarDelimiter = delimiter
					state = dollarQuoteState
				} else {
					builder.WriteByte(value)
				}
			case '?':
				if isPostgresQuestionOperator(query, index) {
					builder.WriteByte(value)
					continue
				}
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

func postgresDollarQuoteDelimiter(query string, start int) (string, bool) {
	if start >= len(query) || query[start] != '$' {
		return "", false
	}
	end := start + 1
	if end < len(query) && query[end] == '$' {
		return "$$", true
	}
	if end >= len(query) || !isPostgresDollarTagStart(query[end]) {
		return "", false
	}
	end++
	for end < len(query) && isPostgresDollarTagPart(query[end]) {
		end++
	}
	if end >= len(query) || query[end] != '$' {
		return "", false
	}
	return query[start : end+1], true
}

func isPostgresDollarTagStart(value byte) bool {
	return value == '_' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}

func isPostgresDollarTagPart(value byte) bool {
	return isPostgresDollarTagStart(value) || value >= '0' && value <= '9'
}

func isPostgresQuestionOperator(query string, index int) bool {
	if index+1 < len(query) && (query[index+1] == '|' || query[index+1] == '&') {
		return true
	}
	previous := index - 1
	for previous >= 0 && (query[previous] == ' ' || query[previous] == '\t' || query[previous] == '\r' || query[previous] == '\n') {
		previous--
	}
	next := index + 1
	for next < len(query) && (query[next] == ' ' || query[next] == '\t' || query[next] == '\r' || query[next] == '\n') {
		next++
	}
	if previous < 0 || next >= len(query) {
		return false
	}
	previousValue := query[previous]
	nextValue := query[next]
	previousLooksLikeExpression := previousValue == ')' || previousValue == ']' || previousValue == '\'' || previousValue == '"' || previousValue == '`' || previousValue == '_' || previousValue >= '0' && previousValue <= '9' || previousValue >= 'a' && previousValue <= 'z' || previousValue >= 'A' && previousValue <= 'Z'
	nextLooksLikeOperand := nextValue == '?' || nextValue == '\'' || nextValue == '"' || nextValue == '`' || nextValue == ':' || nextValue == '$' || nextValue == '_' || nextValue >= '0' && nextValue <= '9' || nextValue >= 'a' && nextValue <= 'z' || nextValue >= 'A' && nextValue <= 'Z'
	return previousLooksLikeExpression && nextLooksLikeOperand
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
