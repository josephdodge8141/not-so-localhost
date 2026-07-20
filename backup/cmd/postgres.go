package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
)

type PostgresDumper struct {
	AdminUser string
}

func (d *PostgresDumper) Type() string { return "postgres" }

func (d *PostgresDumper) Dump(ctx context.Context, info DBInfo, w io.Writer) error {
	var cmd *exec.Cmd
	if info.ConnectionString != "" {
		cmd = exec.CommandContext(ctx, "pg_dump", "-d", info.ConnectionString)
	} else {
		args := []string{
			"-U", info.User,
			"-d", info.Name,
			"-h", info.Host,
			"-p", strconv.Itoa(info.Port),
		}
		cmd = exec.CommandContext(ctx, "pg_dump", args...)
		cmd.Env = append(os.Environ(), "PGPASSWORD="+info.Password)
	}
	cmd.Stdout = w
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (d *PostgresDumper) Restore(ctx context.Context, info DBInfo, r io.Reader, adminPassword string) error {
	port := strconv.Itoa(info.Port)

	owner := info.DBUser
	if owner == "" {
		owner = info.User
	}

	dropCreate := exec.CommandContext(ctx, "psql",
		"-U", d.AdminUser,
		"-h", info.Host,
		"-p", port,
		"-c", `DROP DATABASE IF EXISTS "`+info.Name+`"`,
		"-c", `CREATE DATABASE "`+info.Name+`" OWNER "`+owner+`"`,
	)
	dropCreate.Env = append(os.Environ(), "PGPASSWORD="+adminPassword)
	dropCreate.Stderr = os.Stderr
	if out, err := dropCreate.Output(); err != nil {
		return fmt.Errorf("drop/create: %w\n%s", err, string(out))
	}

	restore := exec.CommandContext(ctx, "psql",
		"-U", owner,
		"-d", info.Name,
		"-h", info.Host,
		"-p", port,
	)
	restore.Env = append(os.Environ(), "PGPASSWORD="+info.Password)
	restore.Stdin = r
	restore.Stderr = os.Stderr
	if out, err := restore.Output(); err != nil {
		return fmt.Errorf("restore: %w\n%s", err, string(out))
	}

	return nil
}
