package kit

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

const _MIGRATOR_POSTGRES_DSN = "postgresql://%s:%s@%s:%d/%s?sslmode=%s&x-multi-statement=true"

var (
	_MIGRATOR_DEFAULT_MIGRATIONS_PATH     = "./migrations"
	_MIGRATOR_DEFAULT_RETRY_ATTEMPTS      = 1
	_MIGRATOR_DEFAULT_RETRY_INITIAL_DELAY = 0 * time.Second
	_MIGRATOR_DEFAULT_RETRY_LIMIT_DELAY   = 0 * time.Second
	_MIGRATOR_ERR_DB_ALREADY_CLOSED       = regexp.MustCompile(`.*connection is already closed.*`)
)

type MigratorRetryConfig struct {
	Attempts     int
	InitialDelay time.Duration
	LimitDelay   time.Duration
}

type MigratorConfig struct {
	MigrationsPath   *string
	DatabaseHost     string
	DatabasePort     int
	DatabaseSSLMode  string
	DatabaseUser     string
	DatabasePassword string
	DatabaseName     string
}

type Migrator struct {
	config   MigratorConfig
	observer Observer
	migrator *migrate.Migrate
	done     chan struct{}
}

func NewMigrator(ctx context.Context, observer Observer, config MigratorConfig,
	retry *MigratorRetryConfig) (*Migrator, error) {
	if config.MigrationsPath == nil {
		config.MigrationsPath = ptr(_MIGRATOR_DEFAULT_MIGRATIONS_PATH)
	}

	*config.MigrationsPath = fmt.Sprintf("file://%s", filepath.Clean(*config.MigrationsPath))

	if retry == nil {
		retry = &MigratorRetryConfig{
			Attempts:     _MIGRATOR_DEFAULT_RETRY_ATTEMPTS,
			InitialDelay: _MIGRATOR_DEFAULT_RETRY_INITIAL_DELAY,
			LimitDelay:   _MIGRATOR_DEFAULT_RETRY_LIMIT_DELAY,
		}
	}

	dsn := fmt.Sprintf(
		_MIGRATOR_POSTGRES_DSN,
		config.DatabaseUser,
		config.DatabasePassword,
		config.DatabaseHost,
		config.DatabasePort,
		config.DatabaseName,
		config.DatabaseSSLMode,
	)

	var migrator *migrate.Migrate

	// TODO: only retry on specific errors
	err := Utils.Deadline(ctx, func(exceeded <-chan struct{}) error {
		return Utils.ExponentialRetry(
			retry.Attempts, retry.InitialDelay, retry.LimitDelay,
			nil, func(attempt int) error {
				var err error

				observer.Infof(ctx, "Trying to connect to the %s database %d/%d",
					config.DatabaseName, attempt, retry.Attempts)

				migrator, err = migrate.New(*config.MigrationsPath, dsn)
				if err != nil {
					return ErrMigratorGeneric().WrapAs(err)
				}

				return nil
			})
	})
	switch {
	case err == nil:
	case ErrDeadlineExceeded().Is(err):
		return nil, ErrMigratorTimedOut()
	default:
		return nil, ErrMigratorGeneric().Wrap(err)
	}

	observer.Infof(ctx, "Connected to the %s database", config.DatabaseName)

	migrator.Log = _newMigrateLogger(&observer)

	done := make(chan struct{}, 1)
	close(done)

	return &Migrator{
		observer: observer,
		config:   config,
		migrator: migrator,
		done:     done,
	}, nil
}

// TODO: concurrent-safe
func (self *Migrator) Assert(ctx context.Context, schemaVersion int) error {
	self.done = make(chan struct{}, 1)

	if ctxDeadline, ok := ctx.Deadline(); ok {
		self.migrator.LockTimeout = time.Until(ctxDeadline)
	}

	err := Utils.Deadline(ctx, func(exceeded <-chan struct{}) error {
		err := func() error {
			currentSchemaVersion, bad, err := self.migrator.Version() // nolint
			if err != nil && err != migrate.ErrNilVersion {
				return ErrMigratorGeneric().WrapAs(err)
			}

			if bad {
				return ErrMigratorGeneric().Withf("current schema version %d is dirty", currentSchemaVersion)
			}

			if currentSchemaVersion > uint(schemaVersion) {
				return ErrMigratorGeneric().Withf("desired schema version %d behind from current one %d",
					schemaVersion, currentSchemaVersion)
			} else if currentSchemaVersion < uint(schemaVersion) {
				return ErrMigratorGeneric().Withf("desired schema version %d ahead of current one %d",
					schemaVersion, currentSchemaVersion)
			}

			self.observer.Infof(ctx, "Desired schema version %d asserted", schemaVersion)

			return nil
		}()

		select {
		case <-self.done:
		default:
			close(self.done)
		}

		return err
	})

	self.migrator.LockTimeout = migrate.DefaultLockTimeout

	switch {
	case err == nil:
		return nil
	case ErrDeadlineExceeded().Is(err):
		return ErrMigratorTimedOut()
	default:
		return ErrMigratorGeneric().Wrap(err)
	}
}

