package main

import (
	"database/sql"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/blend/go-sdk/db"
	"github.com/blend/go-sdk/db/migration"
	"github.com/blend/go-sdk/logger"
	"github.com/blend/go-sdk/util"
	"github.com/blend/go-sdk/uuid"
	"github.com/jackc/pgx"
)

const (
	createCount    = 1 << 10
	selectCount    = 1024
	iterationCount = 128
	threadCount    = 32
)

const (
	selectQuery = `SELECT * FROM test_object`
)

func newTestObject() *testObject {
	return &testObject{
		UUID:       uuid.V4().String(),
		CreatedUTC: time.Now().UTC(),
		Active:     true,
		Name:       uuid.V4().String(),
		Variance:   rand.Float64(),
	}
}

type testObject struct {
	ID         int        `db:"id,pk,serial"`
	UUID       string     `db:"uuid"`
	CreatedUTC time.Time  `db:"created_utc"`
	UpdatedUTC *time.Time `db:"updated_utc"`
	Active     bool       `db:"active"`
	Name       string     `db:"name"`
	Variance   float64    `db:"variance"`
}

func (to *testObject) Populate(rows *sql.Rows) error {
	return rows.Scan(&to.ID, &to.UUID, &to.CreatedUTC, &to.UpdatedUTC, &to.Active, &to.Name, &to.Variance)
}

func (to *testObject) PGXPopulate(rows *pgx.Rows) error {
	return rows.Scan(&to.ID, &to.UUID, &to.CreatedUTC, &to.UpdatedUTC, &to.Active, &to.Name, &to.Variance)
}

func (to testObject) TableName() string {
	return "test_object"
}

func createTable() error {
	return migration.New(
		migration.NewStep(
			migration.TableExists("test_object"),
			migration.Statements(
				`DROP TABLE IF EXISTS test_object`,
			),
		),
		migration.NewStep(
			migration.TableNotExists("test_object"),
			migration.Statements("CREATE TABLE test_object (id serial not null, uuid varchar(64) not null, created_utc timestamp not null, updated_utc timestamp, active boolean, name varchar(64), variance float)"),
		),
	).WithLabel("drop && create `test_object` table").WithLogger(migration.NewLogger(logger.All())).Apply(db.Default())
}

func dropTable() error {
	return migration.New(
		migration.NewStep(
			migration.TableExists("test_object"),
			migration.Statements(
				`DROP TABLE IF EXISTS test_object`,
			),
		),
	).WithLabel("drop `test_object` table").WithLogger(migration.NewLogger(logger.All())).Apply(db.Default())
}

func seedObjects(count int) error {
	var err error
	for x := 0; x < count; x++ {
		err = db.Default().Create(newTestObject())
		if err != nil {
			return err
		}
	}
	return nil
}

func baselineAccess(db *db.Connection, queryLimit int) ([]testObject, error) {
	var results []testObject
	var err error

	stmt, err := db.Connection.Prepare(selectQuery)
	if err != nil {
		return results, err
	}

	res, err := stmt.Query()
	if err != nil {
		return results, err
	}

	if res.Err() != nil {
		return results, res.Err()
	}

	for res.Next() {
		to := newTestObject()
		err = to.Populate(res)
		if err != nil {
			return results, err
		}
		results = append(results, *to)
	}

	return results, nil
}

func dbAccess(db *db.Connection, queryLimit int) ([]testObject, error) {
	var results []testObject
	err := db.GetAll(&results)
	return results, err
}

func benchHarness(db *db.Connection, parallelism int, queryLimit int, accessFunc func(*db.Connection, int) ([]testObject, error)) ([]time.Duration, error) {
	var durations []time.Duration
	var waitHandle = sync.WaitGroup{}
	var errors = make(chan error, parallelism)

	waitHandle.Add(parallelism)
	for threadID := 0; threadID < parallelism; threadID++ {
		go func() {
			defer waitHandle.Done()

			for iteration := 0; iteration < iterationCount; iteration++ {
				start := time.Now()
				items, err := accessFunc(db, queryLimit)
				if err != nil {
					errors <- err
					return
				}

				durations = append(durations, time.Since(start))

				if len(items) < queryLimit {
					errors <- fmt.Errorf("Returned item count less than %d", queryLimit)
					return
				}

				if len(items[len(items)>>1].UUID) == 0 {
					errors <- fmt.Errorf("Returned items have empty `UUID` fields")
					return
				}

				if len(items[len(items)>>1].Name) == 0 {
					errors <- fmt.Errorf("Returned items have empty `Name` fields")
					return
				}

				if items[len(items)>>1].Variance == 0 {
					errors <- fmt.Errorf("Returned items have empty `Variance`")
					return
				}

				if items[0].UUID == items[len(items)>>1].UUID {
					errors <- fmt.Errorf("UUIDs are equal between records")
					return
				}
			}
		}()
	}
	waitHandle.Wait()

	if len(errors) > 0 {
		return durations, <-errors
	}
	return durations, nil
}

func pgxFetchItems(pool *pgx.ConnPool) ([]testObject, error) {
	conn, err := pool.Acquire()
	if err != nil {
		return nil, err
	}
	defer pool.Release(conn)

	var items []testObject
	rows, err := conn.Query(selectQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		to := newTestObject()
		err = to.PGXPopulate(rows)
		if err != nil {
			return nil, err
		}

		items = append(items, *to)
	}
	return items, nil
}

