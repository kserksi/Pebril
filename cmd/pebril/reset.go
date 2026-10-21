package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/kserksi/Pebril/internal/auth"
	"github.com/kserksi/Pebril/internal/config"
	"github.com/kserksi/Pebril/internal/store"
	"golang.org/x/sys/unix"
)

func resetAdministrator(deploy config.Deployment) error {
	if os.Geteuid() == 0 || os.Getuid() != os.Geteuid() {
		return errors.New("Run the reset as the ordinary service user")
	}
	if deploy.DatabaseBackend != "mysql" {
		if err := existingResetDatabase(deploy.DBDir); err != nil {
			return err
		}
	}
	database, err := store.OpenExistingDeployment(deploy)
	if err != nil {
		return err
	}
	defer database.Close()
	authentication, err := auth.New(database)
	if err != nil {
		return err
	}
	initialized, err := authentication.Initialized()
	if err != nil {
		return err
	}
	if !initialized {
		return errors.New("The administrator has not been initialized")
	}
	password, err := resetPassword(os.Stdin, os.Stderr)
	if err != nil {
		return err
	}
	defer clear(password)
	if _, err = authentication.ResetAdministrator(string(password)); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Administrator password reset; existing sessions and upload tokens revoked.")
	return nil
}

func existingResetDatabase(dir string) error {
	for _, name := range []string{"", "pebril.db", "pebril.lock", "pebril.db-wal", "pebril.db-shm", "pebril.db-journal"} {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) && name != "" && name != "pebril.db" {
			continue
		}
		if err != nil {
			return errors.New("Reset requires an existing database owned by the service user")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 || (name == "" && !info.IsDir()) || (name != "" && (!info.Mode().IsRegular() || stat.Nlink != 1)) {
			return errors.New("Database paths must be owned by the service user, without links or permissions allowing other users to write")
		}
	}
	// Reject blank/uninitialized files before store.Open can migrate a new database.
	db, err := sql.Open("sqlite3", (&url.URL{Scheme: "file", Path: filepath.Join(dir, "pebril.db"), RawQuery: "mode=ro"}).String())
	if err != nil {
		return errors.New("Cannot read the existing administrator database")
	}
	defer db.Close()
	var count int
	if err = db.QueryRow("SELECT count(*) FROM users WHERE admin=1").Scan(&count); err != nil || count != 1 {
		return errors.New("Reset requires an initialized administrator database")
	}
	return nil
}

func resetPassword(input, output *os.File) (password []byte, err error) {
	fd := int(input.Fd())
	previous, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, errors.New("Password reset requires terminal input; pipes and redirected input are refused")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer stop()
	hidden := *previous
	hidden.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON
	hidden.Cc[unix.VMIN], hidden.Cc[unix.VTIME] = 1, 0
	if err = unix.IoctlSetTermios(fd, unix.TCSETSF, &hidden); err != nil {
		return nil, errors.New("Cannot disable terminal echo")
	}
	defer func() {
		if restoreErr := unix.IoctlSetTermios(fd, unix.TCSETSF, previous); restoreErr != nil {
			clear(password)
			password = nil
			err = errors.New("Cannot restore terminal settings; password unchanged")
		}
		fmt.Fprintln(output)
	}()
	fmt.Fprint(output, "New administrator password: ")
	password, err = readResetPassword(ctx, fd)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			clear(password)
			password = nil
		}
	}()
	fmt.Fprint(output, "\nConfirm new password: ")
	confirmation, err := readResetPassword(ctx, fd)
	defer clear(confirmation)
	if err != nil {
		return password, err
	}
	if !bytes.Equal(password, confirmation) {
		return password, errors.New("Passwords do not match; password unchanged")
	}
	if err = ctx.Err(); err != nil {
		return password, errors.New("Password reset interrupted; password unchanged")
	}
	return password, nil
}

func readResetPassword(ctx context.Context, fd int) (password []byte, err error) {
	password = make([]byte, 0, 72)
	defer func() {
		if err != nil {
			clear(password)
			password = nil
		}
	}()
	var value [1]byte
	defer clear(value[:])
	for {
		if ctx.Err() != nil {
			return password, errors.New("Password reset interrupted; password unchanged")
		}
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		if _, err = unix.Poll(poll, 100); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return password, errors.New("Cannot read password from terminal")
		}
		if poll[0].Revents&unix.POLLIN == 0 {
			if poll[0].Revents != 0 {
				return password, errors.New("Password terminal closed; password unchanged")
			}
			continue
		}
		n, readErr := unix.Read(fd, value[:])
		if readErr != nil || n != 1 {
			return password, errors.New("Cannot read password from terminal")
		}
		switch value[0] {
		case '\n', '\r':
			if len(password) == 0 {
				return password, errors.New("Password must contain 1 to 72 bytes")
			}
			return password, nil
		case 4:
			return password, errors.New("Password reset cancelled; password unchanged")
		case 8, 127:
			if len(password) > 0 {
				password[len(password)-1] = 0
				password = password[:len(password)-1]
			}
		default:
			if len(password) == 72 {
				return password, errors.New("Password must contain 1 to 72 bytes")
			}
			password = append(password, value[0])
		}
	}
}

func recoverDatabase(deploy config.Deployment) error {
	if os.Getuid() != os.Geteuid() {
		return errors.New("Run recovery as the ordinary service user")
	}
	return store.RecoverDatabase(deploy, func() error {
		if _, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), unix.TCGETS); err != nil {
			return errors.New("Database-owner recovery requires a terminal")
		}
		fmt.Fprintln(os.Stderr, "Stop every Pebril instance using this database, including processes on other hosts. Recovery does not stop them for you.")
		fmt.Fprint(os.Stderr, "Type STOPPED to clear the stale database owner: ")
		text, err := bufio.NewReader(io.LimitReader(os.Stdin, 128)).ReadString('\n')
		if err != nil || strings.TrimSpace(text) != "STOPPED" {
			return errors.New("Database-owner recovery cancelled")
		}
		return nil
	})
}