// TODO: concurrent-safe
func (self *Migrator) Apply(ctx context.Context, schemaVersion int) error {
	self.done = make(chan struct{}, 1)

	if ctxDeadline, ok := ctx.Deadline(); ok {
		self.migrator.LockTimeout = time.Until(ctxDeadline)
	}

	err := Utils.Deadline(ctx, func(exceeded <-chan struct{}) error {
		err := func() error {
			currentSchemaVersion, bad, err := self.migrator.Version() // nolint
			if err != nil && err != migrate.ErrNilVersion {
				return ErrMigratorGeneric().WrapAs(err)
			}

			if bad {
				return ErrMigratorGeneric().Withf("current schema version %d is dirty", currentSchemaVersion)
			}

			if currentSchemaVersion == uint(schemaVersion) {
				self.observer.Info(ctx, "No migrations to apply")
				return nil
			}

			if currentSchemaVersion > uint(schemaVersion) {
				return ErrMigratorGeneric().Withf("desired schema version %d behind from current one %d",
					schemaVersion, currentSchemaVersion)
			}

			self.observer.Infof(ctx, "%d migrations to be applied", schemaVersion-int(currentSchemaVersion))

			err = self.migrator.Migrate(uint(schemaVersion))
			if err != nil {
				return ErrMigratorGeneric().WrapAs(err)
			}

			self.observer.Info(ctx, "Applied all migrations successfully")

			return nil
		}()

		select {
		case <-self.done:
		default:
			close(self.done)
		}

		return err
	})

	self.migrator.LockTimeout = migrate.DefaultLockTimeout

	switch {
	case err == nil:
		return nil
	case ErrDeadlineExceeded().Is(err):
		return ErrMigratorTimedOut()
	default:
		return ErrMigratorGeneric().Wrap(err)
	}
}

// TODO: concurrent-safe
func (self *Migrator) Rollback(ctx context.Context, schemaVersion int) error {
	self.done = make(chan struct{}, 1)

	if ctxDeadline, ok := ctx.Deadline(); ok {
		self.migrator.LockTimeout = time.Until(ctxDeadline)
	}

	err := Utils.Deadline(ctx, func(exceeded <-chan struct{}) error {
		err := func() error {
			currentSchemaVersion, bad, err := self.migrator.Version() // nolint
			if err != nil {
				return ErrMigratorGeneric().WrapAs(err)
			}

			if bad {
				self.observer.Infof(ctx, "Current schema version %d is dirty, ignoring", currentSchemaVersion)

				err = self.migrator.Force(int(currentSchemaVersion))
				if err != nil {
					return ErrMigratorGeneric().WrapAs(err)
				}
			}

			if currentSchemaVersion == uint(schemaVersion) {
				self.observer.Info(ctx, "No migrations to rollback")
				return nil
			}

			if currentSchemaVersion < uint(schemaVersion) {
				return ErrMigratorGeneric().Withf("desired schema version %d ahead of current one %d",
					schemaVersion, currentSchemaVersion)
			}

			self.observer.Infof(ctx, "%d migrations to be rollbacked", int(currentSchemaVersion)-schemaVersion)

			err = self.migrator.Migrate(uint(schemaVersion))
			if err != nil {
				return ErrMigratorGeneric().WrapAs(err)
			}

			self.observer.Info(ctx, "Rollbacked all migrations successfully")

			return nil
		}()

		select {
		case <-self.done:
		default:
			close(self.done)
		}

		return err
	})

	self.migrator.LockTimeout = migrate.DefaultLockTimeout

	switch {
	case err == nil:
		return nil
	case ErrDeadlineExceeded().Is(err):
		return ErrMigratorTimedOut()
	default:
		return ErrMigratorGeneric().Wrap(err)
	}
}

func (self *Migrator) Close(ctx context.Context) error {
	err := Utils.Deadline(ctx, func(exceeded <-chan struct{}) error {
		self.observer.Info(ctx, "Closing migrator")

		select {
		case self.migrator.GracefulStop <- true:
		default:
		}

		<-self.done

		err, errD := self.migrator.Close()
		if errD != nil && _MIGRATOR_ERR_DB_ALREADY_CLOSED.MatchString(errD.Error()) {
			errD = nil
		}

		err = Utils.CombineErrors(err, errD)
		if err != nil {
			return ErrMigratorGeneric().WrapAs(err)
		}

		self.observer.Info(ctx, "Closed migrator")

		return nil
	})
	switch {
	case err == nil:
		return nil
	case ErrDeadlineExceeded().Is(err):
		return ErrMigratorTimedOut()
	default:
		return ErrMigratorGeneric().Wrap(err)
	}
}

type _migrateLogger struct {
	observer *Observer
}

func _newMigrateLogger(observer *Observer) *_migrateLogger {
	return &_migrateLogger{
		observer: observer,
	}
}

func (self _migrateLogger) Printf(format string, v ...any) {
	self.observer.Infof(context.Background(), strings.TrimSpace(format), v...)
}

func (self _migrateLogger) Verbose() bool {
	return false
}
