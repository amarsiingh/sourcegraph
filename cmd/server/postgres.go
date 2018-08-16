package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
)

// make the "en_US.UTF-8" locale so postgres will be utf-8 enabled by default
// alpine doesn't require explicit locale-file generation

//docker:env LANG=en_US.utf8

// We run 9.4 in production, but if we are embedding might as well get
// something modern 9.6. We add the version specifier to prevent accidentally
// upgrading to an even newer version.
// NOTE: We have to stay at 9.6, otherwise existing users databases won't run
// due to needing to be upgraded. There is no nice auto-upgrade we have here
// without some engineering investment.

//docker:install 'postgresql<9.7' 'postgresql-contrib<9.7' su-exec

func maybePostgresProcFile() (string, error) {
	// PG is already configured
	if os.Getenv("PGHOST") != "" || os.Getenv("PGDATASOURCE") != "" {
		return "", nil
	}

	// Postgres needs to be able to write to run
	var output bytes.Buffer
	e := execer{Out: &output}
	e.Command("mkdir", "-p", "/run/postgresql")
	e.Command("chown", "-R", "postgres", "/run/postgresql")
	if err := e.Error(); err != nil {
		log.Printf("Setting up postgres failed:\n%s", output.String())
		return "", err
	}

	// postgres wants its config in the data dir
	path := filepath.Join(os.Getenv("DATA_DIR"), "postgresql")
	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			return "", err
		}

		if verbose {
			log.Printf("Setting up PostgreSQL at %s", path)
		}
		log.Println("✱ Sourcegraph is initializing the internal database... (may take 15-20 seconds)")

		var output bytes.Buffer
		e := execer{Out: &output}
		e.Command("mkdir", "-p", path)
		e.Command("chown", "postgres", path)
		// initdb --nosync saves ~3-15s on macOS during initial startup. By the time actual data lives in the
		// DB, the OS should have had time to fsync.
		e.Command("su-exec", "postgres", "initdb", "-D", path, "--nosync")
		e.Command("su-exec", "postgres", "pg_ctl", "-D", path, "-o -c listen_addresses=127.0.0.1", "-l", "/tmp/pgsql.log", "-w", "start")
		e.Command("su-exec", "postgres", "createdb", "sourcegraph")
		e.Command("su-exec", "postgres", "pg_ctl", "-D", path, "-m", "fast", "-l", "/tmp/pgsql.log", "-w", "stop")
		if err := e.Error(); err != nil {
			log.Printf("Setting up postgres failed:\n%s", output.String())
			os.RemoveAll(path)
			return "", err
		}
	} else {
		// Between restarts the owner of the volume may have changed. Ensure
		// postgres can still read it.
		var output bytes.Buffer
		e := execer{Out: &output}
		e.Command("chown", "-R", "postgres", path)
		if err := e.Error(); err != nil {
			log.Printf("Adjusting fs owners for postgres failed:\n%s", output.String())
			return "", err
		}
	}

	// Set PGHOST to default to 127.0.0.1, NOT localhost, as localhost does not correctly resolve in some environments
	// (see https://github.com/sourcegraph/issues/issues/34 and https://github.com/sourcegraph/sourcegraph/issues/9129).
	setDefaultEnv("PGHOST", "127.0.0.1")
	setDefaultEnv("PGUSER", "postgres")
	setDefaultEnv("PGDATABASE", "sourcegraph")
	setDefaultEnv("PGSSLMODE", "disable")

	return "postgres: su-exec postgres sh -c 'postgres -c listen_addresses=127.0.0.1 -D " + path + "' 2>&1 | grep -v 'database system was shut down' | grep -v 'MultiXact member wraparound' | grep -v 'database system is ready' | grep -v 'autovacuum launcher started' | grep -v 'the database system is starting up'", nil
}