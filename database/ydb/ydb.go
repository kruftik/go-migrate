package ydb

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.uber.org/atomic"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	"github.com/golang-migrate/migrate/v4/database/multistmt"
	"github.com/hashicorp/go-multierror"

	ydbsql "github.com/ydb-platform/ydb-go-sql"
)

var (
	multiStmtDelimiter = []byte(";")

	DefaultMigrationsTable       = "schema_migrations"
	DefaultMultiStatementMaxSize = 10 * 1 << 20 // 10 MB

	ErrNilConfig = fmt.Errorf("no config")
)

type Config struct {
	MigrationsTable       string
	MultiStatementEnabled bool
	MultiStatementMaxSize int
}

func init() {
	database.Register("ydb", &YDB{})
}

func WithInstance(conn *sql.DB, config *Config) (database.Driver, error) {
	if config == nil {
		return nil, ErrNilConfig
	}

	if err := conn.Ping(); err != nil {
		return nil, err
	}

	db := &YDB{
		conn:   conn,
		config: config,
	}

	if err := db.init(); err != nil {
		return nil, err
	}

	return db, nil
}

type YDB struct {
	conn     *sql.DB
	config   *Config
	isLocked atomic.Bool
}

func (db *YDB) Open(dsn string) (database.Driver, error) {
	purl, err := url.Parse(dsn)
	if err != nil {
		return nil, err
	}

	q := migrate.FilterCustomQuery(purl)
	conn, err := sql.Open("ydb", q.String())
	if err != nil {
		return nil, err
	}

	multiStatementMaxSize := DefaultMultiStatementMaxSize
	if s := purl.Query().Get("x-multi-statement-max-size"); len(s) > 0 {
		multiStatementMaxSize, err = strconv.Atoi(s)
		if err != nil {
			return nil, err
		}
	}

	db = &YDB{
		conn: conn,
		config: &Config{
			MigrationsTable:       purl.Query().Get("x-migrations-table"),
			MultiStatementEnabled: purl.Query().Get("x-multi-statement") == "true",
			MultiStatementMaxSize: multiStatementMaxSize,
		},
	}

	if err := db.init(); err != nil {
		return nil, err
	}

	return db, nil
}

func (db *YDB) init() error {
	if len(db.config.MigrationsTable) == 0 {
		db.config.MigrationsTable = DefaultMigrationsTable
	}

	if db.config.MultiStatementMaxSize <= 0 {
		db.config.MultiStatementMaxSize = DefaultMultiStatementMaxSize
	}

	return db.ensureVersionTable()
}

func (db *YDB) execMigration(migration string) error {
	tm := strings.TrimSpace(migration)
	if tm == "" {
		return nil
	}

	ctx := context.Background()
	tmu := strings.ToUpper(tm)
	if strings.HasPrefix(tmu, "CREATE") || strings.HasPrefix(tmu, "ALTER") || strings.HasPrefix(tmu, "DROP") {
		ctx = ydbsql.WithSchemeQuery(ctx)
	}

	_, err := db.conn.ExecContext(ctx, migration)
	return err
}

func (db *YDB) Run(r io.Reader) error {
	if db.config.MultiStatementEnabled {
		var err error
		if e := multistmt.Parse(r, multiStmtDelimiter, db.config.MultiStatementMaxSize, func(m []byte) bool {
			if e := db.execMigration(string(m)); e != nil {
				err = database.Error{OrigErr: e, Err: "migration failed", Query: m}
				return false
			}
			return true
		}); e != nil {
			return e
		}
		return err
	}

	migration, err := ioutil.ReadAll(r)
	if err != nil {
		return err
	}

	if err = db.execMigration(string(migration)); err != nil {
		return database.Error{OrigErr: err, Err: "migration failed", Query: migration}
	}

	return nil
}
func (db *YDB) Version() (int, bool, error) {
	var (
		sequence uint64
		version  int64
		dirty    uint8
		query    = "SELECT sequence, version, dirty FROM `" + db.config.MigrationsTable + "` ORDER BY sequence DESC LIMIT 1"
	)
	if err := db.conn.QueryRow(query).Scan(&sequence, &version, &dirty); err != nil {
		if err == sql.ErrNoRows {
			return database.NilVersion, false, nil
		}
		return 0, false, &database.Error{OrigErr: err, Query: []byte(query)}
	}
	return int(version), dirty == 1, nil
}