func benchPGX(pool *pgx.ConnPool, parallelism int, queryLimit int) ([]time.Duration, error) {
	var durations []time.Duration
	var waitHandle = sync.WaitGroup{}
	var errors = make(chan error, parallelism)

	waitHandle.Add(parallelism)
	for threadID := 0; threadID < parallelism; threadID++ {
		go func() {
			defer waitHandle.Done()

			for iteration := 0; iteration < iterationCount; iteration++ {
				start := time.Now()

				items, err := pgxFetchItems(pool)
				if err != nil {
					errors <- err
					return
				}

				durations = append(durations, time.Since(start))

				if len(items) < queryLimit {
					errors <- fmt.Errorf("Returned item count less than %d", queryLimit)
					return
				}

				if len(items[len(items)>>1].UUID) == 0 {
					errors <- fmt.Errorf("Returned items have empty `UUID` fields")
					return
				}

				if len(items[len(items)>>1].Name) == 0 {
					errors <- fmt.Errorf("Returned items have empty `Name` fields")
					return
				}

				if items[len(items)>>1].Variance == 0 {
					errors <- fmt.Errorf("Returned items have empty `Variance`")
					return
				}

				if items[0].UUID == items[len(items)>>1].UUID {
					errors <- fmt.Errorf("UUIDs are equal between records")
					return
				}
			}
		}()
	}
	waitHandle.Wait()

	if len(errors) > 0 {
		return durations, <-errors
	}

	return durations, nil
}

func main() {
	log := logger.All()

	// default db is used by the migration framework to build the test database
	// it is not used by the benchmarks.
	err := db.OpenDefault(db.NewFromEnv())
	if err != nil {
		log.Fatal(err)
	}

	err = createTable()
	if err != nil {
		log.Fatal(err)
	}
	defer dropTable()

	err = seedObjects(createCount)
	if err != nil {
		log.SyncFatalExit(err)
	}

	log.SyncInfof("Finished seeding objects, starting load test.")

	var pool *pgx.ConnPool
	var config pgx.ConnPoolConfig
	config.MaxConnections = threadCount

	pool, err = pgx.NewConnPool(config)
	if err != nil {
		log.SyncFatalExit(err)
	}

	pgxStart := time.Now()
	pgxTimings, err := benchPGX(pool, threadCount, selectCount)
	if err != nil {
		log.SyncFatalExit(err)
	}
	log.SyncInfof("PGX Elapsed: %v", time.Since(pgxStart))

	// do go-sdk/db query
	uncached := db.NewFromEnv()
	uncached.DisableStatementCache()
	conn, err := uncached.Open()
	if err != nil {
		log.SyncFatalExit(err)
	}
	conn.Connection.SetMaxOpenConns(threadCount)
	conn.Connection.SetMaxIdleConns(threadCount)

	dbStart := time.Now()
	dbTimings, err := benchHarness(uncached, threadCount, selectCount, dbAccess)
	if err != nil {
		log.Fatal(err)
	}
	log.SyncInfof("go-sdk/db Elapsed: %v", time.Since(dbStart))

	// do spiffy query
	cached := db.NewFromEnv()
	cached.EnableStatementCache()
	conn, err = cached.Open()
	if err != nil {
		log.SyncFatalExit(err)
	}
	conn.Connection.SetMaxOpenConns(threadCount)
	conn.Connection.SetMaxIdleConns(threadCount)

	dbCachedStart := time.Now()
	dbCachedTimings, err := benchHarness(cached, threadCount, selectCount, dbAccess)
	if err != nil {
		log.SyncFatalExit(err)
	}
	log.SyncInfof("go-sdk/db (Statement Cache) Elapsed: %v", time.Since(dbCachedStart))

	// do baseline query
	baselineStart := time.Now()
	baseline := db.NewFromEnv()
	conn, err = baseline.Open()
	if err != nil {
		log.SyncFatalExit(err)
	}
	conn.Connection.SetMaxOpenConns(threadCount)
	conn.Connection.SetMaxIdleConns(threadCount)

	baselineTimings, err := benchHarness(baseline, threadCount, selectCount, baselineAccess)
	if err != nil {
		log.SyncFatalExit(err)
	}
	log.SyncInfof("Baseline Elapsed: %v", time.Since(baselineStart))

	fmt.Println()
	fmt.Println()

	fmt.Println("Detailed Results:")
	fmt.Printf("\tAvg Baseline                    : %v\n", util.Math.MeanOfDuration(baselineTimings))
	fmt.Printf("\tAvg go-sdk/db                   : %v\n", util.Math.MeanOfDuration(dbTimings))
	fmt.Printf("\tAvg go-sdk/db (Statement Cache) : %v\n", util.Math.MeanOfDuration(dbCachedTimings))
	fmt.Printf("\tAvg PGX                         : %v\n", util.Math.MeanOfDuration(pgxTimings))

	fmt.Println()

	fmt.Printf("\t99th Baseline                 : %v\n", util.Math.PercentileOfDuration(baselineTimings, 99.0))
	fmt.Printf("\t99th Spiffy                   : %v\n", util.Math.PercentileOfDuration(dbTimings, 99.0))
	fmt.Printf("\t99th Spiffy (Statement Cache) : %v\n", util.Math.PercentileOfDuration(dbCachedTimings, 99.0))
	fmt.Printf("\t99th PGX                      : %v\n", util.Math.PercentileOfDuration(pgxTimings, 99.0))
}
