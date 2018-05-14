package main

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "github.com/golang-migrate/migrate/source/file"
	_ "github.com/lib/pq"
	"github.com/rubenv/sql-migrate"
)

// SQLCloudProxyBinary is the name of the binary we're looking for
const SQLCloudProxyBinary = "cloud_sql_proxy"

// MigrationsFolder is the name of the local (required) folder that holds the migrations
const MigrationsFolder = "migrations"

// SQLCloudProxyPort is the port we're running the proxy on
const SQLCloudProxyPort = 5800

// Proxy CMD ref
var proxyCMD *exec.Cmd

func main() {
	// Step 1: Check for proxy in path, find executable path
	path, err := checkForProxy()
	pError(err)

	defer func() {
		if r := recover(); r != nil {
			fmt.Println("Recovered in f", r)
		}
	}()

	// Step 2: Check for required credentials file and instance identifier
	creds := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	instanceID := os.Getenv("SQL_INSTANCE_ID")
	dbName := os.Getenv("DB_NAME")
	dbPass := os.Getenv("DB_PASS")
	dbUser := os.Getenv("DB_USER")
	if len(creds) == 0 {
		pError(errors.New("Missing required env, GOOGLE_APPLICATION_CREDENTIALS"))
	}
	if len(instanceID) == 0 {
		pError(errors.New("Missing required env, SQL_INSTANCE_ID"))
	}
	if len(dbName) == 0 {
		pError(errors.New("Missing required env, DB_NAME"))
	}
	if len(dbPass) == 0 {
		pError(errors.New("Missing required env, DB_USER"))
	}
	if len(dbUser) == 0 {
		pError(errors.New("Missing required env, DB_PASS"))
	}

	// Step 3: Load up the proxy with the instance and credentials
	instanceArg := fmt.Sprintf("-instances=%s=tcp:%d", instanceID, SQLCloudProxyPort)
	args := []string{instanceArg}

	// Build out the cmd
	outBuff := new(bytes.Buffer)
	proxyCMD = exec.Command(path, args...)
	proxyCMD.Stderr = os.Stderr
	proxyCMD.Stderr = outBuff
	// outPipe, err := proxyCMD.StdoutPipe()
	pError(err)

	// Add ENVs
	osENVs := os.Environ()
	proxyCMD.Env = osENVs

	// Exec the application
	defer func() {
		ensureProcessKill(proxyCMD)
	}()
	trapKillForCleanup()

	// Start the process
	if err := proxyCMD.Start(); err != nil {
		fmt.Println("Err1: ", err)
		pError(err)
	}

	// Dispatch GoRoutine for executing the process
	waitCh := make(chan error, 1)
	go func(cmd *exec.Cmd) {
		err := proxyCMD.Wait()
		pError(fmt.Errorf("Could not start cloud SQL Proxy with error: %+v", err))
		waitCh <- err
	}(proxyCMD)

	// Timeout the waiting of the proxy to get up
	proxyIsUp := new(bool)
	go func() {
		time.Sleep(10 * time.Second)
		if !*proxyIsUp {
			proxyCMD.Process.Kill()
			pError(errors.New("Proxy setup timed out"))
		}
	}()

	// Scan the output to listen for a successful connection
	for {
		bytez, err := outBuff.ReadBytes('\n')
		if err != nil && err != io.EOF {
			pError(err)
		}

		if len(bytez) > 0 {
			if strings.Contains(string(bytez), "Ready for new connections") {
				*proxyIsUp = true
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Ensure migrations folder
	path, err = os.Getwd()
	if _, err := os.Stat(MigrationsFolder); err != nil {
		pError(errors.New("Migrations folder missing"))
	}

	// Proxy is setup, let's attempt the migrations
	pgURL := fmt.Sprintf("postgres://%s:%s@localhost:%d/%s?sslmode=disable", dbUser, dbPass, SQLCloudProxyPort, dbName)
	db, err := sql.Open("postgres", pgURL)
	pError(err)

	// Build driver
	migrations := &migrate.FileMigrationSource{
		Dir: MigrationsFolder,
	}
	n, err := migrate.Exec(db, "postgres", migrations, migrate.Up)
	pError(err)
	fmt.Printf("Applied %d migrations!\n", n)
}

func checkForProxy() (string, error) {
	// Check for the binary in the same folder
	files, err := ioutil.ReadDir("./")
	if err != nil {
		return "", err
	}

	// Try to find the binary locally
	for _, f := range files {
		if f.Name() == SQLCloudProxyBinary {
			localPath := fmt.Sprintf("./%s", SQLCloudProxyBinary)
			return localPath, nil
		}
	}

	// Fall back to searching PATH
	binary, lookErr := exec.LookPath(SQLCloudProxyBinary)
	if lookErr != nil {
		return "", errors.New("Invalid binary. Not in path")
	}
	return binary, nil
}

func currentFilePath() string {
	ex, err := os.Executable()
	pError(err)
	return filepath.Dir(ex)
}

func trapKillForCleanup() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	signal.Notify(c, os.Kill)
	go func() {
		for range c {
			if proxyCMD != nil && proxyCMD.Process != nil {
				proxyCMD.Process.Kill()
			}
		}
	}()
}

func pError(err error) {
	if err != nil {
		fmt.Printf("Exiting with error: %+v\n", err)
		ensureProcessKill(proxyCMD)
		log.Fatal(err)
	}
}

// Ensuring we're killing our child process
func ensureProcessKill(cmdProcess *exec.Cmd) error {
	if cmdProcess != nil {
		// Try the normal way
		cmdProcess.Process.Kill()

		// Sometimes go doesn't kill the process. Lets send a sig 9
		pgid, err := syscall.Getpgid(cmdProcess.Process.Pid)
		if err == nil {
			syscall.Kill(-pgid, 9)
		}
	}
	return nil
}