func (db *YDB) SetVersion(version int, dirty bool) error {
	var (
		bool = func(v bool) uint8 {
			if v {
				return 1
			}
			return 0
		}
		tx, err = db.conn.Begin()
	)
	if err != nil {
		return err
	}

	query := "INSERT INTO " + db.config.MigrationsTable + " (sequence, version, dirty) VALUES ($sequence, $version, $dirty)"
	if _, err := tx.Exec(query, sql.Named("sequence", time.Now().UnixNano()), sql.Named("version", int64(version)), sql.Named("dirty", bool(dirty))); err != nil {
		return &database.Error{OrigErr: err, Query: []byte(query)}
	}

	return tx.Commit()
}

// migrationTableExists checks if migration table exists
// returns nil, if table exists
func (db *YDB) migrationTableExists() error {
	var (
		table string
		query = "SELECT DISTINCT Path FROM `.sys/partition_stats` WHERE Path LIKE '" + db.config.MigrationsTable + "'"
	)

	if err := db.conn.QueryRowContext(ydbsql.WithScanQuery(context.Background()), query).Scan(&table); err != nil {
		if err != sql.ErrNoRows {
			return &database.Error{OrigErr: err, Query: []byte(query)}
		}
	}

	return nil
}

// ensureVersionTable checks if versions table exists and, if not, creates it.
// Note that this function locks the database, which deviates from the usual
// convention of "caller locks" in the ClickHouse type.
func (db *YDB) ensureVersionTable() (err error) {
	if err = db.Lock(); err != nil {
		return err
	}

	defer func() {
		if e := db.Unlock(); e != nil {
			if err == nil {
				err = e
			} else {
				err = multierror.Append(err, e)
			}
		}
	}()

	if err := db.migrationTableExists(); err != nil {
		return err
	}

	// if not, create the empty migration table
	query := fmt.Sprintf(`
		CREATE TABLE %s (
			sequence   UInt64,
			version    Int64,
			dirty      UInt8,
			PRIMARY KEY(sequence)
		)`, db.config.MigrationsTable)

	if _, err := db.conn.ExecContext(ydbsql.WithSchemeQuery(context.Background()), query); err != nil {
		return &database.Error{OrigErr: err, Query: []byte(query)}
	}
	return nil
}

func (db *YDB) Drop() (err error) {
	query := "SELECT DISTINCT Path FROM `.sys/partition_stats`"
	tables, err := db.conn.QueryContext(ydbsql.WithScanQuery(context.Background()), query)

	if err != nil {
		return &database.Error{OrigErr: err, Query: []byte(query)}
	}
	defer func() {
		if errClose := tables.Close(); errClose != nil {
			err = multierror.Append(err, errClose)
		}
	}()

	for tables.Next() {
		var table string
		if err := tables.Scan(&table); err != nil {
			return err
		}

		query = "DROP TABLE " + table

		if _, err := db.conn.ExecContext(ydbsql.WithSchemeQuery(context.Background()), query); err != nil {
			return &database.Error{OrigErr: err, Query: []byte(query)}
		}
	}
	if err := tables.Err(); err != nil {
		return &database.Error{OrigErr: err, Query: []byte(query)}
	}

	return nil
}

func (db *YDB) Lock() error {
	if !db.isLocked.CAS(false, true) {
		return database.ErrLocked
	}

	return nil
}
func (db *YDB) Unlock() error {
	if !db.isLocked.CAS(true, false) {
		return database.ErrNotLocked
	}

	return nil
}
func (db *YDB) Close() error { return db.conn.Close() }
