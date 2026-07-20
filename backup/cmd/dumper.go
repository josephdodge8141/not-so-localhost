package main

import (
	"context"
	"io"
)

type DBInfo struct {
	Type             string
	Name             string
	User             string
	Password         string
	Host             string
	Port             int
	DBUser           string
	ConnectionString string
}

type Dumper interface {
	Type() string
	Dump(ctx context.Context, info DBInfo, w io.Writer) error
	Restore(ctx context.Context, info DBInfo, r io.Reader, adminPassword string) error
}

type DumperRegistry struct {
	entries map[string]Dumper
}

func NewDumperRegistry() *DumperRegistry {
	return &DumperRegistry{entries: make(map[string]Dumper)}
}

func (r *DumperRegistry) Register(d Dumper) {
	r.entries[d.Type()] = d
}

func (r *DumperRegistry) Get(dbType string) Dumper {
	return r.entries[dbType]
}
